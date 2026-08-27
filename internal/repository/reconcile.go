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
}

func New(github GitHub, forgejo Forgejo, store *state.Store, githubToken string) *Reconciler {
	if github == nil || forgejo == nil || store == nil || githubToken == "" {
		panic("repository reconciler requires clients, state, and GitHub token")
	}
	return &Reconciler{github: github, forgejo: forgejo, store: store, githubToken: githubToken}
}

func (r *Reconciler) Discover(ctx context.Context, namespaces []string, dryRun bool) (model.Inventory, error) {
	var inventory model.Inventory
	for _, namespace := range namespaces {
		if namespace != "starintel-labs" && namespace != "lost-rob0t" {
			return inventory, fmt.Errorf("namespace %q is outside the allowed set", namespace)
		}
		githubRepositories, err := r.github.ListRepositories(ctx, namespace)
		if err != nil {
			return inventory, fmt.Errorf("enumerate GitHub namespace %s: %w", namespace, err)
		}
		forgejoRepositories, err := r.forgejo.ListRepositories(ctx, namespace)
		if err != nil {
			return inventory, fmt.Errorf("enumerate Forgejo namespace %s: %w", namespace, err)
		}
		inventory.GitHubRepositories += len(githubRepositories)
		inventory.ForgejoRepositories += len(forgejoRepositories)
		forgejoByName := make(map[string]model.Repository, len(forgejoRepositories))
		for _, repository := range forgejoRepositories {
			forgejoByName[strings.ToLower(repository.FullName)] = repository
		}
		for _, source := range githubRepositories {
			if err := validateRepository(source, namespace); err != nil {
				return inventory, err
			}
			mapping, mapped, err := r.store.RepositoryByGitHubID(ctx, source.ID)
			if err != nil {
				return inventory, err
			}
			if mapped {
				if mapping.ForgejoOwner != source.Owner || mapping.ForgejoName != source.Name {
					if !dryRun {
						if err := r.forgejo.UpdateRepositoryIdentity(ctx, mapping.ForgejoOwner, mapping.ForgejoName, source.Owner, source.Name); err != nil {
							return inventory, fmt.Errorf("update identity for GitHub repository %d: %w", source.ID, err)
						}
					}
				}
				if mapping.Visibility != source.Visibility || mapping.Archived != source.Archived {
					if !dryRun {
						if err := r.forgejo.UpdateRepositorySettings(ctx, source); err != nil {
							return inventory, fmt.Errorf("update settings for %s: %w", source.FullName, err)
						}
					}
				}
				if !dryRun {
					if err := r.store.UpsertRepository(ctx, mappingFor(source)); err != nil {
						return inventory, err
					}
				}
				inventory.InSync++
				continue
			}

			if target, exists := forgejoByName[strings.ToLower(source.FullName)]; exists {
				if target.Visibility == model.VisibilityPublic && source.Visibility != model.VisibilityPublic {
					if !dryRun {
						if err := r.forgejo.UpdateRepositorySettings(ctx, source); err != nil {
							return inventory, fmt.Errorf("fail closed existing repository %s: %w", source.FullName, err)
						}
					}
				}
				if !dryRun {
					if err := r.store.UpsertRepository(ctx, mappingFor(source)); err != nil {
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
			if _, err := r.forgejo.MigrateRepository(ctx, source, r.githubToken); err != nil {
				return inventory, err
			}
			if err := r.store.UpsertRepository(ctx, mappingFor(source)); err != nil {
				return inventory, err
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

func validateRepository(repository model.Repository, namespace string) error {
	if repository.ID <= 0 || repository.Name == "" || repository.FullName == "" || repository.Owner == "" {
		return errors.New("GitHub returned an incomplete repository")
	}
	if repository.Owner != namespace || repository.FullName != repository.Owner+"/"+repository.Name {
		return fmt.Errorf("GitHub repository %q escaped configured namespace %q", repository.FullName, namespace)
	}
	if repository.Visibility != model.VisibilityPrivate && repository.Visibility != model.VisibilityInternal && repository.Visibility != model.VisibilityPublic {
		repository.Visibility = model.VisibilityPrivate
	}
	return nil
}
