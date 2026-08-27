package state_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/state"
)

func TestStoreReopenPreservesRepositoryMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := model.RepositoryMapping{
		GitHubID:       42,
		GitHubFullName: "starintel-labs/alpha",
		ForgejoOwner:   "starintel-labs",
		ForgejoName:    "alpha",
		Visibility:     model.VisibilityPrivate,
		LastStateHash:  "hash-a",
		UpdatedAt:      time.Now().UTC().Truncate(time.Second),
	}
	if err := store.UpsertRepository(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, found, err := store.RepositoryByGitHubID(ctx, want.GitHubID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("repository mapping was not persisted")
	}
	if got.GitHubFullName != want.GitHubFullName || got.ForgejoOwner != want.ForgejoOwner || got.ForgejoName != want.ForgejoName || got.Visibility != want.Visibility || got.LastStateHash != want.LastStateHash {
		t.Fatalf("mapping mismatch: got %#v, want %#v", got, want)
	}
}

func TestClaimWebhookDeliverySuppressesReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	claimed, err := store.ClaimWebhookDelivery(ctx, "github", "delivery-1", "issues", "payload-hash")
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	claimed, err = store.ClaimWebhookDelivery(ctx, "github", "delivery-1", "issues", "payload-hash")
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("replayed delivery was claimed twice")
	}
}

func TestConflictInsertionIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	conflict := model.Conflict{
		Kind:           "git-ref",
		Repository:     "lost-rob0t/prolog-rlm",
		ObjectKey:      "refs/heads/main",
		GitHubState:    "aaa",
		ForgejoState:   "bbb",
		LastKnownState: "ccc",
		CreatedAt:      time.Now().UTC(),
	}
	if err := store.AddConflict(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	if err := store.AddConflict(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	conflicts, err := store.ListConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(conflicts))
	}
}
