package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type GitHub interface {
	ListRepositories(context.Context, string) ([]model.Repository, error)
}

type Forgejo interface {
	ListRepositories(context.Context, string) ([]model.Repository, error)
	MigrateRepository(context.Context, model.Repository, string) (model.Repository, error)
	UpdateRepositoryIdentity(context.Context, string, string, string, string) error
	UpdateRepositorySettings(context.Context, model.Repository) error
}

type Reconciler struct {
	github      GitHub
	forgejo     Forgejo
	store       *state.Store
	githubToken string
	ownerMap    map[string]string
}

// New builds a repository reconciler. ownerMap optionally redirects GitHub
// namespaces to different Forgejo owners (for example
// "starintel-labs:nsaspy"); a nil map keeps namespace identities.
func New(github GitHub, forgejo Forgejo, store *state.Store, githubToken string, ownerMap map[string]string) *Reconciler {
	if github == nil || forgejo == nil || store == nil || githubToken == "" {
		panic("repository reconciler requires clients, state, and GitHub token")
	}
	return &Reconciler{github: github, forgejo: forgejo, store: store, githubToken: githubToken, ownerMap: ownerMap}
}

func (r *Reconciler) forgejoOwner(namespace string) string {
	if owner, ok := r.ownerMap[namespace]; ok && owner != "" {
		return owner
	}
	return namespace
}

func (r *Reconciler) Discover(ctx context.Context, namespaces []string, dryRun bool) (model.Inventory, error) {
	var inventory model.Inventory
	countedForgejo := map[string]bool{}
	for _, namespace := range namespaces {
		if namespace != "starintel-labs" && namespace != "lost-rob0t" {
			return inventory, fmt.Errorf("namespace %q is outside the allowed set", namespace)
		}
		githubRepositories, err := r.github.ListRepositories(ctx, namespace)
		if err != nil {
			return inventory, fmt.Errorf("enumerate GitHub namespace %s: %w", namespace, err)
		}
		forgejoOwner := r.forgejoOwner(namespace)
		forgejoRepositories, err := r.forgejo.ListRepositories(ctx, forgejoOwner)
		if err != nil {
			return inventory, fmt.Errorf("enumerate Forgejo owner %s: %w", forgejoOwner, err)
		}
		inventory.GitHubRepositories += len(githubRepositories)
		if !countedForgejo[forgejoOwner] {
			countedForgejo[forgejoOwner] = true
			inventory.ForgejoRepositories += len(forgejoRepositories)
		}
		forgejoByName := make(map[string]model.Repository, len(forgejoRepositories))
		for _, repository := range forgejoRepositories {
			forgejoByName[strings.ToLower(repository.FullName)] = repository
		}
		for _, source := range githubRepositories {
			if err := validateRepository(source); err != nil {
				return inventory, err
			}
			// GitHub's type=all listings include collaborator and org-member
			// repositories; only repositories owned by the namespace sync.
			if source.Owner != namespace {
				continue
			}
			mapping, mapped, err := r.store.RepositoryByGitHubID(ctx, source.ID)
			if err != nil {
				return inventory, err
			}
			target := source
			target.Owner = forgejoOwner
			target.FullName = forgejoOwner + "/" + source.Name
			if mapped {
				if mapping.ForgejoOwner != target.Owner || mapping.ForgejoName != target.Name {
					if !dryRun {
						if err := r.forgejo.UpdateRepositoryIdentity(ctx, mapping.ForgejoOwner, mapping.ForgejoName, target.Owner, target.Name); err != nil {
							return inventory, fmt.Errorf("update identity for GitHub repository %d: %w", source.ID, err)
						}
					}
				}
				if mapping.Visibility != source.Visibility || mapping.Archived != source.Archived {
					if !dryRun {
						if err := r.forgejo.UpdateRepositorySettings(ctx, target); err != nil {
							return inventory, fmt.Errorf("update settings for %s: %w", source.FullName, err)
						}
					}
				}
				if !dryRun {
					if err := r.store.UpsertRepository(ctx, mappingForPair(source, target.Owner, target.Name)); err != nil {
						return inventory, err
					}
				}
				inventory.InSync++
				continue
			}

			if existing, exists := forgejoByName[strings.ToLower(target.FullName)]; exists {
				if existing.Visibility == model.VisibilityPublic && source.Visibility != model.VisibilityPublic {
					if !dryRun {
						if err := r.forgejo.UpdateRepositorySettings(ctx, target); err != nil {
							return inventory, fmt.Errorf("fail closed existing repository %s: %w", target.FullName, err)
						}
					}
				}
				if !dryRun {
					if err := r.store.UpsertRepository(ctx, mappingForPair(source, existing.Owner, existing.Name)); err != nil {
						return inventory, err
					}
				}
				inventory.InSync++
				continue
			}

			inventory.Missing++
			if dryRun {
				continue
			}
			migrated, err := r.forgejo.MigrateRepository(ctx, target, r.githubToken)
			if err != nil {
				return inventory, err
			}
			if !dryRun {
				// GitHub identity comes from discovery only; the migration
				// response defines the Forgejo side.
				mapping := mappingFor(migrated)
				mapping.GitHubID = source.ID
				mapping.GitHubFullName = source.FullName
				mapping.ForgejoOwner = target.Owner
				mapping.ForgejoName = target.Name
				mapping.Visibility = source.Visibility
				mapping.Archived = source.Archived
				if err := r.store.UpsertRepository(ctx, mapping); err != nil {
					return inventory, err
				}
			}
		}
	}
	conflicts, err := r.store.ListConflicts(ctx)
	if err != nil {
		return inventory, err
	}
	inventory.Conflicted = len(conflicts)
	return inventory, nil
}

// mappingForPair records a mapping whose GitHub identity is canonical and
// whose Forgejo identity is the (possibly redirected) target owner/name.
func mappingForPair(source model.Repository, forgejoOwner, forgejoName string) model.RepositoryMapping {
	return model.RepositoryMapping{
		GitHubID: source.ID, GitHubFullName: source.FullName,
		ForgejoOwner: forgejoOwner, ForgejoName: forgejoName,
		Visibility: source.Visibility, Archived: source.Archived,
		LastStateHash: repositoryHash(source), UpdatedAt: time.Now().UTC(),
	}
}

func mappingFor(repository model.Repository) model.RepositoryMapping {
	return model.RepositoryMapping{
		GitHubID: repository.ID, GitHubFullName: repository.FullName,
		ForgejoOwner: repository.Owner, ForgejoName: repository.Name,
		Visibility: repository.Visibility, Archived: repository.Archived,
		LastStateHash: repositoryHash(repository), UpdatedAt: time.Now().UTC(),
	}
}

func repositoryHash(repository model.Repository) string {
	state := struct {
		ID         int64            `json:"id"`
		FullName   string           `json:"full_name"`
		Visibility model.Visibility `json:"visibility"`
		Archived   bool             `json:"archived"`
	}{repository.ID, repository.FullName, repository.Visibility, repository.Archived}
	encoded, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func validateRepository(repository model.Repository) error {
	if repository.ID <= 0 || repository.Name == "" || repository.FullName == "" || repository.Owner == "" {
		return errors.New("GitHub returned an incomplete repository")
	}
	if repository.FullName != repository.Owner+"/"+repository.Name {
		return fmt.Errorf("GitHub repository %q has a malformed full name", repository.FullName)
	}
	return nil
}
