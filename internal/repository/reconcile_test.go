package repository_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/repository"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type fakeGitHub struct {
	repositories map[string][]model.Repository
	err          error
}

func (f *fakeGitHub) ListRepositories(_ context.Context, namespace string) ([]model.Repository, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]model.Repository(nil), f.repositories[namespace]...), nil
}

type identityChange struct {
	oldOwner, oldName, newOwner, newName string
}

type fakeForgejo struct {
	repositories map[string][]model.Repository
	err          error
	migrated     []model.Repository
	identities   []identityChange
	settings     []model.Repository
}

func (f *fakeForgejo) ListRepositories(_ context.Context, namespace string) ([]model.Repository, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]model.Repository(nil), f.repositories[namespace]...), nil
}

func (f *fakeForgejo) MigrateRepository(_ context.Context, source model.Repository, _ string) (model.Repository, error) {
	f.migrated = append(f.migrated, source)
	return source, nil
}

func (f *fakeForgejo) UpdateRepositoryIdentity(_ context.Context, oldOwner, oldName, newOwner, newName string) error {
	f.identities = append(f.identities, identityChange{oldOwner, oldName, newOwner, newName})
	return nil
}

func (f *fakeForgejo) UpdateRepositorySettings(_ context.Context, repository model.Repository) error {
	f.settings = append(f.settings, repository)
	return nil
}

