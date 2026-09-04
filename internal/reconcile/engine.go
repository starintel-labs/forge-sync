package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/starintel-labs/forge-sync/internal/comments"
	"github.com/starintel-labs/forge-sync/internal/gitrefs"
	"github.com/starintel-labs/forge-sync/internal/issues"
	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/pullrequests"
	"github.com/starintel-labs/forge-sync/internal/releases"
	"github.com/starintel-labs/forge-sync/internal/repository"
	"github.com/starintel-labs/forge-sync/internal/secrets"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type RepositoryResult struct {
	Repository string           `json:"repository"`
	Actions    []gitrefs.Action `json:"actions,omitempty"`
	Conflicts  int              `json:"conflicts"`
}

type Report struct {
	Inventory    model.Inventory    `json:"inventory"`
	Repositories []RepositoryResult `json:"repositories,omitempty"`
}

type Engine struct {
	repositories     *repository.Reconciler
	issues           *issues.Reconciler
	comments         *comments.Reconciler
	pullRequests     *pullrequests.Reconciler
	releases         *releases.Reconciler
	actionSecrets    *secrets.Reconciler
	git              *gitrefs.Synchronizer
	store            *state.Store
	namespaces       []string
	githubToken      string
	forgejoToken     string
	forgejoCloneBase string
	maxConcurrency   int
	maxRefSizeKB     int64
	locks            sync.Map
}

func New(repositories *repository.Reconciler, issueReconciler *issues.Reconciler, commentReconciler *comments.Reconciler, pullRequestReconciler *pullrequests.Reconciler, releaseReconciler *releases.Reconciler, actionSecretReconciler *secrets.Reconciler, git *gitrefs.Synchronizer, store *state.Store, namespaces []string, githubToken, forgejoToken, forgejoAPI string, maxConcurrency int, maxRefSizeKB int64) *Engine {
	if repositories == nil || issueReconciler == nil || commentReconciler == nil || pullRequestReconciler == nil || releaseReconciler == nil || actionSecretReconciler == nil || git == nil || store == nil || len(namespaces) == 0 || githubToken == "" || forgejoToken == "" || maxConcurrency < 1 {
		panic("reconciliation engine configuration is incomplete")
	}
	if maxRefSizeKB <= 0 {
		maxRefSizeKB = defaultMaxRefSizeKB
	}
	return &Engine{
		repositories: repositories, issues: issueReconciler, comments: commentReconciler,
		pullRequests: pullRequestReconciler, releases: releaseReconciler,
		actionSecrets: actionSecretReconciler,
		git:           git, store: store,
		namespaces: append([]string(nil), namespaces...), githubToken: githubToken, forgejoToken: forgejoToken,
		forgejoCloneBase: strings.TrimSuffix(strings.TrimRight(forgejoAPI, "/"), "/api/v1"), maxConcurrency: maxConcurrency,
		maxRefSizeKB: maxRefSizeKB,
	}
}

// defaultMaxRefSizeKB excludes repositories whose git history cannot fit in
// the bounded workspace; operators can raise it, never disable the guard.
const defaultMaxRefSizeKB = 8 << 20 // 8 GiB

func (e *Engine) Discover(ctx context.Context, dryRun bool) (model.Inventory, error) {
	return e.repositories.Discover(ctx, e.namespaces, dryRun)
}

func (e *Engine) Reconcile(ctx context.Context, scope string, dryRun bool) (report Report, finalErr error) {
	if scope == "" {
		scope = "all"
	}
	runID, err := e.store.BeginRun(ctx, scope, dryRun)
	if err != nil {
		return Report{}, err
	}
	defer func() {
		encoded, _ := json.Marshal(report)
		if err := e.store.CompleteRun(context.WithoutCancel(ctx), runID, string(encoded), finalErr); err != nil && finalErr == nil {
			finalErr = err
		}
	}()

	report.Inventory, err = e.repositories.Discover(ctx, e.namespaces, dryRun)
	if err != nil {
		return report, err
	}
	mappings, err := e.store.ListRepositories(ctx)
	if err != nil {
		return report, err
	}
	if scope != "all" {
		filtered := mappings[:0]
		for _, mapping := range mappings {
			if strings.EqualFold(mapping.GitHubFullName, scope) {
				filtered = append(filtered, mapping)
			}
		}
		mappings = filtered
		if len(mappings) == 0 {
			return report, fmt.Errorf("repository %q is not mapped", scope)
		}
	}
	results, err := e.reconcileMappings(ctx, mappings, dryRun)
	if err != nil {
		return report, err
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Repository < results[j].Repository })
	report.Repositories = results
	conflicts, err := e.store.ListConflicts(ctx)
	if err != nil {
		return report, err
	}
	report.Inventory.Conflicted = len(conflicts)
	return report, nil
}

