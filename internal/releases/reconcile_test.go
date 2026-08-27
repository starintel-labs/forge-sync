package releases_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/releases"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type fakeReleases struct {
	items      []model.Release
	created    []model.Release
	updated    []model.Release
	downloaded []int64
	uploaded   []string
}

func (f *fakeReleases) ListReleases(context.Context, string, string) ([]model.Release, error) {
	return append([]model.Release(nil), f.items...), nil
}

func (f *fakeReleases) CreateRelease(_ context.Context, _, _ string, item model.Release) (model.Release, error) {
	f.created = append(f.created, item)
	item.ID = 1000 + int64(len(f.created))
	return item, nil
}

func (f *fakeReleases) UpdateRelease(_ context.Context, _, _ string, id int64, item model.Release) (model.Release, error) {
	item.ID = id
	f.updated = append(f.updated, item)
	return item, nil
}

func (f *fakeReleases) DownloadReleaseAsset(_ context.Context, _, _ string, releaseID, assetID int64) ([]byte, error) {
	f.downloaded = append(f.downloaded, assetID)
	return []byte("asset"), nil
}

func (f *fakeReleases) UploadReleaseAsset(_ context.Context, _, _ string, _ int64, name string, _ []byte) error {
	f.uploaded = append(f.uploaded, name)
	return nil
}

