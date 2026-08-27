package pullrequests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type Forge interface {
	ListPullRequests(context.Context, string, string) ([]model.PullRequest, error)
	CreatePullRequest(context.Context, string, string, model.PullRequest) (model.PullRequest, error)
	UpdatePullRequest(context.Context, string, string, int64, model.PullRequest) (model.PullRequest, error)
}

type Reconciler struct {
	github  Forge
	forgejo Forge
	store   *state.Store
}

func New(github, forgejo Forge, store *state.Store) *Reconciler {
	if github == nil || forgejo == nil || store == nil {
		panic("pull request reconciler requires both forges and state")
	}
	return &Reconciler{github: github, forgejo: forgejo, store: store}
}

func (r *Reconciler) Reconcile(ctx context.Context, repository model.RepositoryMapping, dryRun bool) error {
	githubOwner, githubName, ok := strings.Cut(repository.GitHubFullName, "/")
	if !ok || githubOwner == "" || githubName == "" || repository.ForgejoOwner == "" || repository.ForgejoName == "" {
		return errors.New("repository mapping has invalid identity")
	}
	githubPullRequests, err := r.github.ListPullRequests(ctx, githubOwner, githubName)
	if err != nil {
		return fmt.Errorf("list GitHub pull requests: %w", err)
	}
	forgejoPullRequests, err := r.forgejo.ListPullRequests(ctx, repository.ForgejoOwner, repository.ForgejoName)
	if err != nil {
		return fmt.Errorf("list Forgejo pull requests: %w", err)
	}
	if err := validatePullRequests(githubPullRequests); err != nil {
		return fmt.Errorf("invalid GitHub pull request response: %w", err)
	}
	if err := validatePullRequests(forgejoPullRequests); err != nil {
		return fmt.Errorf("invalid Forgejo pull request response: %w", err)
	}
	mappings, err := r.store.ListPullRequestMappings(ctx, repository.GitHubID)
	if err != nil {
		return err
	}
	githubByID := byID(githubPullRequests)
	forgejoByID := byID(forgejoPullRequests)
	consumedGitHub := map[int64]bool{}
	consumedForgejo := map[int64]bool{}

	for _, mapping := range mappings {
		githubItem, githubFound := githubByID[mapping.GitHubID]
		forgejoItem, forgejoFound := forgejoByID[mapping.ForgejoID]
		if !githubFound || !forgejoFound {
			if !dryRun {
				if err := r.store.AddConflict(ctx, model.Conflict{
					Kind: "pull-request-missing", Repository: repository.GitHubFullName,
					ObjectKey:   fmt.Sprintf("github:%d/forgejo:%d", mapping.GitHubID, mapping.ForgejoID),
					GitHubState: stateOrMissing(githubItem, githubFound), ForgejoState: stateOrMissing(forgejoItem, forgejoFound),
					LastKnownState: mapping.LastStateHash, CreatedAt: time.Now().UTC(),
				}); err != nil {
					return err
				}
			}
			continue
		}
		consumedGitHub[githubItem.ID] = true
		consumedForgejo[forgejoItem.ID] = true
		githubHash := Hash(githubItem)
		forgejoHash := Hash(forgejoItem)
		switch {
		case githubHash == forgejoHash:
			if !dryRun && mapping.LastStateHash != githubHash {
				mapping.LastStateHash = githubHash
				mapping.UpdatedAt = time.Now().UTC()
				if err := r.store.UpsertPullRequestMapping(ctx, mapping); err != nil {
					return err
				}
			}
		case forgejoHash == mapping.LastStateHash:
			if dryRun {
				continue
			}
			if _, err := r.forgejo.UpdatePullRequest(ctx, repository.ForgejoOwner, repository.ForgejoName, mapping.ForgejoIndex, githubItem); err != nil {
				return fmt.Errorf("copy GitHub pull request %d to Forgejo: %w", mapping.GitHubID, err)
			}
			mapping.LastStateHash = githubHash
			mapping.UpdatedAt = time.Now().UTC()
			if err := r.store.UpsertPullRequestMapping(ctx, mapping); err != nil {
				return err
			}
		case githubHash == mapping.LastStateHash:
			if dryRun {
				continue
			}
			if _, err := r.github.UpdatePullRequest(ctx, githubOwner, githubName, mapping.GitHubIndex, forgejoItem); err != nil {
				return fmt.Errorf("copy Forgejo pull request %d to GitHub: %w", mapping.ForgejoID, err)
			}
			mapping.LastStateHash = forgejoHash
			mapping.UpdatedAt = time.Now().UTC()
			if err := r.store.UpsertPullRequestMapping(ctx, mapping); err != nil {
				return err
			}
		default:
			if dryRun {
				continue
			}
			if err := r.store.AddConflict(ctx, model.Conflict{
				Kind: "pull-request", Repository: repository.GitHubFullName,
				ObjectKey:   fmt.Sprintf("github:%d/forgejo:%d", mapping.GitHubID, mapping.ForgejoID),
				GitHubState: githubHash, ForgejoState: forgejoHash, LastKnownState: mapping.LastStateHash,
				CreatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
		}
	}

	unmappedGitHub := unconsumed(githubPullRequests, consumedGitHub)
	unmappedForgejo := unconsumed(forgejoPullRequests, consumedForgejo)
	pairedGitHub, _ := uniqueHashPairs(unmappedGitHub, unmappedForgejo)
	for githubID, forgejoID := range pairedGitHub {
		githubItem := githubByID[githubID]
		forgejoItem := forgejoByID[forgejoID]
		consumedGitHub[githubID] = true
		consumedForgejo[forgejoID] = true
		if !dryRun {
			if err := r.store.UpsertPullRequestMapping(ctx, pullRequestMapping(repository.GitHubID, githubItem, forgejoItem)); err != nil {
				return err
			}
		}
	}

	for _, githubItem := range unconsumed(githubPullRequests, consumedGitHub) {
		if dryRun {
			continue
		}
		created, err := r.forgejo.CreatePullRequest(ctx, repository.ForgejoOwner, repository.ForgejoName, githubItem)
		if err != nil {
			return fmt.Errorf("create Forgejo pull request from GitHub pull request %d: %w", githubItem.ID, err)
		}
		if err := validatePullRequest(created); err != nil {
			return fmt.Errorf("invalid created Forgejo pull request: %w", err)
		}
		if err := r.store.UpsertPullRequestMapping(ctx, pullRequestMapping(repository.GitHubID, githubItem, created)); err != nil {
			return err
		}
	}
	for _, forgejoItem := range unconsumed(forgejoPullRequests, consumedForgejo) {
		if dryRun {
			continue
		}
		created, err := r.github.CreatePullRequest(ctx, githubOwner, githubName, forgejoItem)
		if err != nil {
			return fmt.Errorf("create GitHub pull request from Forgejo pull request %d: %w", forgejoItem.ID, err)
		}
		if err := validatePullRequest(created); err != nil {
			return fmt.Errorf("invalid created GitHub pull request: %w", err)
		}
		if err := r.store.UpsertPullRequestMapping(ctx, pullRequestMapping(repository.GitHubID, created, forgejoItem)); err != nil {
			return err
		}
	}
	return nil
}

func Hash(item model.PullRequest) string {
	state := struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
		Head   string `json:"head"`
		Base   string `json:"base"`
		Draft  bool   `json:"draft"`
		Merged bool   `json:"merged"`
	}{item.Title, item.Body, item.State, item.Head, item.Base, item.Draft, item.Merged}
	encoded, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func validatePullRequests(all []model.PullRequest) error {
	seen := map[int64]bool{}
	for _, item := range all {
		if err := validatePullRequest(item); err != nil {
			return err
		}
		if seen[item.ID] {
			return fmt.Errorf("duplicate pull request ID %d", item.ID)
		}
		seen[item.ID] = true
	}
	return nil
}

func validatePullRequest(item model.PullRequest) error {
	if item.ID <= 0 || item.Index <= 0 || item.Title == "" {
		return errors.New("pull request identity or title is empty")
	}
	if item.State != "open" && item.State != "closed" {
		return fmt.Errorf("pull request %d has invalid state %q", item.ID, item.State)
	}
	if item.Head == "" || item.Base == "" {
		return fmt.Errorf("pull request %d has empty head or base ref", item.ID)
	}
	return nil
}

func byID(all []model.PullRequest) map[int64]model.PullRequest {
	result := make(map[int64]model.PullRequest, len(all))
	for _, item := range all {
		result[item.ID] = item
	}
	return result
}

func unconsumed(all []model.PullRequest, consumed map[int64]bool) []model.PullRequest {
	var result []model.PullRequest
	for _, item := range all {
		if !consumed[item.ID] {
			result = append(result, item)
		}
	}
	return result
}

func uniqueHashPairs(githubItems, forgejoItems []model.PullRequest) (map[int64]int64, map[int64]int64) {
	githubByHash := map[string][]int64{}
	forgejoByHash := map[string][]int64{}
	for _, item := range githubItems {
		githubByHash[Hash(item)] = append(githubByHash[Hash(item)], item.ID)
	}
	for _, item := range forgejoItems {
		forgejoByHash[Hash(item)] = append(forgejoByHash[Hash(item)], item.ID)
	}
	githubPairs := map[int64]int64{}
	forgejoPairs := map[int64]int64{}
	for hash, githubIDs := range githubByHash {
		forgejoIDs := forgejoByHash[hash]
		if len(githubIDs) == 1 && len(forgejoIDs) == 1 {
			githubPairs[githubIDs[0]] = forgejoIDs[0]
			forgejoPairs[forgejoIDs[0]] = githubIDs[0]
		}
	}
	return githubPairs, forgejoPairs
}

func pullRequestMapping(repositoryID int64, githubItem, forgejoItem model.PullRequest) model.PullRequestMapping {
	return model.PullRequestMapping{
		RepositoryGitHubID: repositoryID, GitHubID: githubItem.ID, ForgejoID: forgejoItem.ID,
		GitHubIndex: githubItem.Index, ForgejoIndex: forgejoItem.Index,
		LastStateHash: Hash(githubItem), UpdatedAt: time.Now().UTC(),
	}
}

func stateOrMissing(item model.PullRequest, found bool) string {
	if !found {
		return "missing"
	}
	return Hash(item) + ":" + strconv.FormatInt(item.ID, 10)
}
