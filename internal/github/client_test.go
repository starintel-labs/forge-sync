package github_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/api"
	githubclient "github.com/starintel-labs/forge-sync/internal/github"
	"github.com/starintel-labs/forge-sync/internal/model"
)

func TestListRepositoriesPaginatesAndFailsClosedVisibility(t *testing.T) {
	t.Parallel()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", "<"+server.URL+"/orgs/starintel-labs/repos?per_page=100&page=2>; rel=\"next\"")
			_, _ = w.Write([]byte(`[{"id":1,"name":"one","full_name":"starintel-labs/one","clone_url":"https://github.com/starintel-labs/one.git","default_branch":"main","visibility":"public","archived":false,"updated_at":"2026-01-01T00:00:00Z"}]`))
		case "2":
			_, _ = w.Write([]byte(`[{"id":2,"name":"two","full_name":"starintel-labs/two","clone_url":"https://github.com/starintel-labs/two.git","default_branch":"main","visibility":"unexpected","private":false,"archived":false,"updated_at":"2026-01-02T00:00:00Z"}]`))
		default:
			t.Fatalf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	t.Cleanup(server.Close)

	client, err := githubclient.New(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := client.ListRepositories(context.Background(), "starintel-labs")
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 2 {
		t.Fatalf("got %d repositories, want 2", len(repositories))
	}
	if repositories[1].Visibility != model.VisibilityPrivate {
		t.Fatalf("unknown visibility became %q, want private", repositories[1].Visibility)
	}
}

func TestListRepositoriesFallsBackFromOrganizationToUser(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orgs/lost-rob0t/repos" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/users/lost-rob0t/repos" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	client, err := githubclient.New(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListRepositories(context.Background(), "lost-rob0t"); err != nil {
		t.Fatal(err)
	}
}

func TestListRepositoriesReturnsAPIFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	client, err := githubclient.New(server.URL, "token", time.Second, githubclient.WithRetry(fastRetries(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListRepositories(context.Background(), "starintel-labs"); err == nil {
		t.Fatal("API failure was treated as an empty repository list")
	}
}

func TestListRepositoriesRetriesRateLimitUntilSuccess(t *testing.T) {
	t.Parallel()
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`[{"id":7,"name":"x","full_name":"starintel-labs/x","clone_url":"https://github.com/starintel-labs/x.git","default_branch":"main","visibility":"private","private":true,"archived":false,"updated_at":"2026-01-01T00:00:00Z"}]`))
	}))
	t.Cleanup(server.Close)
	client, err := githubclient.New(server.URL, "token", time.Second, githubclient.WithRetry(fastRetries(t)))
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := client.ListRepositories(context.Background(), "starintel-labs")
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || repositories[0].Visibility != model.VisibilityPrivate {
		t.Fatalf("repositories=%#v", repositories)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d want 2", attempts)
	}
}

func TestListRepositoriesRetriesAreBounded(t *testing.T) {
	t.Parallel()
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	policy := fastRetries(t)
	client, err := githubclient.New(server.URL, "token", time.Second, githubclient.WithRetry(policy))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListRepositories(context.Background(), "starintel-labs"); err == nil {
		t.Fatal("persistent failure was swallowed")
	}
	if int(attempts) != policy.MaxAttempts {
		t.Fatalf("attempts=%d want %d", attempts, policy.MaxAttempts)
	}
}

func TestNonTransientFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	client, err := githubclient.New(server.URL, "token", time.Second, githubclient.WithRetry(fastRetries(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListRepositories(context.Background(), "starintel-labs"); err == nil {
		t.Fatal("authorization failure was swallowed")
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d want 1", attempts)
	}
}

func TestPullRequestClientRoundTrip(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/starintel-labs/example/pulls":
			_, _ = w.Write([]byte(`[{"id":11,"number":3,"title":"feature","body":"body","state":"open","draft":false,"merged":false,"updated_at":"2026-01-01T00:00:00Z","head":{"ref":"feature/x"},"base":{"ref":"main"}}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/starintel-labs/example/pulls":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["head"] != "feature/y" || payload["base"] != "main" || payload["draft"] != false {
				t.Fatalf("payload=%#v", payload)
			}
			_, _ = w.Write([]byte(`{"id":50,"number":9,"title":"feature","body":"body","state":"open","head":{"ref":"feature/y"},"base":{"ref":"main"}}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/starintel-labs/example/pulls/9":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["state"] != "closed" {
				t.Fatalf("payload=%#v", payload)
			}
			_, _ = w.Write([]byte(`{"id":50,"number":9,"title":"feature","body":"body","state":"closed","head":{"ref":"feature/y"},"base":{"ref":"main"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client, err := githubclient.New(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pullRequests, err := client.ListPullRequests(context.Background(), "starintel-labs", "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(pullRequests) != 1 || pullRequests[0].Head != "feature/x" || pullRequests[0].Base != "main" || pullRequests[0].Index != 3 {
		t.Fatalf("pullRequests=%#v", pullRequests)
	}
	created, err := client.CreatePullRequest(context.Background(), "starintel-labs", "example", model.PullRequest{Title: "feature", Body: "body", State: "open", Head: "feature/y", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 50 || created.Index != 9 {
		t.Fatalf("created=%#v", created)
	}
	closed := created
	closed.State = "closed"
	updated, err := client.UpdatePullRequest(context.Background(), "starintel-labs", "example", created.Index, closed)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "closed" {
		t.Fatalf("updated=%#v", updated)
	}
}

func TestReleaseClientRoundTripIncludingAssets(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/starintel-labs/example/releases":
			_, _ = w.Write([]byte(`[{"id":31,"tag_name":"v1.0.0","name":"v1.0.0","body":"notes","draft":false,"prerelease":false,"created_at":"2026-01-01T00:00:00Z","assets":[{"id":91,"name":"artifact.tgz","size":6}]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/starintel-labs/example/releases":
			_, _ = w.Write([]byte(`{"id":77,"tag_name":"v2.0.0","name":"v2.0.0","body":"notes","draft":false,"prerelease":false,"assets":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/starintel-labs/example/releases/assets/91" && r.Header.Get("Accept") == "application/octet-stream":
			_, _ = w.Write([]byte("digest"))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/repos/starintel-labs/example/releases/77/assets"):
			if r.URL.Query().Get("name") != "artifact.tgz" || r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("upload request query=%v content-type=%q", r.URL.Query(), r.Header.Get("Content-Type"))
			}
			content, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "digest" {
				t.Fatalf("content=%q", content)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client, err := githubclient.New(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	releases, err := client.ListReleases(context.Background(), "starintel-labs", "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 || releases[0].Tag != "v1.0.0" || len(releases[0].Assets) != 1 || releases[0].Assets[0].Name != "artifact.tgz" {
		t.Fatalf("releases=%#v", releases)
	}
	content, err := client.DownloadReleaseAsset(context.Background(), "starintel-labs", "example", 31, 91)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "digest" {
		t.Fatalf("content=%q", content)
	}
	created, err := client.CreateRelease(context.Background(), "starintel-labs", "example", model.Release{Tag: "v2.0.0", Name: "v2.0.0", Body: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 77 {
		t.Fatalf("created=%#v", created)
	}
	if err := client.UploadReleaseAsset(context.Background(), "starintel-labs", "example", created.ID, "artifact.tgz", content); err != nil {
		t.Fatal(err)
	}
}

func fastRetries(t *testing.T) api.RetryPolicy {
	t.Helper()
	return api.RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }}
}
