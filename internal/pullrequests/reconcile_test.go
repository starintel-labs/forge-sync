package pullrequests_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/pullrequests"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type fakePullRequests struct {
	items   []model.PullRequest
	created []model.PullRequest
	updated []model.PullRequest
}

func (f *fakePullRequests) ListPullRequests(context.Context, string, string) ([]model.PullRequest, error) {
	return append([]model.PullRequest(nil), f.items...), nil
}

func (f *fakePullRequests) CreatePullRequest(_ context.Context, _, _ string, item model.PullRequest) (model.PullRequest, error) {
	f.created = append(f.created, item)
	item.ID = 1000 + int64(len(f.created))
	item.Index = 2000 + int64(len(f.created))
	return item, nil
}

func (f *fakePullRequests) UpdatePullRequest(_ context.Context, _, _ string, index int64, item model.PullRequest) (model.PullRequest, error) {
	f.updated = append(f.updated, item)
	item.Index = index
	return item, nil
}

func TestGitHubPullRequestMetadataCopiesToForgejoWithoutNumberAssumption(t *testing.T) {
	t.Parallel()
	store, repository := prState(t)
	base := model.PullRequest{ID: 11, Index: 3, Title: "old", State: "open", Head: "feature/x", Base: "main"}
	if err := store.UpsertPullRequestMapping(context.Background(), model.PullRequestMapping{
		RepositoryGitHubID: 1, GitHubID: 11, ForgejoID: 22, GitHubIndex: 3, ForgejoIndex: 9,
		LastStateHash: pullrequests.Hash(base), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	githubItem := base
	githubItem.Title = "new"
	forgejoItem := base
	forgejoItem.ID, forgejoItem.Index = 22, 9
	github := &fakePullRequests{items: []model.PullRequest{githubItem}}
	forgejo := &fakePullRequests{items: []model.PullRequest{forgejoItem}}

	if err := pullrequests.New(github, forgejo, store).Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.updated) != 1 || forgejo.updated[0].Title != "new" || len(github.updated) != 0 {
		t.Fatalf("github=%#v forgejo=%#v", github.updated, forgejo.updated)
	}
}

func TestConcurrentPullRequestMetadataCreatesConflict(t *testing.T) {
	t.Parallel()
	store, repository := prState(t)
	base := model.PullRequest{ID: 11, Index: 3, Title: "old", State: "open", Head: "feature/x", Base: "main"}
	if err := store.UpsertPullRequestMapping(context.Background(), model.PullRequestMapping{
		RepositoryGitHubID: 1, GitHubID: 11, ForgejoID: 22, GitHubIndex: 3, ForgejoIndex: 9,
		LastStateHash: pullrequests.Hash(base), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	githubItem := base
	githubItem.Title = "github"
	forgejoItem := base
	forgejoItem.ID, forgejoItem.Index, forgejoItem.Title = 22, 9, "forgejo"
	github := &fakePullRequests{items: []model.PullRequest{githubItem}}
	forgejo := &fakePullRequests{items: []model.PullRequest{forgejoItem}}

	if err := pullrequests.New(github, forgejo, store).Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(github.updated)+len(forgejo.updated) != 0 {
		t.Fatal("conflicting pull request was overwritten")
	}
	conflicts, err := store.ListConflicts(context.Background())
	if err != nil || len(conflicts) != 1 || conflicts[0].Kind != "pull-request" {
		t.Fatalf("conflicts=%#v err=%v", conflicts, err)
	}
}

func prState(t *testing.T) (*state.Store, model.RepositoryMapping) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := model.RepositoryMapping{
		GitHubID: 1, GitHubFullName: "starintel-labs/example", ForgejoOwner: "starintel-labs", ForgejoName: "example",
		Visibility: model.VisibilityPrivate, LastStateHash: "repository", UpdatedAt: time.Now().UTC(),
	}
	if err := store.UpsertRepository(context.Background(), repository); err != nil {
		t.Fatal(err)
	}
	return store, repository
}