func TestDryRunInventoriesMissingRepositoryWithoutMutation(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	github := &fakeGitHub{repositories: map[string][]model.Repository{
		"starintel-labs": {{ID: 1, Owner: "starintel-labs", Name: "new", FullName: "starintel-labs/new", Visibility: model.VisibilityPrivate}},
	}}
	forgejo := &fakeForgejo{repositories: map[string][]model.Repository{"starintel-labs": {}}}
	reconciler := repository.New(github, forgejo, store, "github-token", nil)

	inventory, err := reconciler.Discover(context.Background(), []string{"starintel-labs"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.GitHubRepositories != 1 || inventory.ForgejoRepositories != 0 || inventory.Missing != 1 {
		t.Fatalf("unexpected inventory: %#v", inventory)
	}
	if len(forgejo.migrated) != 0 || len(forgejo.settings) != 0 || len(forgejo.identities) != 0 {
		t.Fatal("dry-run mutated Forgejo")
	}
	if mappings, err := store.ListRepositories(context.Background()); err != nil || len(mappings) != 0 {
		t.Fatalf("dry-run persisted mappings: %#v, %v", mappings, err)
	}
}

func TestDiscoverImportsNewRepositoryAndPersistsStableID(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	github := &fakeGitHub{repositories: map[string][]model.Repository{
		"starintel-labs": {{ID: 7, Owner: "starintel-labs", Name: "new", FullName: "starintel-labs/new", Visibility: model.VisibilityPrivate}},
	}}
	forgejo := &fakeForgejo{repositories: map[string][]model.Repository{"starintel-labs": {}}}
	reconciler := repository.New(github, forgejo, store, "github-token", nil)

	if _, err := reconciler.Discover(context.Background(), []string{"starintel-labs"}, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.migrated) != 1 {
		t.Fatalf("got %d migrations, want 1", len(forgejo.migrated))
	}
	mapping, found, err := store.RepositoryByGitHubID(context.Background(), 7)
	if err != nil || !found {
		t.Fatalf("stable ID mapping absent: %#v, %v", mapping, err)
	}
}

func TestStableIDDetectsRenameAndTransfer(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	if err := store.UpsertRepository(context.Background(), model.RepositoryMapping{
		GitHubID: 7, GitHubFullName: "starintel-labs/old", ForgejoOwner: "starintel-labs", ForgejoName: "old",
		Visibility: model.VisibilityPrivate, LastStateHash: "old", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	github := &fakeGitHub{repositories: map[string][]model.Repository{
		"lost-rob0t": {{ID: 7, Owner: "lost-rob0t", Name: "renamed", FullName: "lost-rob0t/renamed", Visibility: model.VisibilityPrivate}},
	}}
	forgejo := &fakeForgejo{repositories: map[string][]model.Repository{
		"lost-rob0t": {},
	}}
	reconciler := repository.New(github, forgejo, store, "github-token", nil)

	if _, err := reconciler.Discover(context.Background(), []string{"lost-rob0t"}, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.migrated) != 0 || len(forgejo.identities) != 1 {
		t.Fatalf("migrations=%d identities=%#v", len(forgejo.migrated), forgejo.identities)
	}
	want := identityChange{"starintel-labs", "old", "lost-rob0t", "renamed"}
	if forgejo.identities[0] != want {
		t.Fatalf("identity change = %#v, want %#v", forgejo.identities[0], want)
	}
}

func TestOwnerMapRedirectsMigrationAndMatching(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	github := &fakeGitHub{repositories: map[string][]model.Repository{
		"starintel-labs": {{ID: 7, Owner: "starintel-labs", Name: "new", FullName: "starintel-labs/new", CloneURL: "https://github.com/starintel-labs/new.git", Visibility: model.VisibilityPrivate}},
	}}
	forgejo := &fakeForgejo{repositories: map[string][]model.Repository{"nsaspy": {}}}
	reconciler := repository.New(github, forgejo, store, "github-token", map[string]string{"starintel-labs": "nsaspy"})

	if _, err := reconciler.Discover(context.Background(), []string{"starintel-labs"}, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.migrated) != 1 || forgejo.migrated[0].Owner != "nsaspy" || forgejo.migrated[0].Name != "new" {
		t.Fatalf("migrated=%#v", forgejo.migrated)
	}
	if forgejo.migrated[0].CloneURL != "https://github.com/starintel-labs/new.git" {
		t.Fatalf("clone URL was rewritten: %#v", forgejo.migrated[0])
	}
	mapping, found, err := store.RepositoryByGitHubID(context.Background(), 7)
	if err != nil || !found || mapping.ForgejoOwner != "nsaspy" || mapping.GitHubFullName != "starintel-labs/new" {
		t.Fatalf("mapping=%#v found=%v err=%v", mapping, found, err)
	}
}

func TestOwnerMapPairsPreexistingRepositoryUnderMappedOwner(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	github := &fakeGitHub{repositories: map[string][]model.Repository{
		"lost-rob0t": {{ID: 9, Owner: "lost-rob0t", Name: "tools", FullName: "lost-rob0t/tools", Visibility: model.VisibilityPrivate}},
	}}
	forgejo := &fakeForgejo{repositories: map[string][]model.Repository{
		"nsaspy": {{ID: 90, Owner: "nsaspy", Name: "tools", FullName: "nsaspy/tools", Visibility: model.VisibilityPrivate}},
	}}
	reconciler := repository.New(github, forgejo, store, "github-token", map[string]string{"lost-rob0t": "nsaspy"})

	inventory, err := reconciler.Discover(context.Background(), []string{"lost-rob0t"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Missing != 0 || inventory.InSync != 1 || len(forgejo.migrated) != 0 {
		t.Fatalf("inventory=%#v migrated=%d", inventory, len(forgejo.migrated))
	}
	mapping, found, err := store.RepositoryByGitHubID(context.Background(), 9)
	if err != nil || !found || mapping.ForgejoOwner != "nsaspy" {
		t.Fatalf("mapping=%#v found=%v err=%v", mapping, found, err)
	}
}

func TestOwnedForksSyncButCollaboratorRepositoriesDoNot(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	github := &fakeGitHub{repositories: map[string][]model.Repository{
		"lost-rob0t": {
			{ID: 21, Owner: "lost-rob0t", Name: "vim-fork", FullName: "lost-rob0t/vim-fork", CloneURL: "https://github.com/lost-rob0t/vim-fork.git", Visibility: model.VisibilityPublic},
			{ID: 22, Owner: "Papurudoragon", Name: "bountyforone", FullName: "Papurudoragon/bountyforone", CloneURL: "https://github.com/Papurudoragon/bountyforone.git", Visibility: model.VisibilityPrivate},
		},
	}}
	forgejo := &fakeForgejo{repositories: map[string][]model.Repository{"nsaspy": {}}}
	reconciler := repository.New(github, forgejo, store, "github-token", map[string]string{"lost-rob0t": "nsaspy"})

	inventory, err := reconciler.Discover(context.Background(), []string{"lost-rob0t"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(forgejo.migrated) != 1 || forgejo.migrated[0].Name != "vim-fork" {
		t.Fatalf("migrated=%#v", forgejo.migrated)
	}
	if inventory.GitHubRepositories != 2 || inventory.Missing != 1 || inventory.InSync != 0 {
		t.Fatalf("inventory=%#v", inventory)
	}
	if _, found, _ := store.RepositoryByGitHubID(context.Background(), 22); found {
		t.Fatal("collaborator repository was mapped")
	}
}

func TestAPIFailureNeverBecomesRepositoryDeletionOrImport(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	github := &fakeGitHub{err: errors.New("rate limited")}
	forgejo := &fakeForgejo{repositories: map[string][]model.Repository{"starintel-labs": {}}}
	reconciler := repository.New(github, forgejo, store, "github-token", nil)
	if _, err := reconciler.Discover(context.Background(), []string{"starintel-labs"}, false); err == nil {
		t.Fatal("API failure was accepted")
	}
	if len(forgejo.migrated) != 0 || len(forgejo.identities) != 0 || len(forgejo.settings) != 0 {
		t.Fatal("API failure caused Forgejo mutation")
	}
}

func openStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
