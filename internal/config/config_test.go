package config_test

import (
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/config"
)

// scrubEnv blanks every FORGE_SYNC variable so tests are hermetic against
// the direnv-managed runtime credentials exported into developer shells.
func scrubEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"FORGE_SYNC_GITHUB_API", "FORGE_SYNC_GITHUB_TOKEN",
		"FORGE_SYNC_FORGEJO_API", "FORGE_SYNC_FORGEJO_TOKEN",
		"FORGE_SYNC_GITHUB_WEBHOOK_SECRET", "FORGE_SYNC_FORGEJO_WEBHOOK_SECRET",
		"FORGE_SYNC_NAMESPACES", "FORGE_SYNC_STATE_PATH", "FORGE_SYNC_LISTEN_ADDR",
		"FORGE_SYNC_RECONCILE_INTERVAL", "FORGE_SYNC_REQUEST_TIMEOUT",
		"FORGE_SYNC_GIT_TIMEOUT", "FORGE_SYNC_MAX_CONCURRENCY",
		"FORGE_SYNC_MAX_WEBHOOK_BODY", "FORGE_SYNC_API_MAX_ATTEMPTS",
		"FORGE_SYNC_API_RETRY_BASE", "FORGE_SYNC_API_RETRY_MAX",
		"FORGE_SYNC_FORGEJO_OWNER_MAP",
	} {
		t.Setenv(key, "")
	}
}

func TestFromEnvironmentAppliesDefaultsAndValidates(t *testing.T) {
	scrubEnv(t)
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
	scrubEnv(t)
	t.Setenv("FORGE_SYNC_GITHUB_TOKEN", "gh")
	t.Setenv("FORGE_SYNC_FORGEJO_API", "https://forge.example.org")
	t.Setenv("FORGE_SYNC_FORGEJO_TOKEN", "fj")

	if _, err := config.FromEnvironment(); err == nil {
		t.Fatal("missing webhook secrets accepted")
	}
}

func TestFromEnvironmentRejectsForeignNamespace(t *testing.T) {
	scrubEnv(t)
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
	scrubEnv(t)
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

func TestOwnerMapParsesAndValidates(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		scrubEnv(t)
		t.Setenv("FORGE_SYNC_GITHUB_TOKEN", "gh")
		t.Setenv("FORGE_SYNC_FORGEJO_API", "https://forge.example.org")
		t.Setenv("FORGE_SYNC_FORGEJO_TOKEN", "fj")
		t.Setenv("FORGE_SYNC_GITHUB_WEBHOOK_SECRET", "ghs")
		t.Setenv("FORGE_SYNC_FORGEJO_WEBHOOK_SECRET", "fjs")
	}

	t.Run("valid map", func(t *testing.T) {
		base(t)
		t.Setenv("FORGE_SYNC_FORGEJO_OWNER_MAP", "starintel-labs:nsaspy, lost-rob0t:nsaspy")
		cfg, err := config.FromEnvironment()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ForgejoOwnerMap["starintel-labs"] != "nsaspy" || cfg.ForgejoOwnerMap["lost-rob0t"] != "nsaspy" {
			t.Fatalf("owner map=%v", cfg.ForgejoOwnerMap)
		}
	})
	t.Run("key outside namespaces rejected", func(t *testing.T) {
		base(t)
		t.Setenv("FORGE_SYNC_FORGEJO_OWNER_MAP", "someone-else:nsaspy")
		if _, err := config.FromEnvironment(); err == nil {
			t.Fatal("foreign owner map key accepted")
		}
	})
	t.Run("malformed target rejected", func(t *testing.T) {
		base(t)
		t.Setenv("FORGE_SYNC_FORGEJO_OWNER_MAP", "starintel-labs:nsaspy/x")
		if _, err := config.FromEnvironment(); err == nil {
			t.Fatal("malformed owner accepted")
		}
	})
}
