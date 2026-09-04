package webhooks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/starintel-labs/forge-sync/internal/model"
)

// Hook is the desired webhook configuration for a forge target.
type Hook struct {
	ID     int64
	URL    string
	Events []string
	Active bool
}

// WireForge is the webhook-management surface both forge clients implement.
// ListOrgWebhooks reports found=false when the namespace is not an
// organization, which routes the wiring to per-repository hooks.
type WireForge interface {
	ListOrgWebhooks(ctx context.Context, org string) ([]Hook, bool, error)
	CreateOrgWebhook(ctx context.Context, org string, hook Hook, secret string) error
	UpdateOrgWebhook(ctx context.Context, org string, id int64, hook Hook, secret string) error
	ListRepoWebhooks(ctx context.Context, owner, name string) ([]Hook, error)
	CreateRepoWebhook(ctx context.Context, owner, name string, hook Hook, secret string) error
	UpdateRepoWebhook(ctx context.Context, owner, name string, id int64, hook Hook, secret string) error
	ListRepositories(ctx context.Context, namespace string) ([]model.Repository, error)
}

var (
	githubHookEvents = []string{
		"push", "repository", "issues", "issue_comment", "pull_request",
		"pull_request_review", "pull_request_review_comment", "release",
	}
	forgejoHookEvents = []string{"push", "repository", "issues", "issue_comment", "pull_request", "release"}
)

// Report records what the wiring ensured, for both real and dry runs.
type Report struct {
	Created   []string `json:"created"`
	Updated   []string `json:"updated"`
	Unchanged []string `json:"unchanged"`
	Skipped   []string `json:"skipped,omitempty"`
}

type Wiring struct {
	github     WireForge
	forgejo    WireForge
	namespaces []string
}

func NewWiring(github, forgejo WireForge, namespaces []string) *Wiring {
	if github == nil || forgejo == nil || len(namespaces) == 0 {
		panic("webhook wiring requires both forges and namespaces")
	}
	return &Wiring{github: github, forgejo: forgejo, namespaces: namespaces}
}

// Wire ensures the forge-sync webhooks on both forges: the receiver URL for
// GitHub hooks is the deployment's public webhook URL, and Forgejo hooks
// target the loopback listener. A hook at the target URL is updated in place
// when its events or active state drift; hooks at other URLs are left
// untouched. The webhook secret cannot be read back from either API, so
// rotating FORGE_SYNC_*_WEBHOOK_SECRET requires deleting the existing hook
// before the next wire run recreates it with the new value.
func (w *Wiring) Wire(ctx context.Context, githubURL, forgejoURL, githubSecret, forgejoSecret string, dryRun bool) (Report, error) {
	var report Report
	if githubURL == "" {
		report.Skipped = append(report.Skipped, "github: FORGE_SYNC_GITHUB_WEBHOOK_URL is not configured")
	} else if err := w.wireForge(ctx, w.github, githubURL, githubSecret, githubHookEvents, "github", dryRun, &report); err != nil {
		return report, err
	}
	if forgejoURL == "" {
		report.Skipped = append(report.Skipped, "forgejo: FORGE_SYNC_FORGEJO_WEBHOOK_URL is not configured")
	} else if err := w.wireForge(ctx, w.forgejo, forgejoURL, forgejoSecret, forgejoHookEvents, "forgejo", dryRun, &report); err != nil {
		return report, err
	}
	return report, nil
}

func (w *Wiring) wireForge(ctx context.Context, forge WireForge, hookURL, secret string, events []string, forgeName string, dryRun bool, report *Report) error {
	for _, namespace := range w.namespaces {
		orgHooks, found, err := forge.ListOrgWebhooks(ctx, namespace)
		if err != nil {
			return fmt.Errorf("list %s org webhooks for %s: %w", forgeName, namespace, err)
		}
		if found {
			if err := w.ensureHook(ctx, forge, orgTarget(namespace), hookURL, secret, events, orgHooks, dryRun, report, forgeName+":org/"+namespace); err != nil {
				return err
			}
			continue
		}
		repositories, err := forge.ListRepositories(ctx, namespace)
		if err != nil {
			return fmt.Errorf("list %s repositories for %s: %w", forgeName, namespace, err)
		}
		for _, repository := range repositories {
			hooks, err := forge.ListRepoWebhooks(ctx, repository.Owner, repository.Name)
			if err != nil {
				return fmt.Errorf("list %s webhooks for %s: %w", forgeName, repository.FullName, err)
			}
			label := fmt.Sprintf("%s:repo/%s", forgeName, repository.FullName)
			if err := w.ensureHook(ctx, forge, repoTarget(repository.Owner, repository.Name), hookURL, secret, events, hooks, dryRun, report, label); err != nil {
				return err
			}
		}
	}
	return nil
}

// target identifies one hookable object; owner is the org name for org-level
// hooks or the repository owner for repo-level hooks.
type target struct {
	org  string
	repo string
}

func orgTarget(org string) target { return target{org: org} }
func repoTarget(owner, name string) target {
	return target{org: owner, repo: name}
}

func (w *Wiring) ensureHook(ctx context.Context, forge WireForge, t target, hookURL, secret string, events []string, existing []Hook, dryRun bool, report *Report, label string) error {
	desired := Hook{URL: hookURL, Events: append([]string(nil), events...), Active: true}
	var matched *Hook
	for index := range existing {
		if strings.EqualFold(existing[index].URL, hookURL) {
			matched = &existing[index]
			break
		}
	}
	switch {
	case matched == nil:
		if dryRun {
			report.Created = append(report.Created, label)
			return nil
		}
		if t.repo == "" {
			if err := forge.CreateOrgWebhook(ctx, t.org, desired, secret); err != nil {
				return fmt.Errorf("create %s: %w", label, err)
			}
		} else {
			if err := forge.CreateRepoWebhook(ctx, t.org, t.repo, desired, secret); err != nil {
				return fmt.Errorf("create %s: %w", label, err)
			}
		}
		report.Created = append(report.Created, label)
	case !sameEvents(matched.Events, events) || !matched.Active:
		if dryRun {
			report.Updated = append(report.Updated, label)
			return nil
		}
		if t.repo == "" {
			if err := forge.UpdateOrgWebhook(ctx, t.org, matched.ID, desired, secret); err != nil {
				return fmt.Errorf("update %s: %w", label, err)
			}
		} else {
			if err := forge.UpdateRepoWebhook(ctx, t.org, t.repo, matched.ID, desired, secret); err != nil {
				return fmt.Errorf("update %s: %w", label, err)
			}
		}
		report.Updated = append(report.Updated, label)
	default:
		report.Unchanged = append(report.Unchanged, label)
	}
	return nil
}

func sameEvents(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	a := append([]string(nil), left...)
	b := append([]string(nil), right...)
	sort.Strings(a)
	sort.Strings(b)
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
