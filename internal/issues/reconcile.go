package issues

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type Forge interface {
	ListIssues(context.Context, string, string) ([]model.Issue, error)
	CreateIssue(context.Context, string, string, model.Issue) (model.Issue, error)
	UpdateIssue(context.Context, string, string, int64, model.Issue) (model.Issue, error)
}

type Reconciler struct {
	github  Forge
	forgejo Forge
	store   *state.Store
}

func New(github, forgejo Forge, store *state.Store) *Reconciler {
	if github == nil || forgejo == nil || store == nil {
		panic("issue reconciler requires both forges and state")
	}
	return &Reconciler{github: github, forgejo: forgejo, store: store}
}

func (r *Reconciler) Reconcile(ctx context.Context, repository model.RepositoryMapping, dryRun bool) error {
	githubOwner, githubName, ok := strings.Cut(repository.GitHubFullName, "/")
	if !ok || githubOwner == "" || githubName == "" || repository.ForgejoOwner == "" || repository.ForgejoName == "" {
		return errors.New("repository mapping has invalid identity")
	}
	githubIssues, err := r.github.ListIssues(ctx, githubOwner, githubName)
	if err != nil {
		return fmt.Errorf("list GitHub issues: %w", err)
	}
	forgejoIssues, err := r.forgejo.ListIssues(ctx, repository.ForgejoOwner, repository.ForgejoName)
	if err != nil {
		return fmt.Errorf("list Forgejo issues: %w", err)
	}
	if err := validateIssues(githubIssues); err != nil {
		return fmt.Errorf("invalid GitHub issue response: %w", err)
	}
	if err := validateIssues(forgejoIssues); err != nil {
		return fmt.Errorf("invalid Forgejo issue response: %w", err)
	}
	mappings, err := r.store.ListIssueMappings(ctx, repository.GitHubID)
	if err != nil {
		return err
	}
	githubByID := byID(githubIssues)
	forgejoByID := byID(forgejoIssues)
	consumedGitHub := map[int64]bool{}
	consumedForgejo := map[int64]bool{}

	for _, mapping := range mappings {
		githubIssue, githubFound := githubByID[mapping.GitHubID]
		forgejoIssue, forgejoFound := forgejoByID[mapping.ForgejoID]
		if !githubFound || !forgejoFound {
			if !dryRun {
				if err := r.store.AddConflict(ctx, model.Conflict{
					Kind: "issue-missing", Repository: repository.GitHubFullName,
					ObjectKey:   fmt.Sprintf("github:%d/forgejo:%d", mapping.GitHubID, mapping.ForgejoID),
					GitHubState: stateOrMissing(githubIssue, githubFound), ForgejoState: stateOrMissing(forgejoIssue, forgejoFound),
					LastKnownState: mapping.LastStateHash, CreatedAt: time.Now().UTC(),
				}); err != nil {
					return err
				}
			}
			continue
		}
		consumedGitHub[githubIssue.ID] = true
		consumedForgejo[forgejoIssue.ID] = true
		githubHash := Hash(githubIssue)
		forgejoHash := Hash(forgejoIssue)
		switch {
		case githubHash == forgejoHash:
			if !dryRun && mapping.LastStateHash != githubHash {
				mapping.LastStateHash = githubHash
				mapping.UpdatedAt = time.Now().UTC()
				if err := r.store.UpsertIssueMapping(ctx, mapping); err != nil {
					return err
				}
			}
		case forgejoHash == mapping.LastStateHash:
			if dryRun {
				continue
			}
			if _, err := r.forgejo.UpdateIssue(ctx, repository.ForgejoOwner, repository.ForgejoName, mapping.ForgejoIndex, githubIssue); err != nil {
				return fmt.Errorf("copy GitHub issue %d to Forgejo: %w", mapping.GitHubID, err)
			}
			mapping.LastStateHash = githubHash
			mapping.UpdatedAt = time.Now().UTC()
			if err := r.store.UpsertIssueMapping(ctx, mapping); err != nil {
				return err
			}
		case githubHash == mapping.LastStateHash:
			if dryRun {
				continue
			}
			if _, err := r.github.UpdateIssue(ctx, githubOwner, githubName, mapping.GitHubIndex, forgejoIssue); err != nil {
				return fmt.Errorf("copy Forgejo issue %d to GitHub: %w", mapping.ForgejoID, err)
			}
			mapping.LastStateHash = forgejoHash
			mapping.UpdatedAt = time.Now().UTC()
			if err := r.store.UpsertIssueMapping(ctx, mapping); err != nil {
				return err
			}
		default:
			if dryRun {
				continue
			}
			if err := r.store.AddConflict(ctx, model.Conflict{
				Kind: "issue", Repository: repository.GitHubFullName,
				ObjectKey:   fmt.Sprintf("github:%d/forgejo:%d", mapping.GitHubID, mapping.ForgejoID),
				GitHubState: githubHash, ForgejoState: forgejoHash, LastKnownState: mapping.LastStateHash,
				CreatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
		}
	}

	unmappedGitHub := unconsumed(githubIssues, consumedGitHub)
	unmappedForgejo := unconsumed(forgejoIssues, consumedForgejo)
	pairedGitHub, pairedForgejo := uniqueHashPairs(unmappedGitHub, unmappedForgejo)
	for githubID, forgejoID := range pairedGitHub {
		githubIssue := githubByID[githubID]
		forgejoIssue := forgejoByID[forgejoID]
		consumedGitHub[githubID] = true
		consumedForgejo[forgejoID] = true
		if !dryRun {
			if err := r.store.UpsertIssueMapping(ctx, issueMapping(repository.GitHubID, githubIssue, forgejoIssue)); err != nil {
				return err
			}
		}
	}
	_ = pairedForgejo

	for _, githubIssue := range unconsumed(githubIssues, consumedGitHub) {
		if dryRun {
			continue
		}
		created, err := r.forgejo.CreateIssue(ctx, repository.ForgejoOwner, repository.ForgejoName, githubIssue)
		if err != nil {
			return fmt.Errorf("create Forgejo issue from GitHub issue %d: %w", githubIssue.ID, err)
		}
		if err := validateIssue(created); err != nil {
			return fmt.Errorf("invalid created Forgejo issue: %w", err)
		}
		if err := r.store.UpsertIssueMapping(ctx, issueMapping(repository.GitHubID, githubIssue, created)); err != nil {
			return err
		}
	}
	for _, forgejoIssue := range unconsumed(forgejoIssues, consumedForgejo) {
		if dryRun {
			continue
		}
		created, err := r.github.CreateIssue(ctx, githubOwner, githubName, forgejoIssue)
		if err != nil {
			return fmt.Errorf("create GitHub issue from Forgejo issue %d: %w", forgejoIssue.ID, err)
		}
		if err := validateIssue(created); err != nil {
			return fmt.Errorf("invalid created GitHub issue: %w", err)
		}
		if err := r.store.UpsertIssueMapping(ctx, issueMapping(repository.GitHubID, created, forgejoIssue)); err != nil {
			return err
		}
	}
	return nil
}

func Hash(issue model.Issue) string {
	labels := append([]string(nil), issue.Labels...)
	sort.Strings(labels)
	state := struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		State     string   `json:"state"`
		Labels    []string `json:"labels"`
		Milestone string   `json:"milestone"`
	}{issue.Title, issue.Body, issue.State, labels, issue.Milestone}
	encoded, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func validateIssues(all []model.Issue) error {
	seen := map[int64]bool{}
	for _, issue := range all {
		if err := validateIssue(issue); err != nil {
			return err
		}
		if seen[issue.ID] {
			return fmt.Errorf("duplicate issue ID %d", issue.ID)
		}
		seen[issue.ID] = true
	}
	return nil
}

func validateIssue(issue model.Issue) error {
	if issue.ID <= 0 || issue.Index <= 0 || issue.Title == "" {
		return errors.New("issue identity or title is empty")
	}
	if issue.State != "open" && issue.State != "closed" {
		return fmt.Errorf("issue %d has invalid state %q", issue.ID, issue.State)
	}
	return nil
}

func byID(all []model.Issue) map[int64]model.Issue {
	result := make(map[int64]model.Issue, len(all))
	for _, issue := range all {
		result[issue.ID] = issue
	}
	return result
}

func unconsumed(all []model.Issue, consumed map[int64]bool) []model.Issue {
	var result []model.Issue
	for _, issue := range all {
		if !consumed[issue.ID] {
			result = append(result, issue)
		}
	}
	return result
}

func uniqueHashPairs(githubIssues, forgejoIssues []model.Issue) (map[int64]int64, map[int64]int64) {
	githubByHash := map[string][]int64{}
	forgejoByHash := map[string][]int64{}
	for _, issue := range githubIssues {
		githubByHash[Hash(issue)] = append(githubByHash[Hash(issue)], issue.ID)
	}
	for _, issue := range forgejoIssues {
		forgejoByHash[Hash(issue)] = append(forgejoByHash[Hash(issue)], issue.ID)
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

func issueMapping(repositoryID int64, githubIssue, forgejoIssue model.Issue) model.IssueMapping {
	return model.IssueMapping{
		RepositoryGitHubID: repositoryID, GitHubID: githubIssue.ID, ForgejoID: forgejoIssue.ID,
		GitHubIndex: githubIssue.Index, ForgejoIndex: forgejoIssue.Index,
		LastStateHash: Hash(githubIssue), UpdatedAt: time.Now().UTC(),
	}
}

func stateOrMissing(issue model.Issue, found bool) string {
	if !found {
		return "missing"
	}
	return Hash(issue) + ":" + strconv.FormatInt(issue.ID, 10)
}
