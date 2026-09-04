package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/starintel-labs/forge-sync/internal/comments"
	"github.com/starintel-labs/forge-sync/internal/config"
	"github.com/starintel-labs/forge-sync/internal/forgejo"
	"github.com/starintel-labs/forge-sync/internal/github"
	"github.com/starintel-labs/forge-sync/internal/gitrefs"
	"github.com/starintel-labs/forge-sync/internal/issues"
	"github.com/starintel-labs/forge-sync/internal/pullrequests"
	"github.com/starintel-labs/forge-sync/internal/reconcile"
	"github.com/starintel-labs/forge-sync/internal/releases"
	"github.com/starintel-labs/forge-sync/internal/repository"
	"github.com/starintel-labs/forge-sync/internal/secrets"
	"github.com/starintel-labs/forge-sync/internal/state"
	"github.com/starintel-labs/forge-sync/internal/webhooks"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("forge-sync failed", "error", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return usageError()
	}
	cfg, err := config.FromEnvironment()
	if err != nil {
		return err
	}
	runtime, err := openRuntime(cfg)
	if err != nil {
		return err
	}
	defer runtime.store.Close()
	ctx := context.Background()
	switch arguments[0] {
	case "status":
		if len(arguments) != 1 {
			return usageError()
		}
		stats, err := runtime.store.Stats(ctx)
		if err != nil {
			return err
		}
		return writeJSON(stats)
	case "bootstrap":
		flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
		dryRun := flags.Bool("dry-run", false, "inventory without forge mutations")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return usageError()
		}
		report, err := runtime.engine.Reconcile(ctx, "all", *dryRun)
		printInventory(report)
		return err
	case "discover":
		if len(arguments) != 1 {
			return usageError()
		}
		inventory, err := runtime.engine.Discover(ctx, false)
		printInventory(reconcile.Report{Inventory: inventory})
		return err
	case "reconcile":
		if len(arguments) > 2 {
			return usageError()
		}
		scope := "all"
		if len(arguments) == 2 {
			scope = arguments[1]
		}
		report, err := runtime.engine.Reconcile(ctx, scope, false)
		if writeErr := writeJSON(report); writeErr != nil && err == nil {
			err = writeErr
		}
		return err
	case "inspect":
		if len(arguments) != 2 {
			return usageError()
		}
		return inspect(ctx, runtime.store, arguments[1])
	case "conflicts":
		if len(arguments) != 1 {
			return usageError()
		}
		conflicts, err := runtime.store.ListConflicts(ctx)
		if err != nil {
			return err
		}
		return writeJSON(conflicts)
	case "wire-webhooks":
		flags := flag.NewFlagSet("wire-webhooks", flag.ContinueOnError)
		dryRun := flags.Bool("dry-run", false, "report planned webhook changes without any mutation")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return usageError()
		}
		return wireWebhooks(ctx, cfg, runtime, *dryRun)
	case "serve":
		if len(arguments) != 1 {
			return usageError()
		}
		return serve(cfg, runtime)
	default:
		return usageError()
	}
}

type application struct {
	store   *state.Store
	engine  *reconcile.Engine
	github  *github.Client
	forgejo *forgejo.Client
}

