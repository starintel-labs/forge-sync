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

// A crash between "Forgejo create succeeded" and "mapping persisted" must not
// duplicate the pull request on restart: the identical items pair by hash and
// the mapping is re-established without another create.
func TestRecoveryAfterCrashBetweenCreateAndMapping(t *testing.T) {
	t.Parallel()
	store, repository := prState(t)
	githubItem := model.PullRequest{ID: 11, Index: 3, Title: "recover", State: "open", Head: "feature/x", Base: "main"}
	github := &fakePullRequests{items: []model.PullRequest{githubItem}}
	forgejo := &fakePullRequests{}

	if err := pullrequests.New(github, forgejo, store).Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.created) != 1 {
		t.Fatalf("created=%d want 1", len(forgejo.created))
	}

	// Simulate the lost mapping write: fresh state, Forgejo now lists the
	// pull request created before the crash.
	created := forgejo.created[0]
	created.ID = 1001
	created.Index = 2001
	crashStore, repository := prStateAt(t, filepath.Join(t.TempDir(), "crash.db"))
	forgejoAfterCrash := &fakePullRequests{items: []model.PullRequest{created}}
	if err := pullrequests.New(github, forgejoAfterCrash, crashStore).Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejoAfterCrash.created) != 0 {
		t.Fatalf("recovery created %d duplicates", len(forgejoAfterCrash.created))
	}
	mappings, err := crashStore.ListPullRequestMappings(context.Background(), repository.GitHubID)
	if err != nil || len(mappings) != 1 || mappings[0].GitHubID != 11 || mappings[0].ForgejoID != 1001 {
		t.Fatalf("mappings=%#v err=%v", mappings, err)
	}
}

// Re-running reconciliation over unchanged state performs no writes at all.
func TestReconcileIsIdempotentOnUnchangedState(t *testing.T) {
	t.Parallel()
	store, repository := prState(t)
	githubItem := model.PullRequest{ID: 11, Index: 3, Title: "stable", State: "open", Head: "feature/x", Base: "main"}
	github := &fakePullRequests{items: []model.PullRequest{githubItem}}
	forgejo := &fakePullRequests{}

	first := pullrequests.New(github, forgejo, store)
	if err := first.Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	created := forgejo.created[0]
	created.ID, created.Index = 1001, 2001
	forgejo.items = []model.PullRequest{created}
	forgejo.created, forgejo.updated = nil, nil

	if err := first.Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if err := first.Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.created) != 0 || len(github.created) != 0 || len(forgejo.updated) != 0 || len(github.updated) != 0 {
		t.Fatalf("unchanged state produced writes: created=%d/%d updated=%d/%d",
			len(forgejo.created), len(github.created), len(forgejo.updated), len(github.updated))
	}
}

func prStateAt(t *testing.T, path string) (*state.Store, model.RepositoryMapping) {
	t.Helper()
	store, err := state.Open(path)
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
