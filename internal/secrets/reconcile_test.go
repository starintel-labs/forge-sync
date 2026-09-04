package secrets_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/secrets"
)

type fakeForgejo struct {
	calls []secretCall
	err   error
}

type secretCall struct {
	owner string
	repo  string
	name  string
	value string
}

func (f *fakeForgejo) SetActionSecret(_ context.Context, owner, repo, name, value string) error {
	f.calls = append(f.calls, secretCall{owner: owner, repo: repo, name: name, value: value})
	return f.err
}

func TestReconcileCopiesConfiguredSecretToMappedForgejoRepository(t *testing.T) {
	fake := &fakeForgejo{}
	reconciler := secrets.New(fake, []model.ActionSecret{{
		Repository: "starintel-labs/example",
		Name:       "API_KEY",
		Value:      "existing-key",
	}})

	err := reconciler.Reconcile(context.Background(), model.RepositoryMapping{
		GitHubFullName: "starintel-labs/example",
		ForgejoOwner:   "nsaspy",
		ForgejoName:    "example",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(fake.calls))
	}
	if got := fake.calls[0]; got != (secretCall{owner: "nsaspy", repo: "example", name: "API_KEY", value: "existing-key"}) {
		t.Fatalf("call=%#v", got)
	}
}

func TestReconcileDryRunDoesNotWriteSecrets(t *testing.T) {
	fake := &fakeForgejo{}
	reconciler := secrets.New(fake, []model.ActionSecret{{
		Repository: "starintel-labs/example",
		Name:       "API_KEY",
		Value:      "existing-key",
	}})

	if err := reconciler.Reconcile(context.Background(), model.RepositoryMapping{GitHubFullName: "starintel-labs/example", ForgejoOwner: "nsaspy", ForgejoName: "example"}, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("dry-run calls=%d want 0", len(fake.calls))
	}
}

func TestReconcileIgnoresRepositoriesWithoutConfiguredSecrets(t *testing.T) {
	fake := &fakeForgejo{}
	reconciler := secrets.New(fake, []model.ActionSecret{{
		Repository: "starintel-labs/example",
		Name:       "API_KEY",
		Value:      "existing-key",
	}})

	if err := reconciler.Reconcile(context.Background(), model.RepositoryMapping{GitHubFullName: "starintel-labs/other", ForgejoOwner: "nsaspy", ForgejoName: "other"}, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("unconfigured calls=%d want 0", len(fake.calls))
	}
}

func TestReconcileDoesNotExposeSecretValueOnWriteFailure(t *testing.T) {
	fake := &fakeForgejo{err: errors.New("Forgejo unavailable")}
	reconciler := secrets.New(fake, []model.ActionSecret{{
		Repository: "starintel-labs/example",
		Name:       "API_KEY",
		Value:      "existing-key",
	}})

	err := reconciler.Reconcile(context.Background(), model.RepositoryMapping{GitHubFullName: "starintel-labs/example", ForgejoOwner: "nsaspy", ForgejoName: "example"}, false)
	if err == nil || !strings.Contains(err.Error(), "API_KEY") || strings.Contains(err.Error(), "existing-key") {
		t.Fatalf("error=%q", err)
	}
}