func openRuntime(cfg config.Config) (*application, error) {
	store, err := state.Open(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	githubClient, err := github.New(cfg.GitHubAPI, cfg.GitHubToken, cfg.RequestTimeout, github.WithRetry(cfg.APIRetry), github.WithPacing(cfg.GitHubMinInterval))
	if err != nil {
		store.Close()
		return nil, err
	}
	forgejoClient, err := forgejo.New(cfg.ForgejoAPI, cfg.ForgejoToken, cfg.RequestTimeout, forgejo.WithRetry(cfg.APIRetry), forgejo.WithPacing(cfg.ForgejoMinInterval))
	if err != nil {
		store.Close()
		return nil, err
	}
	repositories := repository.New(githubClient, forgejoClient, store, cfg.GitHubToken, cfg.ForgejoOwnerMap)
	issueReconciler := issues.New(githubClient, forgejoClient, store)
	commentReconciler := comments.New(githubClient, forgejoClient, store)
	pullRequestReconciler := pullrequests.New(githubClient, forgejoClient, store)
	releaseReconciler := releases.New(githubClient, forgejoClient, store, releases.WithMaxAssetSize(cfg.MaxAssetSizeBytes))
	actionSecretReconciler := secrets.New(forgejoClient, cfg.ActionSecrets)
	gitSynchronizer := gitrefs.NewSynchronizer(store, cfg.GitTimeout)
	engine := reconcile.New(
		repositories, issueReconciler, commentReconciler, pullRequestReconciler, releaseReconciler, actionSecretReconciler, gitSynchronizer, store,
		cfg.Namespaces, cfg.GitHubToken, cfg.ForgejoToken, cfg.ForgejoAPI, cfg.MaxConcurrency, cfg.MaxRefSizeKB,
	)
	return &application{store: store, engine: engine, github: githubClient, forgejo: forgejoClient}, nil
}

func wireWebhooks(ctx context.Context, cfg config.Config, runtime *application, dryRun bool) error {
	wiring := webhooks.NewWiring(runtime.github, runtime.forgejo, cfg.Namespaces)
	report, err := wiring.Wire(ctx, cfg.GitHubWebhookURL, cfg.ForgejoWebhookURL, cfg.GitHubWebhookSecret, cfg.ForgejoWebhookSecret, dryRun)
	if err != nil {
		return err
	}
	return writeJSON(report)
}

func serve(cfg config.Config, runtime *application) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	webhookServer := webhooks.New(runtime.store, runtime.engine, cfg.GitHubWebhookSecret, cfg.ForgejoWebhookSecret, cfg.MaxWebhookBody)
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", webhookServer)
	mux.Handle("/webhooks/forgejo", webhookServer)
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			response.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		checkCtx, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		if err := runtime.store.Ping(checkCtx); err != nil {
			http.Error(response, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	mux.HandleFunc("/metrics", func(response http.ResponseWriter, request *http.Request) {
		stats, err := runtime.store.Stats(request.Context())
		if err != nil {
			http.Error(response, "metrics unavailable", http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintf(response,
			"forge_sync_repositories %d\nforge_sync_issues %d\nforge_sync_pull_requests %d\nforge_sync_comments %d\nforge_sync_releases %d\nforge_sync_conflicts %d\nforge_sync_webhook_deliveries %d\nforge_sync_reconciliation_runs %d\n",
			stats.Repositories, stats.Issues, stats.PullRequests, stats.Comments, stats.Releases, stats.Conflicts, stats.Deliveries, stats.Runs)
	})
	server := &http.Server{
		Addr: cfg.ListenAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: cfg.GitTimeout + 2*cfg.RequestTimeout,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
	}
	scheduler := reconcile.NewScheduler(runtime.engine, cfg.ReconcileInterval)
	go scheduler.Run(ctx, func(err error) { slog.Error("periodic reconciliation failed", "error", err) })
	go func() {
		if _, err := runtime.engine.Reconcile(ctx, "all", false); err != nil && ctx.Err() == nil {
			slog.Error("startup reconciliation failed", "error", err)
		}
	}()
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("forge-sync listening", "address", cfg.ListenAddr)
		serverErrors <- server.ListenAndServe()
	}()
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
	return nil
}

func inspect(ctx context.Context, store *state.Store, fullName string) error {
	mappings, err := store.ListRepositories(ctx)
	if err != nil {
		return err
	}
	for _, mapping := range mappings {
		if !strings.EqualFold(mapping.GitHubFullName, fullName) {
			continue
		}
		issueMappings, err := store.ListIssueMappings(ctx, mapping.GitHubID)
		if err != nil {
			return err
		}
		conflicts, err := store.ListConflicts(ctx)
		if err != nil {
			return err
		}
		var matching []any
		for _, conflict := range conflicts {
			if strings.EqualFold(conflict.Repository, fullName) {
				matching = append(matching, conflict)
			}
		}
		return writeJSON(map[string]any{"repository": mapping, "issues": issueMappings, "conflicts": matching})
	}
	return fmt.Errorf("repository %q is not mapped", fullName)
}

func printInventory(report reconcile.Report) {
	fmt.Printf("GitHub repositories: %d\nForgejo repositories: %d\nMissing: %d\nIn sync: %d\nConflicted: %d\n",
		report.Inventory.GitHubRepositories, report.Inventory.ForgejoRepositories,
		report.Inventory.Missing, report.Inventory.InSync, report.Inventory.Conflicted)
}

func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func usageError() error {
	return errors.New("usage: forge-sync {status|bootstrap [--dry-run]|discover|reconcile [owner/repo]|inspect owner/repo|conflicts|wire-webhooks [--dry-run]|serve}")
}
