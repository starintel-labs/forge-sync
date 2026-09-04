package secrets

import (
	"context"
	"fmt"
	"sort"

	"github.com/starintel-labs/forge-sync/internal/model"
)

type Forgejo interface {
	SetActionSecret(context.Context, string, string, string, string) error
}

type Reconciler struct {
	forgejo Forgejo
	byRepo  map[string][]model.ActionSecret
}

func New(forgejo Forgejo, configured []model.ActionSecret) *Reconciler {
	if forgejo == nil {
		panic("action secret reconciler requires a Forgejo client")
	}
	byRepo := make(map[string][]model.ActionSecret)
	for _, secret := range configured {
		byRepo[secret.Repository] = append(byRepo[secret.Repository], secret)
	}
	for repository := range byRepo {
		sort.Slice(byRepo[repository], func(i, j int) bool {
			return byRepo[repository][i].Name < byRepo[repository][j].Name
		})
	}
	return &Reconciler{forgejo: forgejo, byRepo: byRepo}
}

func (r *Reconciler) Reconcile(ctx context.Context, mapping model.RepositoryMapping, dryRun bool) error {
	configured := r.byRepo[mapping.GitHubFullName]
	if len(configured) == 0 || dryRun {
		return nil
	}
	if mapping.ForgejoOwner == "" || mapping.ForgejoName == "" {
		return fmt.Errorf("Forgejo mapping for %s is incomplete", mapping.GitHubFullName)
	}
	for _, secret := range configured {
		if err := r.forgejo.SetActionSecret(ctx, mapping.ForgejoOwner, mapping.ForgejoName, secret.Name, secret.Value); err != nil {
			return fmt.Errorf("synchronize Actions secret %q for %s: %w", secret.Name, mapping.GitHubFullName, err)
		}
	}
	return nil
}
