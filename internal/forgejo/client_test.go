package forgejo_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/forgejo"
	"github.com/starintel-labs/forge-sync/internal/model"
)

func TestMigrateRepositoryRequestsSupportedMetadata(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/migrate" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "token forgejo-token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"issues", "labels", "milestones", "pull_requests", "releases", "wiki"} {
			if payload[key] != true {
				t.Errorf("%s = %#v, want true", key, payload[key])
			}
		}
		if payload["mirror"] != false || payload["private"] != true || payload["service"] != "github" {
			t.Fatalf("unsafe migration payload: %#v", payload)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":9,"name":"alpha","full_name":"starintel-labs/alpha","private":true}`))
	}))
	t.Cleanup(server.Close)
	client, err := forgejo.New(server.URL, "forgejo-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	repo := model.Repository{ID: 7, Owner: "starintel-labs", Name: "alpha", FullName: "starintel-labs/alpha", CloneURL: "https://github.com/starintel-labs/alpha.git", Visibility: model.VisibilityPrivate}
	if _, err := client.MigrateRepository(context.Background(), repo, "github-token"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateUnknownVisibilityIsPrivate(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Private bool `json:"private"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if !payload.Private {
			t.Fatal("unknown visibility did not fail closed to private")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":9,"name":"alpha","full_name":"starintel-labs/alpha","private":true}`))
	}))
	t.Cleanup(server.Close)
	client, err := forgejo.New(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	repo := model.Repository{Owner: "starintel-labs", Name: "alpha", CloneURL: "https://github.com/starintel-labs/alpha.git", Visibility: model.Visibility("unknown")}
	if _, err := client.MigrateRepository(context.Background(), repo, "github-token"); err != nil {
		t.Fatal(err)
	}
}
