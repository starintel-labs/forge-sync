package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/state"
)

// hermeticEnvironment pins every variable the runtime construction reads so
// the CLI tests never depend on ambient forge credentials (they must pass on
// a machine with no FORGE_SYNC_* environment at all).
func hermeticEnvironment(t *testing.T) string {
	t.Helper()
	t.Setenv("FORGE_SYNC_GITHUB_WEBHOOK_SECRET", "test-github-secret")
	t.Setenv("FORGE_SYNC_FORGEJO_WEBHOOK_SECRET", "test-forgejo-secret")
	t.Setenv("FORGE_SYNC_GITHUB_API", "https://github.invalid")
	t.Setenv("FORGE_SYNC_GITHUB_TOKEN", "test-github-token")
	t.Setenv("FORGE_SYNC_FORGEJO_API", "https://forgejo.invalid")
	t.Setenv("FORGE_SYNC_FORGEJO_TOKEN", "test-forgejo-token")
	path := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("FORGE_SYNC_STATE_PATH", path)
	return path
}

func seedConflict(t *testing.T, path string) int64 {
	t.Helper()
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	conflict := model.Conflict{
		Kind:           "git-ref-divergence",
		Repository:     "starintel-labs/forge-sync",
		ObjectKey:      "refs/heads/master",
		GitHubState:    "aaa",
		ForgejoState:   "bbb",
		LastKnownState: "ccc",
	}
	if err := store.AddConflict(context.Background(), conflict); err != nil {
		t.Fatal(err)
	}
	conflicts, err := store.ListConflicts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("seed produced %d conflicts, want 1", len(conflicts))
	}
	return conflicts[0].ID
}

func captureStdout(t *testing.T, arguments []string) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	runErr := run(arguments)
	writer.Close()
	os.Stdout = original
	var buffer bytes.Buffer
	if _, err := io.Copy(&buffer, reader); err != nil {
		t.Fatal(err)
	}
	return buffer.String(), runErr
}

func TestConflictsResolveCommandMarksRowResolved(t *testing.T) {
	path := hermeticEnvironment(t)
	id := seedConflict(t, path)

	output, err := captureStdout(t, []string{"conflicts", "resolve", strconv.FormatInt(id, 10)})
	if err != nil {
		t.Fatal(err)
	}
	var resolved model.Conflict
	if err := json.Unmarshal([]byte(output), &resolved); err != nil {
		t.Fatalf("resolve output is not JSON: %v\n%s", err, output)
	}
	if resolved.ID != id || resolved.ResolvedAt == nil {
		t.Fatalf("resolved output missing audit fields: %#v", resolved)
	}

	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	unresolved, err := store.ListConflicts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("resolved conflict still listed: %#v", unresolved)
	}
}

func TestConflictsResolveCommandRejectsUnknownID(t *testing.T) {
	path := hermeticEnvironment(t)
	seedConflict(t, path)

	if err := run([]string{"conflicts", "resolve", "999999"}); err == nil {
		t.Fatal("resolving an unknown ID must fail")
	}

	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	unresolved, err := store.ListConflicts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 1 {
		t.Fatalf("failed resolution mutated the store: %#v", unresolved)
	}
}

func TestConflictsResolveCommandRejectsMalformedID(t *testing.T) {
	hermeticEnvironment(t)
	for _, arguments := range [][]string{
		{"conflicts", "resolve", "abc"},
		{"conflicts", "resolve"},
		{"conflicts", "resolve", "1", "extra"},
		{"conflicts", "frobnicate", "1"},
	} {
		if err := run(arguments); err == nil {
			t.Fatalf("usage %v must fail", arguments)
		}
	}
}

func TestConflictsListCommandEmitsRowIDs(t *testing.T) {
	path := hermeticEnvironment(t)
	id := seedConflict(t, path)

	output, err := captureStdout(t, []string{"conflicts"})
	if err != nil {
		t.Fatal(err)
	}
	var conflicts []model.Conflict
	if err := json.Unmarshal([]byte(output), &conflicts); err != nil {
		t.Fatalf("conflicts output is not JSON: %v\n%s", err, output)
	}
	if len(conflicts) != 1 || conflicts[0].ID != id {
		t.Fatalf("listing lost the stable row ID: %#v", conflicts)
	}
}
