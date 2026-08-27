package gitrefs_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/gitrefs"
	"github.com/starintel-labs/forge-sync/internal/state"
)

func TestSynchronizerFastForwardsCanonicalBranch(t *testing.T) {
	t.Parallel()
	fixture := newGitFixture(t)
	fixture.commit(t, "A")
	fixture.push(t, fixture.github, "main")
	fixture.push(t, fixture.forgejo, "main")
	fixture.commit(t, "B")
	fixture.push(t, fixture.github, "main")

	result, err := fixture.synchronizer(t).Sync(context.Background(), "starintel-labs/example", gitrefs.Remote{URL: fixture.github}, gitrefs.Remote{URL: fixture.forgejo}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Actions) != 1 || len(result.Conflicts) != 0 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if got, want := fixture.ref(t, fixture.forgejo, "refs/heads/main"), fixture.ref(t, fixture.github, "refs/heads/main"); got != want {
		t.Fatalf("Forgejo main = %s, GitHub main = %s", got, want)
	}
}

func TestSynchronizerRecordsDivergenceWithoutPush(t *testing.T) {
	t.Parallel()
	fixture := newGitFixture(t)
	fixture.commit(t, "A")
	fixture.push(t, fixture.github, "main")
	fixture.push(t, fixture.forgejo, "main")
	fixture.commit(t, "B")
	fixture.push(t, fixture.github, "main")
	fixture.run(t, "reset", "--hard", "HEAD~1")
	fixture.commit(t, "C")
	fixture.push(t, fixture.forgejo, "main")
	forgejoBefore := fixture.ref(t, fixture.forgejo, "refs/heads/main")

	result, err := fixture.synchronizer(t).Sync(context.Background(), "starintel-labs/example", gitrefs.Remote{URL: fixture.github}, gitrefs.Remote{URL: fixture.forgejo}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Actions) != 0 || len(result.Conflicts) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if got := fixture.ref(t, fixture.forgejo, "refs/heads/main"); got != forgejoBefore {
		t.Fatalf("divergent Forgejo main changed from %s to %s", forgejoBefore, got)
	}
}

func TestSynchronizerPromotesForgejoDevelopmentBranch(t *testing.T) {
	t.Parallel()
	fixture := newGitFixture(t)
	fixture.commit(t, "A")
	fixture.push(t, fixture.github, "main")
	fixture.push(t, fixture.forgejo, "main")
	fixture.run(t, "checkout", "-b", "feature/foo")
	fixture.commit(t, "feature")
	fixture.push(t, fixture.forgejo, "feature/foo")

	result, err := fixture.synchronizer(t).Sync(context.Background(), "starintel-labs/example", gitrefs.Remote{URL: fixture.github}, gitrefs.Remote{URL: fixture.forgejo}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Actions) != 1 || result.Actions[0].To != gitrefs.GitHub {
		t.Fatalf("unexpected result: %#v", result)
	}
	if got := fixture.ref(t, fixture.github, "refs/heads/feature/foo"); got == "" {
		t.Fatal("feature/foo did not appear on GitHub")
	}
}

func TestSynchronizerDryRunNeverPushes(t *testing.T) {
	t.Parallel()
	fixture := newGitFixture(t)
	fixture.commit(t, "A")
	fixture.push(t, fixture.github, "main")
	before := fixture.ref(t, fixture.forgejo, "refs/heads/main")
	result, err := fixture.synchronizer(t).Sync(context.Background(), "starintel-labs/example", gitrefs.Remote{URL: fixture.github}, gitrefs.Remote{URL: fixture.forgejo}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Actions) != 1 {
		t.Fatalf("dry-run did not report planned action: %#v", result)
	}
	if got := fixture.ref(t, fixture.forgejo, "refs/heads/main"); got != before {
		t.Fatalf("dry-run changed Forgejo ref from %q to %q", before, got)
	}
}

type gitFixture struct {
	root, work, github, forgejo string
}

func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	root := t.TempDir()
	fixture := gitFixture{root: root, work: filepath.Join(root, "work"), github: filepath.Join(root, "github.git"), forgejo: filepath.Join(root, "forgejo.git")}
	runGit(t, root, "init", "--bare", fixture.github)
	runGit(t, root, "init", "--bare", fixture.forgejo)
	runGit(t, root, "init", "-b", "main", fixture.work)
	runGit(t, fixture.work, "config", "user.email", "test@example.invalid")
	runGit(t, fixture.work, "config", "user.name", "forge-sync test")
	return fixture
}

func (f gitFixture) commit(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, "state"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.run(t, "add", "state")
	f.run(t, "commit", "-m", content)
}

func (f gitFixture) push(t *testing.T, remote, branch string) {
	t.Helper()
	f.run(t, "push", remote, "HEAD:refs/heads/"+branch)
}

func (f gitFixture) ref(t *testing.T, remote, ref string) string {
	t.Helper()
	output := runGit(t, f.root, "--git-dir", remote, "rev-parse", "--verify", ref)
	if strings.Contains(output, "fatal:") {
		return ""
	}
	return strings.TrimSpace(output)
}

func (f gitFixture) run(t *testing.T, args ...string) string {
	t.Helper()
	return runGit(t, f.work, args...)
}

func (f gitFixture) synchronizer(t *testing.T) *gitrefs.Synchronizer {
	t.Helper()
	store, err := state.Open(filepath.Join(f.root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return gitrefs.NewSynchronizer(store, 10*time.Second)
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "Needed a single revision") || strings.Contains(string(output), "not a valid ref") {
			return ""
		}
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
