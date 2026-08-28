package issues_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/issues"
	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type fakeIssues struct {
	issues   []model.Issue
	created  []model.Issue
	updated  []model.Issue
	comments map[int64][]model.Comment
}

func (f *fakeIssues) ListIssues(context.Context, string, string) ([]model.Issue, error) {
	return append([]model.Issue(nil), f.issues...), nil
}

func (f *fakeIssues) CreateIssue(_ context.Context, _, _ string, issue model.Issue) (model.Issue, error) {
	f.created = append(f.created, issue)
	issue.ID = 1000 + int64(len(f.created))
	issue.Index = 2000 + int64(len(f.created))
	return issue, nil
}

func (f *fakeIssues) UpdateIssue(_ context.Context, _, _ string, index int64, issue model.Issue) (model.Issue, error) {
	f.updated = append(f.updated, issue)
	issue.Index = index
	return issue, nil
}

func TestExistingMappingDoesNotAssumeEqualIssueNumbers(t *testing.T) {
	t.Parallel()
	store := issueStore(t)
	insertRepository(t, store)
	base := model.Issue{ID: 101, Index: 123, Title: "same", Body: "body", State: "open"}
	lastHash := issues.Hash(base)
	if err := store.UpsertIssueMapping(context.Background(), model.IssueMapping{
		RepositoryGitHubID: 1, GitHubID: 101, ForgejoID: 202, GitHubIndex: 123, ForgejoIndex: 9, LastStateHash: lastHash, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	github := &fakeIssues{issues: []model.Issue{base}}
	forgejoIssue := base
	forgejoIssue.ID = 202
	forgejoIssue.Index = 9
	forgejo := &fakeIssues{issues: []model.Issue{forgejoIssue}}
	reconciler := issues.New(github, forgejo, store)

	if err := reconciler.Reconcile(context.Background(), mapping(), false); err != nil {
		t.Fatal(err)
	}
	if len(github.created)+len(github.updated)+len(forgejo.created)+len(forgejo.updated) != 0 {
		t.Fatal("equal state with unequal issue numbers caused mutation")
	}
}

func TestGitHubIssueChangeCopiesToForgejo(t *testing.T) {
	t.Parallel()
	store := issueStore(t)
	insertRepository(t, store)
	base := model.Issue{ID: 101, Index: 1, Title: "old", State: "open"}
	if err := store.UpsertIssueMapping(context.Background(), model.IssueMapping{
		RepositoryGitHubID: 1, GitHubID: 101, ForgejoID: 202, GitHubIndex: 1, ForgejoIndex: 9, LastStateHash: issues.Hash(base), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	githubIssue := base
	githubIssue.Title = "new from GitHub"
	forgejoIssue := base
	forgejoIssue.ID, forgejoIssue.Index = 202, 9
	github := &fakeIssues{issues: []model.Issue{githubIssue}}
	forgejo := &fakeIssues{issues: []model.Issue{forgejoIssue}}

	if err := issues.New(github, forgejo, store).Reconcile(context.Background(), mapping(), false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.updated) != 1 || forgejo.updated[0].Title != githubIssue.Title || len(github.updated) != 0 {
		t.Fatalf("github updates=%#v forgejo updates=%#v", github.updated, forgejo.updated)
	}
}

func TestForgejoIssueChangeCopiesToGitHub(t *testing.T) {
	t.Parallel()
	store := issueStore(t)
	insertRepository(t, store)
	base := model.Issue{ID: 101, Index: 1, Title: "old", State: "open"}
	if err := store.UpsertIssueMapping(context.Background(), model.IssueMapping{
		RepositoryGitHubID: 1, GitHubID: 101, ForgejoID: 202, GitHubIndex: 1, ForgejoIndex: 9, LastStateHash: issues.Hash(base), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	forgejoIssue := base
	forgejoIssue.ID, forgejoIssue.Index, forgejoIssue.State = 202, 9, "closed"
	github := &fakeIssues{issues: []model.Issue{base}}
	forgejo := &fakeIssues{issues: []model.Issue{forgejoIssue}}

	if err := issues.New(github, forgejo, store).Reconcile(context.Background(), mapping(), false); err != nil {
		t.Fatal(err)
	}
	if len(github.updated) != 1 || github.updated[0].State != "closed" || len(forgejo.updated) != 0 {
		t.Fatalf("github updates=%#v forgejo updates=%#v", github.updated, forgejo.updated)
	}
}

func TestConcurrentIssueChangesCreateConflict(t *testing.T) {
	t.Parallel()
	store := issueStore(t)
	insertRepository(t, store)
	base := model.Issue{ID: 101, Index: 1, Title: "old", State: "open"}
	if err := store.UpsertIssueMapping(context.Background(), model.IssueMapping{
		RepositoryGitHubID: 1, GitHubID: 101, ForgejoID: 202, GitHubIndex: 1, ForgejoIndex: 9, LastStateHash: issues.Hash(base), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	githubIssue := base
	githubIssue.Title = "GitHub edit"
	forgejoIssue := base
	forgejoIssue.ID, forgejoIssue.Index, forgejoIssue.Title = 202, 9, "Forgejo edit"
	github := &fakeIssues{issues: []model.Issue{githubIssue}}
	forgejo := &fakeIssues{issues: []model.Issue{forgejoIssue}}

	if err := issues.New(github, forgejo, store).Reconcile(context.Background(), mapping(), false); err != nil {
		t.Fatal(err)
	}
	if len(github.updated)+len(forgejo.updated) != 0 {
		t.Fatal("conflicting issue was overwritten")
	}
	conflicts, err := store.ListConflicts(context.Background())
	if err != nil || len(conflicts) != 1 || conflicts[0].Kind != "issue" {
		t.Fatalf("conflicts=%#v err=%v", conflicts, err)
	}
}

func TestNewForgejoIssueCreatesMappedGitHubIssue(t *testing.T) {
	t.Parallel()
	store := issueStore(t)
	insertRepository(t, store)
	forgejo := &fakeIssues{issues: []model.Issue{{ID: 77, Index: 4, Title: "local", State: "open"}}}
	github := &fakeIssues{}

	if err := issues.New(github, forgejo, store).Reconcile(context.Background(), mapping(), false); err != nil {
		t.Fatal(err)
	}
	if len(github.created) != 1 || len(forgejo.created) != 0 {
		t.Fatalf("github created=%#v forgejo created=%#v", github.created, forgejo.created)
	}
	mappings, err := store.ListIssueMappings(context.Background(), 1)
	if err != nil || len(mappings) != 1 || mappings[0].ForgejoID != 77 || mappings[0].GitHubIndex == mappings[0].ForgejoIndex {
		t.Fatalf("mappings=%#v err=%v", mappings, err)
	}
}

// A migrated issue whose body gained an attribution footer must still pair by
// index+title instead of being re-created on both sides; the next cycle
// converges Forgejo to the canonical GitHub body.
func TestMigratedIssueWithRewrittenBodyPairsByIndexAndTitle(t *testing.T) {
	t.Parallel()
	store := issueStore(t)
	insertRepository(t, store)
	githubItem := model.Issue{ID: 11, Index: 5, Title: "migrated", Body: "original", State: "open"}
	forgejoItem := model.Issue{ID: 22, Index: 5, Title: "migrated", Body: "original\n\n*by nsaspy, migrated from GitHub*", State: "open"}
	github := &fakeIssues{issues: []model.Issue{githubItem}}
	forgejo := &fakeIssues{issues: []model.Issue{forgejoItem}}

	if err := issues.New(github, forgejo, store).Reconcile(context.Background(), mapping(), false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.created) != 0 || len(github.created) != 0 {
		t.Fatalf("migrated issue duplicated: forgejo=%d github=%d", len(forgejo.created), len(github.created))
	}
	stored, err := store.ListIssueMappings(context.Background(), 1)
	if err != nil || len(stored) != 1 || stored[0].GitHubID != 11 || stored[0].ForgejoID != 22 {
		t.Fatalf("mappings=%#v err=%v", stored, err)
	}
	if stored[0].LastStateHash != issues.Hash(forgejoItem) {
		t.Fatal("pairing baseline must be the Forgejo state")
	}

	forgejoSecond := &fakeIssues{issues: []model.Issue{forgejoItem}}
	githubSecond := &fakeIssues{issues: []model.Issue{githubItem}}
	if err := issues.New(githubSecond, forgejoSecond, store).Reconcile(context.Background(), mapping(), false); err != nil {
		t.Fatal(err)
	}
	if len(forgejoSecond.updated) != 1 || forgejoSecond.updated[0].Body != "original" {
		t.Fatalf("footer was not converged: %#v", forgejoSecond.updated)
	}
}

// A duplicate index/title on one side makes pairing ambiguous: no mapping is
// fabricated (items flow through the ordinary create path instead).
func TestAmbiguousIndexTitlePairingIsSkipped(t *testing.T) {
	t.Parallel()
	store := issueStore(t)
	insertRepository(t, store)
	github := &fakeIssues{issues: []model.Issue{
		{ID: 11, Index: 5, Title: "same", Body: "a", State: "open"},
		{ID: 12, Index: 5, Title: "same", Body: "b", State: "open"},
	}}
	forgejo := &fakeIssues{issues: []model.Issue{
		{ID: 22, Index: 5, Title: "same", Body: "c", State: "open"},
	}}
	if err := issues.New(github, forgejo, store).Reconcile(context.Background(), mapping(), false); err != nil {
		t.Fatal(err)
	}
	stored, err := store.ListIssueMappings(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range stored {
		if (m.GitHubID == 11 || m.GitHubID == 12) && m.ForgejoID == 22 {
			t.Fatalf("ambiguous originals were paired: %#v", m)
		}
	}
	if len(forgejo.created) != 2 || len(github.created) != 1 {
		t.Fatalf("creates=%d/%d", len(forgejo.created), len(github.created))
	}
}

func mapping() model.RepositoryMapping {
	return model.RepositoryMapping{GitHubID: 1, GitHubFullName: "starintel-labs/example", ForgejoOwner: "starintel-labs", ForgejoName: "example"}
}

func issueStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func insertRepository(t *testing.T, store *state.Store) {
	t.Helper()
	m := mapping()
	m.Visibility = model.VisibilityPrivate
	m.LastStateHash = "repository"
	m.UpdatedAt = time.Now().UTC()
	if err := store.UpsertRepository(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}
