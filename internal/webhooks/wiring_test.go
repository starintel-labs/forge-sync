package webhooks_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/webhooks"
)

type fakeHooks struct {
	orgWebhooks   map[string][]webhooks.Hook
	repoWebhooks  map[string][]webhooks.Hook
	repositories  map[string][]model.Repository
	created       []string
	updated       []string
	secrets       map[string]string
	nextOrgHookID int64
}

func newFakeHooks(repositories map[string][]model.Repository) *fakeHooks {
	return &fakeHooks{
		orgWebhooks:   map[string][]webhooks.Hook{},
		repoWebhooks:  map[string][]webhooks.Hook{},
		repositories:  repositories,
		created:       []string{},
		updated:       []string{},
		secrets:       map[string]string{},
		nextOrgHookID: 100,
	}
}

func (f *fakeHooks) ListOrgWebhooks(_ context.Context, org string) ([]webhooks.Hook, bool, error) {
	hooks, found := f.orgWebhooks[org]
	return append([]webhooks.Hook(nil), hooks...), found, nil
}

func (f *fakeHooks) CreateOrgWebhook(_ context.Context, org string, hook webhooks.Hook, secret string) error {
	f.nextOrgHookID++
	hook.ID = f.nextOrgHookID
	f.orgWebhooks[org] = append(f.orgWebhooks[org], hook)
	f.secrets[fmt.Sprintf("org/%s/%d", org, hook.ID)] = secret
	f.created = append(f.created, "org/"+org)
	return nil
}

func (f *fakeHooks) UpdateOrgWebhook(_ context.Context, org string, id int64, hook webhooks.Hook, secret string) error {
	for index := range f.orgWebhooks[org] {
		if f.orgWebhooks[org][index].ID == id {
			f.orgWebhooks[org][index] = hook
			f.orgWebhooks[org][index].ID = id
			f.secrets[fmt.Sprintf("org/%s/%d", org, id)] = secret
			f.updated = append(f.updated, "org/"+org)
			return nil
		}
	}
	return fmt.Errorf("hook %d not found on org %s", id, org)
}

func (f *fakeHooks) ListRepoWebhooks(_ context.Context, owner, name string) ([]webhooks.Hook, error) {
	return append([]webhooks.Hook(nil), f.repoWebhooks[owner+"/"+name]...), nil
}

func (f *fakeHooks) CreateRepoWebhook(_ context.Context, owner, name string, hook webhooks.Hook, secret string) error {
	key := owner + "/" + name
	hook.ID = int64(len(f.repoWebhooks[key]) + 1)
	f.repoWebhooks[key] = append(f.repoWebhooks[key], hook)
	f.secrets[key+"/"+fmt.Sprint(hook.ID)] = secret
	f.created = append(f.created, "repo/"+key)
	return nil
}

func (f *fakeHooks) UpdateRepoWebhook(_ context.Context, owner, name string, id int64, hook webhooks.Hook, secret string) error {
	key := owner + "/" + name
	for index := range f.repoWebhooks[key] {
		if f.repoWebhooks[key][index].ID == id {
			f.repoWebhooks[key][index] = hook
			f.updated = append(f.updated, "repo/"+key)
			return nil
		}
	}
	return fmt.Errorf("hook %d not found on repo %s", id, key)
}

func (f *fakeHooks) ListRepositories(_ context.Context, namespace string) ([]model.Repository, error) {
	return append([]model.Repository(nil), f.repositories[namespace]...), nil
}

var _ webhooks.WireForge = (*fakeHooks)(nil)

