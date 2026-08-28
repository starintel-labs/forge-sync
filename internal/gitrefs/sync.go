package gitrefs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/state"
)

const maxGitOutput = 1 << 20

type Remote struct {
	URL      string
	Username string
	Token    string
}

type Result struct {
	Actions   []Action
	Conflicts []model.Conflict
}

type Synchronizer struct {
	store   *state.Store
	timeout time.Duration
}

func NewSynchronizer(store *state.Store, timeout time.Duration) *Synchronizer {
	if store == nil || timeout <= 0 {
		panic("git synchronizer requires state and a positive timeout")
	}
	return &Synchronizer{store: store, timeout: timeout}
}

func (s *Synchronizer) Sync(ctx context.Context, repository string, github, forgejo Remote, dryRun bool) (Result, error) {
	if err := github.validate(); err != nil {
		return Result{}, fmt.Errorf("GitHub remote: %w", err)
	}
	if err := forgejo.validate(); err != nil {
		return Result{}, fmt.Errorf("Forgejo remote: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	workspace, err := os.MkdirTemp("", "forge-sync-git-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(workspace)
	if err := os.Chmod(workspace, 0o700); err != nil {
		return Result{}, err
	}
	gitDir := filepath.Join(workspace, "repository.git")
	if _, err := run(ctx, workspace, Remote{}, "init", "--bare", gitDir); err != nil {
		return Result{}, fmt.Errorf("initialize temporary repository: %w", err)
	}
	if err := fetchWithRetry(ctx, gitDir, GitHub, github); err != nil {
		return Result{}, err
	}
	if err := fetchWithRetry(ctx, gitDir, Forgejo, forgejo); err != nil {
		return Result{}, err
	}
	githubRefs, err := localRefs(ctx, gitDir, GitHub)
	if err != nil {
		return Result{}, err
	}
	forgejoRefs, err := localRefs(ctx, gitDir, Forgejo)
	if err != nil {
		return Result{}, err
	}
	ancestry := func(ancestor, descendant string) (bool, error) {
		_, err := run(ctx, gitDir, Remote{}, "--git-dir", gitDir, "merge-base", "--is-ancestor", ancestor, descendant)
		if err == nil {
			return true, nil
		}
		var commandErr *commandError
		if errors.As(err, &commandErr) && commandErr.exitCode == 1 {
			return false, nil
		}
		return false, err
	}
	actions, conflicts, err := Plan(repository, githubRefs, forgejoRefs, ancestry)
	if err != nil {
		return Result{}, err
	}
	result := Result{Actions: actions, Conflicts: conflicts}
	if dryRun {
		return result, nil
	}
	for _, conflict := range conflicts {
		if err := s.store.AddConflict(ctx, conflict); err != nil {
			return result, err
		}
	}
	for _, action := range actions {
		target := forgejo
		if action.To == GitHub {
			target = github
		}
		if _, err := run(ctx, gitDir, target, "--git-dir", gitDir, "push", "--porcelain", target.URL, action.SHA+":"+action.Ref); err != nil {
			return result, fmt.Errorf("push %s to %s: %w", action.Ref, action.To, err)
		}
	}
	return result, nil
}

func (r Remote) validate() error {
	if r.URL == "" {
		return errors.New("URL is empty")
	}
	parsed, err := url.Parse(r.URL)
	if err != nil {
		return errors.New("URL is invalid")
	}
	if parsed.User != nil {
		return errors.New("credentials must not be embedded in URL")
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		if parsed.Host == "" {
			return errors.New("HTTP URL has no host")
		}
		if r.Token == "" {
			return errors.New("HTTP remote token is empty")
		}
	}
	return nil
}

// fetchWithRetry retries transient gateway and network failures (for example
// a reverse proxy returning 502 while the forge restarts) with bounded
// exponential backoff. Authentication and repository-not-found failures fail
// immediately.
func fetchWithRetry(ctx context.Context, gitDir string, forge Forge, remote Remote) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		lastErr = fetch(ctx, gitDir, forge, remote)
		if lastErr == nil {
			return nil
		}
		if !transientGitFailure(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

func transientGitFailure(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, marker := range []string{"error: 502", "error: 503", "error: 504", "error: 429", "Connection reset by peer", "connection refused", "timed out", "Could not resolve host", "Failed to connect"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func fetch(ctx context.Context, gitDir string, forge Forge, remote Remote) error {
	prefix := "refs/forge-sync/" + string(forge)
	_, err := run(ctx, gitDir, remote,
		"--git-dir", gitDir, "fetch", "--no-tags", "--prune", remote.URL,
		"+refs/heads/*:"+prefix+"/heads/*",
		"+refs/tags/*:"+prefix+"/tags/*")
	if err != nil {
		return fmt.Errorf("fetch %s refs: %w", forge, err)
	}
	return nil
}

func localRefs(ctx context.Context, gitDir string, forge Forge) (map[string]string, error) {
	prefix := "refs/forge-sync/" + string(forge) + "/"
	output, err := run(ctx, gitDir, Remote{}, "--git-dir", gitDir, "for-each-ref", "--format=%(refname) %(objectname)", prefix)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 2 || !strings.HasPrefix(parts[0], prefix) {
			return nil, fmt.Errorf("invalid local ref line %q", scanner.Text())
		}
		name := strings.TrimPrefix(parts[0], prefix)
		if strings.HasPrefix(name, "heads/") {
			name = "refs/heads/" + strings.TrimPrefix(name, "heads/")
		} else if strings.HasPrefix(name, "tags/") {
			name = "refs/tags/" + strings.TrimPrefix(name, "tags/")
		} else {
			return nil, fmt.Errorf("unexpected local ref %q", parts[0])
		}
		result[name] = parts[1]
	}
	return result, scanner.Err()
}

type commandError struct {
	exitCode int
	output   string
}

func (e *commandError) Error() string {
	return fmt.Sprintf("git exited %d: %s", e.exitCode, e.output)
}

func run(ctx context.Context, directory string, remote Remote, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if remote.Token != "" {
		taskDir := filepath.Dir(directory)
		if strings.HasSuffix(directory, ".git") {
			taskDir = filepath.Dir(directory)
		}
		askpass := filepath.Join(taskDir, "askpass.sh")
		body := "#!/bin/sh\ncase \"$1\" in\n  *Username*) printf '%s\\n' \"$FORGE_SYNC_GIT_USERNAME\" ;;\n  *) printf '%s\\n' \"$FORGE_SYNC_GIT_TOKEN\" ;;\nesac\n"
		if err := os.WriteFile(askpass, []byte(body), 0o700); err != nil {
			return "", err
		}
		username := remote.Username
		if username == "" {
			username = "oauth2"
		}
		command.Env = append(command.Env,
			"GIT_ASKPASS="+askpass,
			"FORGE_SYNC_GIT_USERNAME="+username,
			"FORGE_SYNC_GIT_TOKEN="+remote.Token)
	}
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if ctx.Err() != nil {
		return output.String(), ctx.Err()
	}
	if err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return output.String(), &commandError{exitCode: exitCode, output: strings.TrimSpace(output.String())}
	}
	return output.String(), nil
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (w *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := maxGitOutput - w.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
			w.truncated = true
		}
		_, _ = w.buffer.Write(value)
	} else {
		w.truncated = true
	}
	return original, nil
}

func (w *limitedBuffer) String() string {
	if w.truncated {
		return w.buffer.String() + "\n[output truncated]"
	}
	return w.buffer.String()
}
