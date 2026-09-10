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

func TestResolveConflictExcludesRowFromUnresolvedList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first := model.Conflict{
		Kind:           "git-ref-divergence",
		Repository:     "starintel-labs/forge-sync",
		ObjectKey:      "refs/heads/master",
		GitHubState:    "aaa",
		ForgejoState:   "bbb",
		LastKnownState: "ccc",
		CreatedAt:      time.Now().UTC(),
	}
	second := first
	second.ObjectKey = "refs/heads/other"
	if err := store.AddConflict(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.AddConflict(ctx, second); err != nil {
		t.Fatal(err)
	}

	unresolved, err := store.ListConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 2 {
		t.Fatalf("got %d unresolved conflicts, want 2", len(unresolved))
	}
	target := unresolved[0]
	if target.ID == 0 || target.ID == unresolved[1].ID {
		t.Fatalf("conflict IDs are not distinct and non-zero: %d, %d", unresolved[0].ID, unresolved[1].ID)
	}

	resolved, ok, err := store.ResolveConflict(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("resolving an existing unresolved conflict reported false")
	}
	if resolved.ID != target.ID {
		t.Fatalf("resolved row ID = %d, want %d", resolved.ID, target.ID)
	}
	if resolved.ResolvedAt == nil || resolved.ResolvedAt.IsZero() {
		t.Fatalf("resolved row carries no resolved_at: %#v", resolved)
	}
	if resolved.Kind != target.Kind || resolved.ObjectKey != target.ObjectKey {
		t.Fatalf("resolved row mismatch: got %#v, want %#v", resolved, target)
	}

	remaining, err := store.ListConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != unresolved[1].ID {
		t.Fatalf("unresolved list after resolve = %#v, want only row %d", remaining, unresolved[1].ID)
	}

	if _, ok, err := store.ResolveConflict(ctx, target.ID); err != nil || ok {
		t.Fatalf("re-resolving a resolved row: ok=%v err=%v", ok, err)
	}
}

func TestResolveConflictUnknownIDFailsWithoutMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	conflict := model.Conflict{
		Kind:           "release",
		Repository:     "starintel-labs/forge-sync",
		ObjectKey:      "v1.2.3",
		GitHubState:    "aaa",
		ForgejoState:   "bbb",
		LastKnownState: "ccc",
	}
	if err := store.AddConflict(ctx, conflict); err != nil {
		t.Fatal(err)
	}

	resolved, ok, err := store.ResolveConflict(ctx, 424242)
	if err != nil {
		t.Fatal(err)
	}
	if ok || resolved.ID != 0 {
		t.Fatalf("unknown ID resolved: ok=%v row=%#v", ok, resolved)
	}
	unresolved, err := store.ListConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 1 {
		t.Fatalf("failed resolution mutated the store: %d unresolved", len(unresolved))
	}
}

func TestResolvedConflictPersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	conflict := model.Conflict{
		Kind:           "git-ref-divergence",
		Repository:     "starintel-labs/forge-sync",
		ObjectKey:      "refs/heads/master",
		GitHubState:    "aaa",
		ForgejoState:   "bbb",
		LastKnownState: "ccc",
	}
	if err := store.AddConflict(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	conflicts, err := store.ListConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := conflicts[0].ID
	if _, ok, err := store.ResolveConflict(ctx, id); err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	remaining, err := store.ListConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("resolved row reappeared in the unresolved list: %#v", remaining)
	}
	if _, ok, err := store.ResolveConflict(ctx, id); err != nil || ok {
		t.Fatalf("resolved row did not persist across reopen: ok=%v err=%v", ok, err)
	}
}