func githubHookHasEvents(hook webhooks.Hook) bool {
	for _, event := range []string{"push", "repository", "issues", "issue_comment", "pull_request", "release"} {
		found := false
		for _, candidate := range hook.Events {
			if strings.EqualFold(candidate, event) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func wiringFor(t *testing.T, github, forgejo *fakeHooks, namespaces ...string) *webhooks.Wiring {
	t.Helper()
	return webhooks.NewWiring(github, forgejo, namespaces)
}

func TestWireCreatesOrgHookForOrgNamespace(t *testing.T) {
	t.Parallel()
	github := newFakeHooks(map[string][]model.Repository{})
	forgejo := newFakeHooks(map[string][]model.Repository{})
	github.orgWebhooks["starintel-labs"] = []webhooks.Hook{}
	forgejo.orgWebhooks["starintel-labs"] = []webhooks.Hook{}

	report, err := wiringFor(t, github, forgejo, "starintel-labs").Wire(
		context.Background(), "https://example.invalid/webhooks/github", "http://127.0.0.1:8080/webhooks/forgejo",
		"gh-secret", "fj-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 2 || len(report.Updated) != 0 || len(report.Skipped) != 0 {
		t.Fatalf("unexpected report: %#v", report)
	}
	hooks, _ := github.orgWebhooks["starintel-labs"]
	if len(hooks) != 1 || hooks[0].URL != "https://example.invalid/webhooks/github" {
		t.Fatalf("github org hook missing: %#v", hooks)
	}
	if !githubHookHasEvents(hooks[0]) {
		t.Fatalf("github hook lacks the documented events: %#v", hooks[0])
	}
	if got := github.secrets["org/starintel-labs/101"]; got != "gh-secret" {
		t.Fatalf("github webhook secret not applied: %q", got)
	}
	hooks, _ = forgejo.orgWebhooks["starintel-labs"]
	if len(hooks) != 1 || hooks[0].URL != "http://127.0.0.1:8080/webhooks/forgejo" {
		t.Fatalf("forgejo org hook missing: %#v", hooks)
	}
}

func TestWireUserNamespaceWiresPerRepository(t *testing.T) {
	t.Parallel()
	repositories := map[string][]model.Repository{
		"lost-rob0t": {{Owner: "lost-rob0t", Name: "zara", FullName: "lost-rob0t/zara"}},
	}
	github := newFakeHooks(repositories)
	forgejo := newFakeHooks(map[string][]model.Repository{})

	if _, err := wiringFor(t, github, forgejo, "lost-rob0t").Wire(
		context.Background(), "https://example.invalid/webhooks/github", "http://127.0.0.1:8080/webhooks/forgejo",
		"gh-secret", "fj-secret", false); err != nil {
		t.Fatal(err)
	}
	if len(github.created) != 1 || github.created[0] != "repo/lost-rob0t/zara" {
		t.Fatalf("github created=%v", github.created)
	}
	hooks := github.repoWebhooks["lost-rob0t/zara"]
	if len(hooks) != 1 || hooks[0].URL != "https://example.invalid/webhooks/github" || !strings.EqualFold(hooks[0].Events[0], "push") {
		t.Fatalf("repo hook missing: %#v", hooks)
	}
}

func TestWireIsIdempotent(t *testing.T) {
	t.Parallel()
	repositories := map[string][]model.Repository{}
	github := newFakeHooks(repositories)
	forgejo := newFakeHooks(map[string][]model.Repository{})
	github.orgWebhooks["starintel-labs"] = []webhooks.Hook{}
	forgejo.orgWebhooks["starintel-labs"] = []webhooks.Hook{}

	wiring := wiringFor(t, github, forgejo, "starintel-labs")
	if _, err := wiring.Wire(context.Background(), "https://example.invalid/webhooks/github", "http://127.0.0.1:8080/webhooks/forgejo", "s1", "s2", false); err != nil {
		t.Fatal(err)
	}
	second, err := wiring.Wire(context.Background(), "https://example.invalid/webhooks/github", "http://127.0.0.1:8080/webhooks/forgejo", "s1", "s2", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Created) != 0 || len(second.Updated) != 0 || len(second.Unchanged) != 2 {
		t.Fatalf("second run not idempotent: %#v", second)
	}
}

func TestWireUpdatesDriftedHook(t *testing.T) {
	t.Parallel()
	github := newFakeHooks(map[string][]model.Repository{})
	github.orgWebhooks["starintel-labs"] = []webhooks.Hook{{
		ID: 7, URL: "https://example.invalid/webhooks/github", Events: []string{"push"}, Active: false,
	}}

	report, err := wiringFor(t, github, newFakeHooks(map[string][]model.Repository{}), "starintel-labs").Wire(
		context.Background(), "https://example.invalid/webhooks/github", "", "s", "s", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Updated) != 1 || report.Updated[0] != "github:org/starintel-labs" {
		t.Fatalf("drift not repaired: %#v", report)
	}
}

func TestWireLeavesForeignHooksAlone(t *testing.T) {
	t.Parallel()
	github := newFakeHooks(map[string][]model.Repository{})
	foreign := webhooks.Hook{ID: 5, URL: "https://other.invalid/hook", Events: []string{"push"}, Active: true}
	github.orgWebhooks["starintel-labs"] = []webhooks.Hook{foreign}

	if _, err := wiringFor(t, github, newFakeHooks(map[string][]model.Repository{}), "starintel-labs").Wire(
		context.Background(), "https://example.invalid/webhooks/github", "", "s", "", false); err != nil {
		t.Fatal(err)
	}
	if len(github.orgWebhooks["starintel-labs"]) != 2 || github.orgWebhooks["starintel-labs"][0].URL != "https://other.invalid/hook" {
		t.Fatalf("foreign hook was mutated: %#v", github.orgWebhooks["starintel-labs"])
	}
}

func TestWireDryRunNeverMutates(t *testing.T) {
	t.Parallel()
	github := newFakeHooks(map[string][]model.Repository{})
	forgejo := newFakeHooks(map[string][]model.Repository{})
	github.orgWebhooks["starintel-labs"] = []webhooks.Hook{}
	forgejo.orgWebhooks["starintel-labs"] = []webhooks.Hook{}

	report, err := wiringFor(t, github, forgejo, "starintel-labs").Wire(
		context.Background(), "https://example.invalid/webhooks/github", "http://127.0.0.1:8080/webhooks/forgejo", "s", "s", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 2 || len(github.created) != 0 || len(forgejo.created) != 0 {
		t.Fatalf("dry run mutated or misreported: %#v", report)
	}
}

func TestWireSkipsUnconfiguredGitHubURL(t *testing.T) {
	t.Parallel()
	github := newFakeHooks(map[string][]model.Repository{})
	report, err := wiringFor(t, github, newFakeHooks(map[string][]model.Repository{}), "starintel-labs").Wire(
		context.Background(), "", "http://127.0.0.1:8080/webhooks/forgejo", "s", "s", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], "github") {
		t.Fatalf("expected github skip: %#v", report.Skipped)
	}
	if len(github.created) != 0 {
		t.Fatalf("github mutated despite missing URL: %v", github.created)
	}
}
