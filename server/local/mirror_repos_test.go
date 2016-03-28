package local

import (
	"testing"

	"golang.org/x/net/context"

	"sourcegraph.com/sourcegraph/sourcegraph/go-sourcegraph/sourcegraph"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/vcs"
	vcstest "sourcegraph.com/sourcegraph/sourcegraph/pkg/vcs/testing"
	"sourcegraph.com/sourcegraph/sourcegraph/store"
)

func TestRefreshVCS(t *testing.T) {
	ctx, mock := testContext()
	var updatedEverything bool
	mock.servers.Repos.MockGet(t, "r")
	mock.stores.RepoVCS.MockOpen(t, "r", vcstest.MockRepository{
		Branches_: func(_ vcs.BranchesOptions) ([]*vcs.Branch, error) {
			return []*vcs.Branch{}, nil
		},
		UpdateEverything_: func(_ vcs.RemoteOpts) (*vcs.UpdateResult, error) {
			updatedEverything = true
			return &vcs.UpdateResult{Changes: []vcs.Change{}}, nil
		},
	})

	_, err := MirrorRepos.RefreshVCS(ctx, &sourcegraph.MirrorReposRefreshVCSOp{Repo: sourcegraph.RepoSpec{URI: "r"}})
	if !updatedEverything {
		t.Error("Did not call UpdateEverything")
	}
	if err != nil {
		t.Fatalf("RefreshVCS call failed: %s", err)
	}
}

func TestRefreshVCS_cloneRepo(t *testing.T) {
	ctx, mock := testContext()
	var cloned, built bool
	mock.servers.Repos.MockGet(t, "r")
	mock.servers.Repos.MockGetCommit_ByID_NoCheck(t, "deadbeef")
	mock.stores.RepoVCS.MockOpen(t, "r", vcstest.MockRepository{
		Branches_: func(_ vcs.BranchesOptions) ([]*vcs.Branch, error) {
			return nil, vcs.ErrRepoNotExist
		},
	})
	mock.stores.RepoVCS.Clone_ = func(_ context.Context, _ string, _ *store.CloneInfo) error {
		cloned = true
		return nil
	}
	mock.servers.Builds.Create_ = func(_ context.Context, _ *sourcegraph.BuildsCreateOp) (*sourcegraph.Build, error) {
		built = true
		return &sourcegraph.Build{}, nil
	}

	_, err := MirrorRepos.RefreshVCS(ctx, &sourcegraph.MirrorReposRefreshVCSOp{Repo: sourcegraph.RepoSpec{URI: "r"}})
	if !cloned {
		t.Error("RefreshVCS did not clone missing repo")
	}
	if !built {
		t.Error("RefreshVCS did not build repo")
	}
	if err != nil {
		t.Fatalf("RefreshVCS call failed: %s", err)
	}
}

func TestRefreshVCS_cloneRepoExists(t *testing.T) {
	ctx, mock := testContext()
	var built bool
	mock.servers.Repos.MockGet(t, "r")
	mock.servers.Repos.MockGetCommit_ByID_NoCheck(t, "deadbeef")
	mock.stores.RepoVCS.MockOpen(t, "r", vcstest.MockRepository{
		Branches_: func(_ vcs.BranchesOptions) ([]*vcs.Branch, error) {
			return nil, vcs.ErrRepoNotExist
		},
	})
	mock.stores.RepoVCS.Clone_ = func(_ context.Context, _ string, _ *store.CloneInfo) error {
		return vcs.ErrRepoExist
	}
	mock.servers.Builds.Create_ = func(_ context.Context, _ *sourcegraph.BuildsCreateOp) (*sourcegraph.Build, error) {
		built = true
		return &sourcegraph.Build{}, nil
	}

	_, err := MirrorRepos.RefreshVCS(ctx, &sourcegraph.MirrorReposRefreshVCSOp{Repo: sourcegraph.RepoSpec{URI: "r"}})
	if !built {
		t.Error("RefreshVCS did not build repo")
	}
	if err != nil {
		t.Fatalf("RefreshVCS call failed: %s", err)
	}
}