package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	client, err := githubclient.New(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListRepositories(context.Background(), "starintel-labs"); err == nil {
		t.Fatal("API failure was treated as an empty repository list")
	}
}
