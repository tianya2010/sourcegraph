package localstore

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rogpeppe/rog-go/parallel"

	"gopkg.in/gorp.v1"

	"golang.org/x/net/context"
	"sourcegraph.com/sourcegraph/sourcegraph/api/sourcegraph"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/dbutil"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/inventory/filelang"
	"sourcegraph.com/sourcegraph/sourcegraph/services/backend/accesscontrol"
	"sourcegraph.com/sourcegraph/srclib/graph"
	"sourcegraph.com/sourcegraph/srclib/store/pb"
)

func init() {
	// dbGlobalRef DB-maps a GlobalRef object.
	type dbGlobalRef struct {
		DefRepo     string `db:"def_repo"`
		DefUnitType string `db:"def_unit_type"`
		DefUnit     string `db:"def_unit"`
		DefPath     string `db:"def_path"`
		Repo        string
		CommitID    string `db:"commit_id"`
		File        string
		Count       int
		UpdatedAt   *time.Time `db:"updated_at"`
	}
	GraphSchema.Map.AddTableWithName(dbGlobalRef{}, "global_refs").SetKeys(false, "DefRepo", "DefUnitType", "DefUnit", "DefPath", "Repo", "CommitID", "File")
	GraphSchema.CreateSQL = append(GraphSchema.CreateSQL,
		`ALTER TABLE global_refs ALTER COLUMN updated_at TYPE timestamp with time zone USING updated_at::timestamp with time zone;`,
		`CREATE INDEX global_refs_repo ON global_refs(repo text_pattern_ops);`,
		`CREATE INDEX global_refs_def_repo ON global_refs(def_repo text_pattern_ops);`,
		`CREATE INDEX global_refs_commit_id ON global_refs USING btree (commit_id);`,
	)
}

// globalRefs is a DB-backed implementation of the GlobalRefs store.
type globalRefs struct{}

func (g *globalRefs) Get(ctx context.Context, op *sourcegraph.DefsListRefLocationsOp) (*sourcegraph.RefLocationsList, error) {
	opt := op.Opt
	if opt == nil {
		opt = &sourcegraph.DefListRefLocationsOptions{}
	}

	// Optimization: fetch ref stats in parallel to fetching ref locations.
	var totalRepos int64
	statsDone, errc := make(chan bool), make(chan error)
	go func() {
		var err error
		totalRepos, err = g.getRefStats(ctx, op)
		if err != nil {
			errc <- err
		}
		statsDone <- true
	}()

	var args []interface{}
	arg := func(a interface{}) string {
		v := gorp.PostgresDialect{}.BindVar(len(args))
		args = append(args, a)
		return v
	}

	innerSelectSQL := `SELECT repo, file, count FROM global_refs`
	innerSelectSQL += ` WHERE def_repo=` + arg(op.Def.Repo) + ` AND def_unit_type=` + arg(op.Def.UnitType) + ` AND def_unit=` + arg(op.Def.Unit) + ` AND def_path=` + arg(op.Def.Path)
	if len(opt.Repos) > 0 {
		repoBindVars := make([]string, len(opt.Repos))
		for i, r := range opt.Repos {
			repoBindVars[i] = arg(r)
		}
		innerSelectSQL += " AND repo in (" + strings.Join(repoBindVars, ",") + ")"
	}
	innerSelectSQL += fmt.Sprintf(" LIMIT %s OFFSET %s", arg(opt.PerPageOrDefault()), arg(opt.Offset()))

	sql := "SELECT repo, SUM(count) OVER(PARTITION BY repo) AS repo_count, file, count FROM (" + innerSelectSQL + ") res"
	orderBySQL := " ORDER BY repo_count DESC, count DESC"

	sql += orderBySQL

	// dbRefLocationsResult holds the result of the SELECT query for fetching global refs.
	type dbRefLocationsResult struct {
		Repo      string
		RepoCount int `db:"repo_count"`
		File      string
		Count     int
	}
	var dbRefResult []*dbRefLocationsResult
	if _, err := graphDBH(ctx).Select(&dbRefResult, sql, args...); err != nil {
		return nil, err
	}

	// repoRefs holds the ordered list of repos referencing this def. The list is sorted by
	// decreasing ref counts per repo, and the file list in each individual DefRepoRef is
	// also sorted by descending ref counts.
	var repoRefs []*sourcegraph.DefRepoRef
	defRepoIdx := -1
	// refsByRepo groups each referencing file by repo.
	refsByRepo := make(map[string]*sourcegraph.DefRepoRef)
	for _, r := range dbRefResult {
		if _, ok := refsByRepo[r.Repo]; !ok {
			refsByRepo[r.Repo] = &sourcegraph.DefRepoRef{
				Repo:  r.Repo,
				Count: int32(r.RepoCount),
			}
			repoRefs = append(repoRefs, refsByRepo[r.Repo])
			// Note the position of the def's own repo in the slice.
			if op.Def.Repo == r.Repo {
				defRepoIdx = len(repoRefs) - 1
			}
		}
		if r.File != "" && r.Count != 0 {
			refsByRepo[r.Repo].Files = append(refsByRepo[r.Repo].Files, &sourcegraph.DefFileRef{
				Path:  r.File,
				Count: int32(r.Count),
			})
		}
	}

	// Place the def's own repo at the head of the slice, if it exists in the
	// slice and is not at the head already.
	if defRepoIdx > 0 {
		repoRefs[0], repoRefs[defRepoIdx] = repoRefs[defRepoIdx], repoRefs[0]
	}

	// HACK: set hard limit on # of repos returned for one def, to avoid making excessive number
	// of GitHub Repos.Get calls in the accesscontrol check below.
	// TODO: remove this limit once we properly cache GitHub API responses.
	if len(repoRefs) > 100 {
		repoRefs = repoRefs[:100]
	}

	// Filter out repos that the user does not have access to.
	hasAccess := make([]bool, len(repoRefs))
	par := parallel.NewRun(30)
	var mu sync.Mutex
	for i_, r_ := range repoRefs {
		i, r := i_, r_
		par.Do(func() error {
			if err := accesscontrol.VerifyUserHasReadAccess(ctx, "GlobalRefs.Get", r.Repo); err == nil {
				mu.Lock()
				hasAccess[i] = true
				mu.Unlock()
			}
			return nil
		})
	}
	if err := par.Wait(); err != nil {
		return nil, err
	}

	var filteredRepoRefs []*sourcegraph.DefRepoRef
	for i, r := range repoRefs {
		if !hasAccess[i] {
			continue
		}
		filteredRepoRefs = append(filteredRepoRefs, r)
	}

	// Wait for the stats query we kicked off above to finish.
	//
	// TODO(perf): The query in getRefStats above can potentially be slow when
	// the amount of repos referencing the given def is large. To prevent
	// stalling, timeout if the query takes too long and return a default
	// value. The performance of the query above improves after being cached,
	// so it is possible on subsequent runs the timeout won't be triggered.
	timeout := time.After(200 * time.Millisecond)
	select {
	case <-statsDone:
	case <-timeout:
	case err := <-errc:
		return nil, err
	}

	return &sourcegraph.RefLocationsList{
		RepoRefs:   filteredRepoRefs,
		TotalRepos: int32(totalRepos),
	}, nil
}

