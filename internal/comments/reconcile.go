package comments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type Forge interface {
	ListComments(context.Context, string, string, int64) ([]model.Comment, error)
	CreateComment(context.Context, string, string, int64, model.Comment) (model.Comment, error)
	UpdateComment(context.Context, string, string, int64, model.Comment) (model.Comment, error)
}

type Reconciler struct {
	github, forgejo Forge
	store           *state.Store
}

func New(github, forgejo Forge, store *state.Store) *Reconciler {
	if github == nil || forgejo == nil || store == nil {
		panic("comment reconciler requires both forges and state")
	}
	return &Reconciler{github: github, forgejo: forgejo, store: store}
}

func (r *Reconciler) Reconcile(ctx context.Context, repository model.RepositoryMapping, issueMappings []model.IssueMapping, dryRun bool) error {
	githubOwner, githubName, ok := strings.Cut(repository.GitHubFullName, "/")
	if !ok || githubOwner == "" || githubName == "" || repository.ForgejoOwner == "" || repository.ForgejoName == "" {
		return errors.New("repository mapping has invalid identity")
	}
	for _, issueMapping := range issueMappings {
		githubComments, err := r.github.ListComments(ctx, githubOwner, githubName, issueMapping.GitHubIndex)
		if err != nil {
			return fmt.Errorf("list GitHub comments for issue %d: %w", issueMapping.GitHubID, err)
		}
		forgejoComments, err := r.forgejo.ListComments(ctx, repository.ForgejoOwner, repository.ForgejoName, issueMapping.ForgejoIndex)
		if err != nil {
			return fmt.Errorf("list Forgejo comments for issue %d: %w", issueMapping.ForgejoID, err)
		}
		if err := r.reconcileThread(ctx, repository, thread{githubID: issueMapping.GitHubID, githubIndex: issueMapping.GitHubIndex, forgejoIndex: issueMapping.ForgejoIndex}, githubOwner, githubName, githubComments, forgejoComments, dryRun); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) ReconcilePullRequests(ctx context.Context, repository model.RepositoryMapping, pullRequestMappings []model.PullRequestMapping, dryRun bool) error {
	githubOwner, githubName, ok := strings.Cut(repository.GitHubFullName, "/")
	if !ok || githubOwner == "" || githubName == "" || repository.ForgejoOwner == "" || repository.ForgejoName == "" {
		return errors.New("repository mapping has invalid identity")
	}
	for _, pullRequestMapping := range pullRequestMappings {
		githubComments, err := r.github.ListComments(ctx, githubOwner, githubName, pullRequestMapping.GitHubIndex)
		if err != nil {
			return fmt.Errorf("list GitHub comments for pull request %d: %w", pullRequestMapping.GitHubID, err)
		}
		forgejoComments, err := r.forgejo.ListComments(ctx, repository.ForgejoOwner, repository.ForgejoName, pullRequestMapping.ForgejoIndex)
		if err != nil {
			return fmt.Errorf("list Forgejo comments for pull request %d: %w", pullRequestMapping.ForgejoID, err)
		}
		if err := r.reconcileThread(ctx, repository, thread{githubID: pullRequestMapping.GitHubID, githubIndex: pullRequestMapping.GitHubIndex, forgejoIndex: pullRequestMapping.ForgejoIndex}, githubOwner, githubName, githubComments, forgejoComments, dryRun); err != nil {
			return err
		}
	}
	return nil
}

type thread struct {
	githubID     int64
	githubIndex  int64
	forgejoIndex int64
}

func (r *Reconciler) reconcileThread(ctx context.Context, repository model.RepositoryMapping, target thread, githubOwner, githubName string, githubComments, forgejoComments []model.Comment, dryRun bool) error {
	if err := validate(githubComments); err != nil {
		return err
	}
	if err := validate(forgejoComments); err != nil {
		return err
	}
	mappings, err := r.store.ListCommentMappings(ctx, repository.GitHubID, target.githubID)
	if err != nil {
		return err
	}
	githubByID := commentByID(githubComments)
	forgejoByID := commentByID(forgejoComments)
	consumedGitHub := map[int64]bool{}
	consumedForgejo := map[int64]bool{}
	for _, mapping := range mappings {
		githubComment, githubFound := githubByID[mapping.GitHubID]
		forgejoComment, forgejoFound := forgejoByID[mapping.ForgejoID]
		if !githubFound || !forgejoFound {
			if !dryRun {
				if err := r.store.AddConflict(ctx, model.Conflict{
					Kind: "comment-missing", Repository: repository.GitHubFullName,
					ObjectKey:   fmt.Sprintf("github:%d/forgejo:%d", mapping.GitHubID, mapping.ForgejoID),
					GitHubState: hashOrMissing(githubComment, githubFound), ForgejoState: hashOrMissing(forgejoComment, forgejoFound),
					LastKnownState: mapping.LastStateHash, CreatedAt: time.Now().UTC(),
				}); err != nil {
					return err
				}
			}
			continue
		}
		consumedGitHub[githubComment.ID] = true
		consumedForgejo[forgejoComment.ID] = true
		githubHash, forgejoHash := Hash(githubComment), Hash(forgejoComment)
		switch {
		case githubHash == forgejoHash:
			if !dryRun && mapping.LastStateHash != githubHash {
				mapping.LastStateHash, mapping.UpdatedAt = githubHash, time.Now().UTC()
				if err := r.store.UpsertCommentMapping(ctx, mapping); err != nil {
					return err
				}
			}
		case forgejoHash == mapping.LastStateHash:
			if !dryRun {
				if _, err := r.forgejo.UpdateComment(ctx, repository.ForgejoOwner, repository.ForgejoName, mapping.ForgejoID, githubComment); err != nil {
					return err
				}
				mapping.LastStateHash, mapping.UpdatedAt = githubHash, time.Now().UTC()
				if err := r.store.UpsertCommentMapping(ctx, mapping); err != nil {
					return err
				}
			}
		case githubHash == mapping.LastStateHash:
			if !dryRun {
				if _, err := r.github.UpdateComment(ctx, githubOwner, githubName, mapping.GitHubID, forgejoComment); err != nil {
					return err
				}
				mapping.LastStateHash, mapping.UpdatedAt = forgejoHash, time.Now().UTC()
				if err := r.store.UpsertCommentMapping(ctx, mapping); err != nil {
					return err
				}
			}
		default:
			if !dryRun {
				if err := r.store.AddConflict(ctx, model.Conflict{
					Kind: "comment", Repository: repository.GitHubFullName,
					ObjectKey:   fmt.Sprintf("github:%d/forgejo:%d", mapping.GitHubID, mapping.ForgejoID),
					GitHubState: githubHash, ForgejoState: forgejoHash, LastKnownState: mapping.LastStateHash,
					CreatedAt: time.Now().UTC(),
				}); err != nil {
					return err
				}
			}
		}
	}

	for githubID, forgejoID := range uniquePairs(unconsumed(githubComments, consumedGitHub), unconsumed(forgejoComments, consumedForgejo)) {
		githubComment, forgejoComment := githubByID[githubID], forgejoByID[forgejoID]
		consumedGitHub[githubID], consumedForgejo[forgejoID] = true, true
		if !dryRun {
			if err := r.store.UpsertCommentMapping(ctx, mappingFor(repository.GitHubID, target.githubID, githubComment, forgejoComment)); err != nil {
				return err
			}
		}
	}
	for _, githubComment := range unconsumed(githubComments, consumedGitHub) {
		if dryRun {
			continue
		}
		created, err := r.forgejo.CreateComment(ctx, repository.ForgejoOwner, repository.ForgejoName, target.forgejoIndex, githubComment)
		if err != nil {
			return err
		}
		if err := r.store.UpsertCommentMapping(ctx, mappingFor(repository.GitHubID, target.githubID, githubComment, created)); err != nil {
			return err
		}
	}
	for _, forgejoComment := range unconsumed(forgejoComments, consumedForgejo) {
		if dryRun {
			continue
		}
		created, err := r.github.CreateComment(ctx, githubOwner, githubName, target.githubIndex, forgejoComment)
		if err != nil {
			return err
		}
		if err := r.store.UpsertCommentMapping(ctx, mappingFor(repository.GitHubID, target.githubID, created, forgejoComment)); err != nil {
			return err
		}
	}
	return nil
}

func Hash(comment model.Comment) string {
	sum := sha256.Sum256([]byte(comment.Body))
	return hex.EncodeToString(sum[:])
}

func validate(comments []model.Comment) error {
	seen := map[int64]bool{}
	for _, comment := range comments {
		if comment.ID <= 0 {
			return errors.New("comment ID is invalid")
		}
		if seen[comment.ID] {
			return fmt.Errorf("duplicate comment ID %d", comment.ID)
		}
		seen[comment.ID] = true
	}
	return nil
}

func commentByID(comments []model.Comment) map[int64]model.Comment {
	result := make(map[int64]model.Comment, len(comments))
	for _, comment := range comments {
		result[comment.ID] = comment
	}
	return result
}

func unconsumed(comments []model.Comment, consumed map[int64]bool) []model.Comment {
	var result []model.Comment
	for _, comment := range comments {
		if !consumed[comment.ID] {
			result = append(result, comment)
		}
	}
	return result
}

func uniquePairs(githubComments, forgejoComments []model.Comment) map[int64]int64 {
	githubByHash := map[string][]int64{}
	forgejoByHash := map[string][]int64{}
	for _, comment := range githubComments {
		githubByHash[Hash(comment)] = append(githubByHash[Hash(comment)], comment.ID)
	}
	for _, comment := range forgejoComments {
		forgejoByHash[Hash(comment)] = append(forgejoByHash[Hash(comment)], comment.ID)
	}
	result := map[int64]int64{}
	for hash, githubIDs := range githubByHash {
		forgejoIDs := forgejoByHash[hash]
		if len(githubIDs) == 1 && len(forgejoIDs) == 1 {
			result[githubIDs[0]] = forgejoIDs[0]
		}
	}
	return result
}

func mappingFor(repositoryID, issueGitHubID int64, githubComment, forgejoComment model.Comment) model.CommentMapping {
	return model.CommentMapping{
		RepositoryGitHubID: repositoryID, IssueGitHubID: issueGitHubID,
		GitHubID: githubComment.ID, ForgejoID: forgejoComment.ID,
		LastStateHash: Hash(githubComment), UpdatedAt: time.Now().UTC(),
	}
}

func hashOrMissing(comment model.Comment, found bool) string {
	if !found {
		return "missing"
	}
	return Hash(comment)
}
