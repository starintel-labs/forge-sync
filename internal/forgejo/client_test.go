package forgejo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/api"
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

func TestListPullRequestsPaginatesAndMapsRefs(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/starintel-labs/example/pulls" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"id":11,"index":3,"title":"one","body":"","state":"open","merged":false,"head":{"ref":"feature/one"},"base":{"ref":"main"}},{"id":12,"index":4,"title":"two","body":"","state":"closed","merged":true,"head":{"ref":"feature/two"},"base":{"ref":"main"}}]`))
	}))
	t.Cleanup(server.Close)
	client, err := forgejo.New(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pullRequests, err := client.ListPullRequests(context.Background(), "starintel-labs", "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(pullRequests) != 2 {
		t.Fatalf("pullRequests=%d want 2", len(pullRequests))
	}
	if pullRequests[0].Head != "feature/one" || pullRequests[0].Index != 3 {
		t.Fatalf("pullRequests[0]=%#v", pullRequests[0])
	}
	if !pullRequests[1].Merged || pullRequests[1].State != "closed" {
		t.Fatalf("pullRequests[1]=%#v", pullRequests[1])
	}
}

func TestListPullRequestsPaginatesUntilShortPage(t *testing.T) {
	t.Parallel()
	var pages int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := atomic.AddInt32(&pages, 1)
		if page == 1 {
			batch := make([]string, 50)
			for i := range batch {
				batch[i] = `{"id":` + fmt.Sprint(i+1) + `,"index":` + fmt.Sprint(i+1) + `,"title":"p","state":"open","head":{"ref":"feature/x"},"base":{"ref":"main"}}`
			}
			_, _ = w.Write([]byte(`[` + strings.Join(batch, ",") + `]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	client, err := forgejo.New(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pullRequests, err := client.ListPullRequests(context.Background(), "starintel-labs", "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(pullRequests) != 50 || pages != 2 {
		t.Fatalf("pullRequests=%d pages=%d", len(pullRequests), pages)
	}
}

func TestCreateAndUpdatePullRequest(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/starintel-labs/example/pulls":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["head"] != "feature/z" || payload["base"] != "main" {
				t.Fatalf("payload=%#v", payload)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":60,"index":10,"title":"z","state":"open","head":{"ref":"feature/z"},"base":{"ref":"main"}}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/starintel-labs/example/pulls/10":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["state"] != "closed" {
				t.Fatalf("payload=%#v", payload)
			}
			_, _ = w.Write([]byte(`{"id":60,"index":10,"title":"z","state":"closed","head":{"ref":"feature/z"},"base":{"ref":"main"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client, err := forgejo.New(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.CreatePullRequest(context.Background(), "starintel-labs", "example", model.PullRequest{Title: "z", State: "open", Head: "feature/z", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 60 || created.Index != 10 {
		t.Fatalf("created=%#v", created)
	}
	updated, err := client.UpdatePullRequest(context.Background(), "starintel-labs", "example", 10, model.PullRequest{Title: "z", State: "closed", Head: "feature/z", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "closed" {
		t.Fatalf("updated=%#v", updated)
	}
}

func TestReleaseRoundTripIncludingAssetUpload(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/starintel-labs/example/releases":
			_, _ = w.Write([]byte(`[{"id":31,"tag_name":"v1.0.0","name":"v1.0.0","body":"notes","draft":false,"prerelease":false,"attachments":[{"id":41,"name":"build.tgz","size":3}]}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/starintel-labs/example/releases/31/assets/41":
			_, _ = w.Write([]byte("bin"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/starintel-labs/example/releases":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":80,"tag_name":"v2.0.0","name":"v2.0.0","attachments":[]}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/repos/starintel-labs/example/releases/80/assets"):
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			file, header, err := r.FormFile("attachment")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			content, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			if header.Filename != "build.tgz" || string(content) != "bin" {
				t.Fatalf("upload=%q content=%q", header.Filename, content)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":81}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client, err := forgejo.New(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	releases, err := client.ListReleases(context.Background(), "starintel-labs", "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 || releases[0].Tag != "v1.0.0" || len(releases[0].Assets) != 1 {
		t.Fatalf("releases=%#v", releases)
	}
	content, err := client.DownloadReleaseAsset(context.Background(), "starintel-labs", "example", 31, 41)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "bin" {
		t.Fatalf("content=%q", content)
	}
	created, err := client.CreateRelease(context.Background(), "starintel-labs", "example", model.Release{Tag: "v2.0.0", Name: "v2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 80 {
		t.Fatalf("created=%#v", created)
	}
	if err := client.UploadReleaseAsset(context.Background(), "starintel-labs", "example", 80, "build.tgz", content); err != nil {
		t.Fatal(err)
	}
}

func TestForgejoRetriesServerErrors(t *testing.T) {
	t.Parallel()
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	client, err := forgejo.New(server.URL, "token", time.Second, forgejo.WithRetry(api.RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListReleases(context.Background(), "starintel-labs", "example"); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d want 3", attempts)
	}
}