// getRefStats fetches global ref aggregation stats pagination and display
// purposes.
func (g *globalRefs) getRefStats(ctx context.Context, op *sourcegraph.DefsListRefLocationsOp) (int64, error) {
	var countArgs []interface{}
	countArg := func(a interface{}) string {
		v := gorp.PostgresDialect{}.BindVar(len(countArgs))
		countArgs = append(countArgs, a)
		return v
	}

	q := "SELECT COUNT(DISTINCT repo) AS Repos FROM global_refs WHERE"
	q += " def_repo=" + countArg(op.Def.Repo) + " AND def_unit_type=" + countArg(op.Def.UnitType) + " AND def_unit=" + countArg(op.Def.Unit) + " AND def_path=" + countArg(op.Def.Path)

	return graphDBH(ctx).SelectInt(q, countArgs...)
}

func (g *globalRefs) Update(ctx context.Context, op *pb.ImportOp) error {
	if err := accesscontrol.VerifyUserHasWriteAccess(ctx, "GlobalRefs.Update", op.Repo); err != nil {
		return err
	}

	if op.Data == nil {
		return nil
	}

	tmpCreateSQL := `CREATE TEMPORARY TABLE global_refs_tmp (
	def_repo TEXT,
	def_unit_type TEXT,
	def_unit TEXT,
	def_path TEXT,
	repo TEXT,
	commit_id TEXT,
	file TEXT,
	count integer
)
ON COMMIT DROP;`
	tmpInsertSQL := `INSERT INTO global_refs_tmp(def_repo, def_unit_type, def_unit, def_path, repo, commit_id, file, count) VALUES($1, $2, $3, $4, $5, $6, $7, $8);`
	finalDeleteSQL := `DELETE FROM global_refs WHERE repo=$1 AND commit_id=$2 AND file IN (SELECT file FROM global_refs_tmp GROUP BY file);`
	finalInsertSQL := `INSERT INTO global_refs(def_repo, def_unit_type, def_unit, def_path, repo, commit_id, file, count, updated_at)
	SELECT def_repo, def_unit_type, def_unit, def_path, repo, commit_id, file, sum(count) as count, now() as updated_at
	FROM global_refs_tmp
	GROUP BY def_repo, def_unit_type, def_unit, def_path, repo, commit_id, file;`

	return dbutil.Transact(graphDBH(ctx), func(tx gorp.SqlExecutor) error {
		// Create a temporary table to load all new ref data.
		if _, err := tx.Exec(tmpCreateSQL); err != nil {
			return err
		}

		// Insert refs into temporary table
		var r graph.Ref
		for _, rp := range op.Data.Refs {
			// Ignore broken refs.
			if rp.DefPath == "" {
				continue
			}
			// Ignore def refs.
			if rp.Def {
				continue
			}
			// Ignore vendored refs.
			if filelang.IsVendored(r.File, false) {
				continue
			}
			// Avoid modify pointer
			r = *rp
			if r.DefRepo == "" {
				r.DefRepo = op.Repo
			}
			if r.DefUnit == "" {
				r.DefUnit = op.Unit.Name
			}
			if r.DefUnitType == "" {
				r.DefUnitType = op.Unit.Type
			}
			// Ignore ref to builtin defs of golang/go repo (string, int, bool, etc) as this
			// doesn't add significant value; yet it adds up to a lot of space in the db,
			// and queries for refs of builtin defs take long to finish.
			if r.DefUnitType == "GoPackage" && r.DefRepo == "github.com/golang/go" && r.DefUnit == "builtin" {
				continue
			}
			if _, err := tx.Exec(tmpInsertSQL, r.DefRepo, r.DefUnitType, r.DefUnit, r.DefPath, op.Repo, op.CommitID, r.File, 1); err != nil {
				return err
			}
		}

		// Purge all existing ref data for files in this source unit.
		if _, err := tx.Exec(finalDeleteSQL, op.Repo, op.CommitID); err != nil {
			return err
		}

		// Insert refs into global refs table
		if _, err := tx.Exec(finalInsertSQL); err != nil {
			return err
		}

		return nil
	})
}