func TestGitHubReleaseMetadataCopiesToForgejoWithoutIDAssumption(t *testing.T) {
	t.Parallel()
	store, repository := releaseState(t)
	base := model.Release{ID: 31, Tag: "v1.0.0", Name: "v1.0.0", Body: "old"}
	if err := store.UpsertReleaseMapping(context.Background(), model.ReleaseMapping{
		RepositoryGitHubID: 1, GitHubID: 31, ForgejoID: 22, Tag: "v1.0.0",
		LastStateHash: releases.Hash(base), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	githubItem := base
	githubItem.Body = "new"
	forgejoItem := base
	forgejoItem.ID = 22
	github := &fakeReleases{items: []model.Release{githubItem}}
	forgejo := &fakeReleases{items: []model.Release{forgejoItem}}

	if err := releases.New(github, forgejo, store).Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.updated) != 1 || forgejo.updated[0].Body != "new" || len(github.updated) != 0 {
		t.Fatalf("github=%#v forgejo=%#v", github.updated, forgejo.updated)
	}
}

func TestConcurrentReleaseMetadataCreatesConflict(t *testing.T) {
	t.Parallel()
	store, repository := releaseState(t)
	base := model.Release{ID: 31, Tag: "v1.0.0", Name: "v1.0.0", Body: "old"}
	if err := store.UpsertReleaseMapping(context.Background(), model.ReleaseMapping{
		RepositoryGitHubID: 1, GitHubID: 31, ForgejoID: 22, Tag: "v1.0.0",
		LastStateHash: releases.Hash(base), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	githubItem := base
	githubItem.Body = "github"
	forgejoItem := base
	forgejoItem.ID, forgejoItem.Body = 22, "forgejo"
	github := &fakeReleases{items: []model.Release{githubItem}}
	forgejo := &fakeReleases{items: []model.Release{forgejoItem}}

	if err := releases.New(github, forgejo, store).Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(github.updated)+len(forgejo.updated) != 0 {
		t.Fatal("conflicting release was overwritten")
	}
	conflicts, err := store.ListConflicts(context.Background())
	if err != nil || len(conflicts) != 1 || conflicts[0].Kind != "release" {
		t.Fatalf("conflicts=%#v err=%v", conflicts, err)
	}
}

func TestAssetsAreEnsuredOnBothSidesWithoutDeletion(t *testing.T) {
	t.Parallel()
	store, repository := releaseState(t)
	githubItem := model.Release{ID: 31, Tag: "v1.0.0", Name: "v1.0.0", Assets: []model.ReleaseAsset{{ID: 91, Name: "shared.tgz"}, {ID: 92, Name: "github-only.tgz"}}}
	forgejoItem := model.Release{ID: 22, Tag: "v1.0.0", Name: "v1.0.0", Assets: []model.ReleaseAsset{{ID: 81, Name: "shared.tgz"}, {ID: 82, Name: "forgejo-only.tgz"}}}
	github := &fakeReleases{items: []model.Release{githubItem}}
	forgejo := &fakeReleases{items: []model.Release{forgejoItem}}

	if err := releases.New(github, forgejo, store).Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.uploaded) != 1 || forgejo.uploaded[0] != "github-only.tgz" {
		t.Fatalf("forgejo uploads=%v", forgejo.uploaded)
	}
	if len(github.uploaded) != 1 || github.uploaded[0] != "forgejo-only.tgz" {
		t.Fatalf("github uploads=%v", github.uploaded)
	}
	mappings, err := store.ListReleaseMappings(context.Background(), repository.GitHubID)
	if err != nil || len(mappings) != 1 || mappings[0].GitHubID != 31 || mappings[0].ForgejoID != 22 {
		t.Fatalf("mappings=%#v err=%v", mappings, err)
	}
}

func TestMissingReleaseSideRecordsConflictWithoutDeletion(t *testing.T) {
	t.Parallel()
	store, repository := releaseState(t)
	base := model.Release{ID: 31, Tag: "v1.0.0", Name: "v1.0.0"}
	if err := store.UpsertReleaseMapping(context.Background(), model.ReleaseMapping{
		RepositoryGitHubID: 1, GitHubID: 31, ForgejoID: 22, Tag: "v1.0.0",
		LastStateHash: releases.Hash(base), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	github := &fakeReleases{items: []model.Release{base}}
	forgejo := &fakeReleases{}

	if err := releases.New(github, forgejo, store).Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.created) != 0 || len(github.created) != 0 {
		t.Fatal("missing release was re-created instead of recorded")
	}
	conflicts, err := store.ListConflicts(context.Background())
	if err != nil || len(conflicts) != 1 || conflicts[0].Kind != "release-missing" {
		t.Fatalf("conflicts=%#v err=%v", conflicts, err)
	}
}

func TestRecoveryAfterCrashBetweenCreateAndMapping(t *testing.T) {
	t.Parallel()
	store, repository := releaseState(t)
	githubItem := model.Release{ID: 31, Tag: "v1.0.0", Name: "v1.0.0", Body: "notes"}
	github := &fakeReleases{items: []model.Release{githubItem}}
	forgejo := &fakeReleases{}

	if err := releases.New(github, forgejo, store).Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.created) != 1 {
		t.Fatalf("created=%d want 1", len(forgejo.created))
	}

	// Crash before the mapping write: fresh state, Forgejo lists the release.
	created := forgejo.created[0]
	created.ID = 1001
	crashStore, crashRepository := releaseStateAt(t, filepath.Join(t.TempDir(), "crash.db"))
	forgejoAfterCrash := &fakeReleases{items: []model.Release{created}}
	if err := releases.New(github, forgejoAfterCrash, crashStore).Reconcile(context.Background(), crashRepository, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejoAfterCrash.created) != 0 {
		t.Fatalf("recovery created %d duplicates", len(forgejoAfterCrash.created))
	}
	mappings, err := crashStore.ListReleaseMappings(context.Background(), crashRepository.GitHubID)
	if err != nil || len(mappings) != 1 || mappings[0].GitHubID != 31 || mappings[0].ForgejoID != 1001 {
		t.Fatalf("mappings=%#v err=%v", mappings, err)
	}
}

// Re-running reconciliation over unchanged state performs no writes at all.
func TestReconcileIsIdempotentOnUnchangedState(t *testing.T) {
	t.Parallel()
	store, repository := releaseState(t)
	githubItem := model.Release{ID: 31, Tag: "v1.0.0", Name: "v1.0.0", Body: "notes", Assets: []model.ReleaseAsset{{ID: 91, Name: "a.tgz"}}}
	github := &fakeReleases{items: []model.Release{githubItem}}
	forgejo := &fakeReleases{}

	first := releases.New(github, forgejo, store)
	if err := first.Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	created := forgejo.created[0]
	created.ID = 1001
	forgejo.items = []model.Release{created}
	forgejo.created, forgejo.updated, forgejo.uploaded, forgejo.downloaded = nil, nil, nil, nil

	if err := first.Reconcile(context.Background(), repository, false); err != nil {
		t.Fatal(err)
	}
	if len(forgejo.created) != 0 || len(github.created) != 0 || len(forgejo.updated) != 0 || len(github.updated) != 0 || len(forgejo.uploaded) != 0 || len(github.uploaded) != 0 {
		t.Fatalf("unchanged state produced writes")
	}
}

func releaseState(t *testing.T) (*state.Store, model.RepositoryMapping) {
	t.Helper()
	return releaseStateAt(t, filepath.Join(t.TempDir(), "state.db"))
}

func releaseStateAt(t *testing.T, path string) (*state.Store, model.RepositoryMapping) {
	t.Helper()
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := model.RepositoryMapping{
		GitHubID: 1, GitHubFullName: "starintel-labs/example", ForgejoOwner: "starintel-labs", ForgejoName: "example",
		Visibility: model.VisibilityPrivate, LastStateHash: "repository", UpdatedAt: time.Now().UTC(),
	}
	if err := store.UpsertRepository(context.Background(), repository); err != nil {
		t.Fatal(err)
	}
	return store, repository
}
