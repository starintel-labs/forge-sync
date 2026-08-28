package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/starintel-labs/forge-sync/internal/api"
)

const (
	defaultGitHubAPI  = "https://api.github.com"
	defaultListenAddr = "127.0.0.1:8080"
)

type Config struct {
	GitHubAPI            string
	GitHubToken          string
	ForgejoAPI           string
	ForgejoToken         string
	GitHubWebhookSecret  string
	ForgejoWebhookSecret string
	Namespaces           []string
	StatePath            string
	ListenAddr           string
	ReconcileInterval    time.Duration
	RequestTimeout       time.Duration
	GitTimeout           time.Duration
	MaxConcurrency       int
	MaxWebhookBody       int64
	APIRetry             api.RetryPolicy
	ForgejoOwnerMap      map[string]string
}

func FromEnvironment() (Config, error) {
	cfg := Config{
		GitHubAPI:            valueOr("FORGE_SYNC_GITHUB_API", defaultGitHubAPI),
		GitHubToken:          os.Getenv("FORGE_SYNC_GITHUB_TOKEN"),
		ForgejoAPI:           strings.TrimRight(os.Getenv("FORGE_SYNC_FORGEJO_API"), "/"),
		ForgejoToken:         os.Getenv("FORGE_SYNC_FORGEJO_TOKEN"),
		GitHubWebhookSecret:  os.Getenv("FORGE_SYNC_GITHUB_WEBHOOK_SECRET"),
		ForgejoWebhookSecret: os.Getenv("FORGE_SYNC_FORGEJO_WEBHOOK_SECRET"),
		Namespaces:           splitList(valueOr("FORGE_SYNC_NAMESPACES", "starintel-labs,lost-rob0t")),
		StatePath:            valueOr("FORGE_SYNC_STATE_PATH", "/var/lib/forge-sync/forge-sync.db"),
		ListenAddr:           valueOr("FORGE_SYNC_LISTEN_ADDR", defaultListenAddr),
		ReconcileInterval:    durationOr("FORGE_SYNC_RECONCILE_INTERVAL", 5*time.Minute),
		RequestTimeout:       durationOr("FORGE_SYNC_REQUEST_TIMEOUT", 30*time.Second),
		GitTimeout:           durationOr("FORGE_SYNC_GIT_TIMEOUT", 5*time.Minute),
		MaxConcurrency:       intOr("FORGE_SYNC_MAX_CONCURRENCY", 4),
		MaxWebhookBody:       int64Or("FORGE_SYNC_MAX_WEBHOOK_BODY", 1<<20),
		APIRetry: api.RetryPolicy{
			MaxAttempts: intOr("FORGE_SYNC_API_MAX_ATTEMPTS", 4),
			BaseDelay:   durationOr("FORGE_SYNC_API_RETRY_BASE", time.Second),
			MaxDelay:    durationOr("FORGE_SYNC_API_RETRY_MAX", 30*time.Second),
			Sleep:       api.Sleep,
		},
		ForgejoOwnerMap: ownerMapOr("FORGE_SYNC_FORGEJO_OWNER_MAP"),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var missing []string
	for name, value := range map[string]string{
		"FORGE_SYNC_GITHUB_TOKEN":           c.GitHubToken,
		"FORGE_SYNC_FORGEJO_API":            c.ForgejoAPI,
		"FORGE_SYNC_FORGEJO_TOKEN":          c.ForgejoToken,
		"FORGE_SYNC_GITHUB_WEBHOOK_SECRET":  c.GitHubWebhookSecret,
		"FORGE_SYNC_FORGEJO_WEBHOOK_SECRET": c.ForgejoWebhookSecret,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	if len(c.Namespaces) == 0 {
		return errors.New("at least one namespace is required")
	}
	for _, namespace := range c.Namespaces {
		if namespace != "starintel-labs" && namespace != "lost-rob0t" {
			return fmt.Errorf("namespace %q is outside the allowed set", namespace)
		}
	}
	if c.MaxConcurrency < 1 || c.MaxConcurrency > 32 {
		return errors.New("max concurrency must be between 1 and 32")
	}
	if c.MaxWebhookBody < 1024 || c.MaxWebhookBody > 16<<20 {
		return errors.New("max webhook body must be between 1 KiB and 16 MiB")
	}
	if c.ReconcileInterval < time.Minute {
		return errors.New("reconcile interval must be at least one minute")
	}
	if c.RequestTimeout <= 0 || c.GitTimeout <= 0 {
		return errors.New("request and Git timeouts must be positive")
	}
	if err := c.APIRetry.Validate(); err != nil {
		return fmt.Errorf("API retry policy: %w", err)
	}
	for namespace, owner := range c.ForgejoOwnerMap {
		if !contains(c.Namespaces, namespace) {
			return fmt.Errorf("owner map key %q is outside the configured namespaces", namespace)
		}
		if owner == "" || strings.ContainsAny(owner, "/ \t") {
			return fmt.Errorf("owner map target for %q is invalid", namespace)
		}
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func ownerMapOr(key string) map[string]string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return nil
	}
	result := map[string]string{}
	for _, pair := range strings.Split(value, ",") {
		namespace, owner, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		result[strings.TrimSpace(namespace)] = strings.TrimSpace(owner)
	}
	return result
}

func valueOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func splitList(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func durationOr(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return -1
	}
	return parsed
}

func intOr(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return value
}

func int64Or(key string, fallback int64) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64)
	if err != nil {
		return fallback
	}
	return value
}
