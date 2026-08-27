package comments_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/comments"
	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type fakeComments struct {
	comments map[int64][]model.Comment
	created  []model.Comment
	updated  []model.Comment
}

func (f *fakeComments) ListComments(_ context.Context, _, _ string, issueIndex int64) ([]model.Comment, error) {
	return append([]model.Comment(nil), f.comments[issueIndex]...), nil
}

func (f *fakeComments) CreateComment(_ context.Context, _, _ string, issueIndex int64, source model.Comment) (model.Comment, error) {
	f.created = append(f.created, source)
	source.ID = 1000 + int64(len(f.created))
	source.IssueID = issueIndex
	return source, nil
}

func (f *fakeComments) UpdateComment(_ context.Context, _, _ string, commentID int64, source model.Comment) (model.Comment, error) {
	f.updated = append(f.updated, source)
	source.ID = commentID
	return source, nil
}

func TestNewGitHubCommentCopiesOnceAndSuppressesLoop(t *testing.T) {
	t.Parallel()
	store, repository, issueMapping := commentState(t)
	github := &fakeComments{comments: map[int64][]model.Comment{issueMapping.GitHubIndex: {{ID: 11, IssueID: 101, Body: "hello"}}}}
	forgejo := &fakeComments{comments: map[int64][]model.Comment{issueMapping.ForgejoIndex: {}}}
	reconciler := comments.New(github, forgejo, store)

	if err := reconciler.Reconcile(context.Background(), repository, []model.IssueMapping{issueMapping}, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.created) != 1 {
		t.Fatalf("Forgejo created=%#v", forgejo.created)
	}
	created := forgejo.created[0]
	created.ID = 1001
	forgejo.comments[issueMapping.ForgejoIndex] = []model.Comment{created}
	if err := reconciler.Reconcile(context.Background(), repository, []model.IssueMapping{issueMapping}, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.created) != 1 || len(github.created) != 0 || len(github.updated)+len(forgejo.updated) != 0 {
		t.Fatal("synchronized comment bounced on the second reconciliation")
	}
}

func TestConcurrentCommentChangesConflict(t *testing.T) {
	t.Parallel()
	store, repository, issueMapping := commentState(t)
	base := model.Comment{ID: 11, IssueID: 101, Body: "base"}
	if err := store.UpsertCommentMapping(context.Background(), model.CommentMapping{
		RepositoryGitHubID: 1, IssueGitHubID: 101, GitHubID: 11, ForgejoID: 22, LastStateHash: comments.Hash(base), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	github := &fakeComments{comments: map[int64][]model.Comment{issueMapping.GitHubIndex: {{ID: 11, IssueID: 101, Body: "github"}}}}
	forgejo := &fakeComments{comments: map[int64][]model.Comment{issueMapping.ForgejoIndex: {{ID: 22, IssueID: 202, Body: "forgejo"}}}}

	if err := comments.New(github, forgejo, store).Reconcile(context.Background(), repository, []model.IssueMapping{issueMapping}, false); err != nil {
		t.Fatal(err)
	}
	if len(github.updated)+len(forgejo.updated) != 0 {
		t.Fatal("conflicting comment was overwritten")
	}
	conflicts, err := store.ListConflicts(context.Background())
	if err != nil || len(conflicts) != 1 || conflicts[0].Kind != "comment" {
		t.Fatalf("conflicts=%#v err=%v", conflicts, err)
	}
}

func commentState(t *testing.T) (*state.Store, model.RepositoryMapping, model.IssueMapping) {
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
	issueMapping := model.IssueMapping{
		RepositoryGitHubID: 1, GitHubID: 101, ForgejoID: 202, GitHubIndex: 3, ForgejoIndex: 9,
		LastStateHash: "issue", UpdatedAt: time.Now().UTC(),
	}
	if err := store.UpsertIssueMapping(context.Background(), issueMapping); err != nil {
		t.Fatal(err)
	}
	return store, repository, issueMapping
}