func (e *Engine) reconcileMappings(ctx context.Context, mappings []model.RepositoryMapping, dryRun bool) ([]RepositoryResult, error) {
	if len(mappings) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan model.RepositoryMapping, e.maxConcurrency)
	results := make(chan RepositoryResult, len(mappings))
	errorsFound := make(chan error, 1)
	workers := e.maxConcurrency
	if workers > len(mappings) {
		workers = len(mappings)
	}
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for mapping := range jobs {
				result, err := e.reconcileOne(ctx, mapping, dryRun)
				if err != nil {
					select {
					case errorsFound <- err:
						cancel()
					default:
					}
					return
				}
				select {
				case results <- result:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, mapping := range mappings {
			select {
			case jobs <- mapping:
			case <-ctx.Done():
				return
			}
		}
	}()
	wait.Wait()
	close(results)
	select {
	case err := <-errorsFound:
		return nil, err
	default:
	}
	var collected []RepositoryResult
	for result := range results {
		collected = append(collected, result)
	}
	if len(collected) != len(mappings) {
		return nil, errors.New("reconciliation stopped before all repositories completed")
	}
	return collected, nil
}

func (e *Engine) reconcileOne(ctx context.Context, mapping model.RepositoryMapping, dryRun bool) (RepositoryResult, error) {
	lockValue, _ := e.locks.LoadOrStore(mapping.GitHubID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	if err := e.actionSecrets.Reconcile(ctx, mapping, dryRun); err != nil {
		return RepositoryResult{}, fmt.Errorf("synchronize Actions secrets for %s: %w", mapping.GitHubFullName, err)
	}
	if mapping.SizeKB > e.maxRefSizeKB {
		// A repository whose history exceeds the bounded workspace is
		// excluded from ref synchronization (never deleted): issues, PRs,
		// comments, and releases still reconcile, and the exclusion is
		// recorded so operators see it.
		if !dryRun {
			if err := e.store.AddConflict(ctx, model.Conflict{
				Kind: "ref-sync-skipped", Repository: mapping.GitHubFullName,
				ObjectKey:   "git-refs",
				GitHubState: fmt.Sprintf("%d KiB", mapping.SizeKB), ForgejoState: fmt.Sprintf("limit %d KiB", e.maxRefSizeKB),
				LastKnownState: "workspace-bound", CreatedAt: time.Now().UTC(),
			}); err != nil {
				return RepositoryResult{}, err
			}
		}
		return RepositoryResult{Repository: mapping.GitHubFullName, Conflicts: 1}, nil
	}
	githubRemote := gitrefs.Remote{
		URL: "https://github.com/" + mapping.GitHubFullName + ".git", Username: "x-access-token", Token: e.githubToken,
	}
	forgejoRemote := gitrefs.Remote{
		URL: e.forgejoCloneBase + "/" + mapping.ForgejoOwner + "/" + mapping.ForgejoName + ".git", Username: "oauth2", Token: e.forgejoToken,
	}
	gitResult, err := e.git.Sync(ctx, mapping.GitHubFullName, githubRemote, forgejoRemote, dryRun)
	if err != nil {
		if dryRun {
			return RepositoryResult{}, fmt.Errorf("synchronize refs for %s: %w", mapping.GitHubFullName, err)
		}
		// A single repository whose mirror refuses a ref update (for
		// example a GitHub branch rule blocking force-pushes) must not
		// abort the whole cycle: record it durably, keep reconciling the
		// remaining objects, and let operators surface it via conflicts.
		if err := e.store.AddConflict(ctx, model.Conflict{
			Kind: "git-ref-error", Repository: mapping.GitHubFullName,
			ObjectKey:   "git-refs", GitHubState: "sync-failed", ForgejoState: "sync-failed",
			LastKnownState: err.Error(), CreatedAt: time.Now().UTC(),
		}); err != nil {
			return RepositoryResult{}, fmt.Errorf("record git-ref failure for %s: %w", mapping.GitHubFullName, err)
		}
		return RepositoryResult{Repository: mapping.GitHubFullName, Conflicts: 1}, nil
	}
	if err := e.issues.Reconcile(ctx, mapping, dryRun); err != nil {
		return RepositoryResult{}, fmt.Errorf("synchronize issues for %s: %w", mapping.GitHubFullName, err)
	}
	issueMappings, err := e.store.ListIssueMappings(ctx, mapping.GitHubID)
	if err != nil {
		return RepositoryResult{}, err
	}
	if err := e.comments.Reconcile(ctx, mapping, issueMappings, dryRun); err != nil {
		return RepositoryResult{}, fmt.Errorf("synchronize comments for %s: %w", mapping.GitHubFullName, err)
	}
	if err := e.pullRequests.Reconcile(ctx, mapping, dryRun); err != nil {
		return RepositoryResult{}, fmt.Errorf("synchronize pull requests for %s: %w", mapping.GitHubFullName, err)
	}
	pullRequestMappings, err := e.store.ListPullRequestMappings(ctx, mapping.GitHubID)
	if err != nil {
		return RepositoryResult{}, err
	}
	if err := e.comments.ReconcilePullRequests(ctx, mapping, pullRequestMappings, dryRun); err != nil {
		return RepositoryResult{}, fmt.Errorf("synchronize pull request comments for %s: %w", mapping.GitHubFullName, err)
	}
	if err := e.releases.Reconcile(ctx, mapping, dryRun); err != nil {
		if dryRun {
			return RepositoryResult{}, fmt.Errorf("synchronize releases for %s: %w", mapping.GitHubFullName, err)
		}
		// A release failure (for example an asset download timing out)
		// must not abort the cycle: record it durably and continue.
		if err := e.store.AddConflict(ctx, model.Conflict{
			Kind: "release-error", Repository: mapping.GitHubFullName,
			ObjectKey: "releases", GitHubState: "sync-failed", ForgejoState: "sync-failed",
			LastKnownState: err.Error(), CreatedAt: time.Now().UTC(),
		}); err != nil {
			return RepositoryResult{}, fmt.Errorf("record release failure for %s: %w", mapping.GitHubFullName, err)
		}
		return RepositoryResult{Repository: mapping.GitHubFullName, Conflicts: 1}, nil
	}
	return RepositoryResult{Repository: mapping.GitHubFullName, Actions: gitResult.Actions, Conflicts: len(gitResult.Conflicts)}, nil
}

func (e *Engine) ProcessWebhook(ctx context.Context, _ string, _ string, payload []byte) error {
	var envelope struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode webhook payload: %w", err)
	}
	if envelope.Repository.FullName == "" {
		_, err := e.Reconcile(ctx, "all", false)
		return err
	}
	owner, name, ok := strings.Cut(envelope.Repository.FullName, "/")
	if !ok || name == "" {
		return errors.New("webhook repository full name is malformed")
	}
	if contains(e.namespaces, owner) {
		_, err := e.Reconcile(ctx, envelope.Repository.FullName, false)
		return err
	}
	// Forgejo-side payloads carry the mapped owner, not the GitHub namespace:
	// resolve through the durable mapping table instead.
	matches, err := e.store.RepositoriesByForgejoPath(ctx, owner, name)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf("webhook repository %q is outside configured namespaces", envelope.Repository.FullName)
	}
	for _, match := range matches {
		if _, err := e.Reconcile(ctx, match.GitHubFullName, false); err != nil {
			return err
		}
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

type Scheduler struct {
	engine   *Engine
	interval time.Duration
}

func NewScheduler(engine *Engine, interval time.Duration) *Scheduler {
	if engine == nil || interval <= 0 {
		panic("scheduler requires engine and positive interval")
	}
	return &Scheduler{engine: engine, interval: interval}
}

func (s *Scheduler) Run(ctx context.Context, reportError func(error)) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.engine.Reconcile(ctx, "all", false); err != nil && ctx.Err() == nil {
				reportError(err)
			}
		}
	}
}
