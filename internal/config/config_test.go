package config_test

import (
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/config"
)

func TestFromEnvironmentAppliesDefaultsAndValidates(t *testing.T) {
	t.Setenv("FORGE_SYNC_GITHUB_TOKEN", "gh")
	t.Setenv("FORGE_SYNC_FORGEJO_API", "https://forge.example.org/")
	t.Setenv("FORGE_SYNC_FORGEJO_TOKEN", "fj")
	t.Setenv("FORGE_SYNC_GITHUB_WEBHOOK_SECRET", "ghs")
	t.Setenv("FORGE_SYNC_FORGEJO_WEBHOOK_SECRET", "fjs")

	cfg, err := config.FromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ForgejoAPI != "https://forge.example.org" {
		t.Fatalf("ForgejoAPI=%q", cfg.ForgejoAPI)
	}
	if len(cfg.Namespaces) != 2 || cfg.Namespaces[0] != "starintel-labs" || cfg.Namespaces[1] != "lost-rob0t" {
		t.Fatalf("Namespaces=%v", cfg.Namespaces)
	}
	if cfg.APIRetry.MaxAttempts != 4 || cfg.APIRetry.BaseDelay != time.Second || cfg.APIRetry.MaxDelay != 30*time.Second {
		t.Fatalf("APIRetry=%#v", cfg.APIRetry)
	}
	if cfg.APIRetry.Sleep == nil {
		t.Fatal("retry sleep must default to the real sleeper")
	}
}

func TestFromEnvironmentRejectsMissingSecrets(t *testing.T) {
	t.Setenv("FORGE_SYNC_GITHUB_TOKEN", "gh")
	t.Setenv("FORGE_SYNC_FORGEJO_API", "https://forge.example.org")
	t.Setenv("FORGE_SYNC_FORGEJO_TOKEN", "fj")

	if _, err := config.FromEnvironment(); err == nil {
		t.Fatal("missing webhook secrets accepted")
	}
}

func TestFromEnvironmentRejectsForeignNamespace(t *testing.T) {
	t.Setenv("FORGE_SYNC_GITHUB_TOKEN", "gh")
	t.Setenv("FORGE_SYNC_FORGEJO_API", "https://forge.example.org")
	t.Setenv("FORGE_SYNC_FORGEJO_TOKEN", "fj")
	t.Setenv("FORGE_SYNC_GITHUB_WEBHOOK_SECRET", "ghs")
	t.Setenv("FORGE_SYNC_FORGEJO_WEBHOOK_SECRET", "fjs")
	t.Setenv("FORGE_SYNC_NAMESPACES", "starintel-labs,someone-else")

	if _, err := config.FromEnvironment(); err == nil {
		t.Fatal("foreign namespace accepted")
	}
}

func TestFromEnvironmentRejectsInvalidRetryPolicy(t *testing.T) {
	t.Setenv("FORGE_SYNC_GITHUB_TOKEN", "gh")
	t.Setenv("FORGE_SYNC_FORGEJO_API", "https://forge.example.org")
	t.Setenv("FORGE_SYNC_FORGEJO_TOKEN", "fj")
	t.Setenv("FORGE_SYNC_GITHUB_WEBHOOK_SECRET", "ghs")
	t.Setenv("FORGE_SYNC_FORGEJO_WEBHOOK_SECRET", "fjs")
	t.Setenv("FORGE_SYNC_API_MAX_ATTEMPTS", "0")

	if _, err := config.FromEnvironment(); err == nil {
		t.Fatal("zero retry attempts accepted")
	}
}
