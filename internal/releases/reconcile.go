package releases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/state"
)

type Forge interface {
	ListReleases(context.Context, string, string) ([]model.Release, error)
	CreateRelease(context.Context, string, string, model.Release) (model.Release, error)
	UpdateRelease(context.Context, string, string, int64, model.Release) (model.Release, error)
	DownloadReleaseAsset(context.Context, string, string, int64, int64) ([]byte, error)
	UploadReleaseAsset(context.Context, string, string, int64, string, []byte) error
}

type Reconciler struct {
	github       Forge
	forgejo      Forge
	store        *state.Store
	maxAssetSize int64
}

// Option customizes the release reconciler.
type Option func(*Reconciler)

// WithMaxAssetSize bounds asset synchronization: assets larger than the
// limit are recorded as skipped instead of being buffered into memory.
// Zero or negative disables the bound.
func WithMaxAssetSize(limit int64) Option {
	return func(r *Reconciler) { r.maxAssetSize = limit }
}

func New(github, forgejo Forge, store *state.Store, options ...Option) *Reconciler {
	if github == nil || forgejo == nil || store == nil {
		panic("release reconciler requires both forges and state")
	}
	reconciler := &Reconciler{github: github, forgejo: forgejo, store: store}
	for _, option := range options {
		option(reconciler)
	}
	return reconciler
}

func (r *Reconciler) Reconcile(ctx context.Context, repository model.RepositoryMapping, dryRun bool) error {
	githubOwner, githubName, ok := strings.Cut(repository.GitHubFullName, "/")
	if !ok || githubOwner == "" || githubName == "" || repository.ForgejoOwner == "" || repository.ForgejoName == "" {
		return errors.New("repository mapping has invalid identity")
	}
	githubReleases, err := r.github.ListReleases(ctx, githubOwner, githubName)
	if err != nil {
		return fmt.Errorf("list GitHub releases: %w", err)
	}
	forgejoReleases, err := r.forgejo.ListReleases(ctx, repository.ForgejoOwner, repository.ForgejoName)
	if err != nil {
		return fmt.Errorf("list Forgejo releases: %w", err)
	}
	if err := validateReleases(githubReleases); err != nil {
		return fmt.Errorf("invalid GitHub release response: %w", err)
	}
	if err := validateReleases(forgejoReleases); err != nil {
		return fmt.Errorf("invalid Forgejo release response: %w", err)
	}
	mappings, err := r.store.ListReleaseMappings(ctx, repository.GitHubID)
	if err != nil {
		return err
	}
	githubByID := byID(githubReleases)
	forgejoByID := byID(forgejoReleases)
	consumedGitHub := map[int64]bool{}
	consumedForgejo := map[int64]bool{}

	for _, mapping := range mappings {
		githubRelease, githubFound := githubByID[mapping.GitHubID]
		forgejoRelease, forgejoFound := forgejoByID[mapping.ForgejoID]
		if !githubFound || !forgejoFound {
			// A mapped release that vanished from one side is never deleted or
			// recreated: the survivor is pinned to the mapping and the operator
			// resolves the conflict explicitly.
			if githubFound {
				consumedGitHub[githubRelease.ID] = true
			}
			if forgejoFound {
				consumedForgejo[forgejoRelease.ID] = true
			}
			if !dryRun {
				if err := r.store.AddConflict(ctx, model.Conflict{
					Kind: "release-missing", Repository: repository.GitHubFullName,
					ObjectKey:   fmt.Sprintf("github:%d/forgejo:%d", mapping.GitHubID, mapping.ForgejoID),
					GitHubState: stateOrMissing(githubRelease, githubFound), ForgejoState: stateOrMissing(forgejoRelease, forgejoFound),
					LastKnownState: mapping.LastStateHash, CreatedAt: time.Now().UTC(),
				}); err != nil {
					return err
				}
			}
			continue
		}
		consumedGitHub[githubRelease.ID] = true
		consumedForgejo[forgejoRelease.ID] = true
		githubHash, forgejoHash := Hash(githubRelease), Hash(forgejoRelease)
		switch {
		case githubHash == forgejoHash:
			if !dryRun && mapping.LastStateHash != githubHash {
				mapping.LastStateHash, mapping.UpdatedAt = githubHash, time.Now().UTC()
				if err := r.store.UpsertReleaseMapping(ctx, mapping); err != nil {
					return err
				}
			}
		case forgejoHash == mapping.LastStateHash:
			if dryRun {
				continue
			}
			updated, err := r.forgejo.UpdateRelease(ctx, repository.ForgejoOwner, repository.ForgejoName, forgejoRelease.ID, githubRelease)
			if err != nil {
				return fmt.Errorf("copy GitHub release %d to Forgejo: %w", githubRelease.ID, err)
			}
			mapping.LastStateHash, mapping.UpdatedAt = githubHash, time.Now().UTC()
			if err := r.store.UpsertReleaseMapping(ctx, mapping); err != nil {
				return err
			}
			forgejoRelease = updated
		case githubHash == mapping.LastStateHash:
			if dryRun {
				continue
			}
			updated, err := r.github.UpdateRelease(ctx, githubOwner, githubName, githubRelease.ID, forgejoRelease)
			if err != nil {
				return fmt.Errorf("copy Forgejo release %d to GitHub: %w", forgejoRelease.ID, err)
			}
			mapping.LastStateHash, mapping.UpdatedAt = forgejoHash, time.Now().UTC()
			if err := r.store.UpsertReleaseMapping(ctx, mapping); err != nil {
				return err
			}
			githubRelease = updated
		default:
			if dryRun {
				continue
			}
			if err := r.store.AddConflict(ctx, model.Conflict{
				Kind: "release", Repository: repository.GitHubFullName,
				ObjectKey:   fmt.Sprintf("github:%d/forgejo:%d", mapping.GitHubID, mapping.ForgejoID),
				GitHubState: githubHash, ForgejoState: forgejoHash, LastKnownState: mapping.LastStateHash,
				CreatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
		}
		if !dryRun {
			if err := r.reconcileAssets(ctx, githubOwner, githubName, repository, githubRelease, forgejoRelease); err != nil {
				return err
			}
		}
	}

	unmappedGitHub := unconsumed(githubReleases, consumedGitHub)
	unmappedForgejo := unconsumed(forgejoReleases, consumedForgejo)
	pairedGitHub, _ := uniqueHashPairs(unmappedGitHub, unmappedForgejo)
	for githubID, forgejoID := range pairedGitHub {
		githubRelease, forgejoRelease := githubByID[githubID], forgejoByID[forgejoID]
		consumedGitHub[githubID], consumedForgejo[forgejoID] = true, true
		if !dryRun {
			if err := r.store.UpsertReleaseMapping(ctx, releaseMapping(repository.GitHubID, githubRelease, forgejoRelease)); err != nil {
				return err
			}
			if err := r.reconcileAssets(ctx, githubOwner, githubName, repository, githubRelease, forgejoRelease); err != nil {
				return err
			}
		}
	}

	// Migration rewrites release bodies; tags are stable identity. Pair the
	// remainder by unique tag, recording the Forgejo state as baseline.
	tagGitHub, _ := tagPairs(unconsumed(githubReleases, consumedGitHub), unconsumed(forgejoReleases, consumedForgejo))
	for githubID, forgejoID := range tagGitHub {
		githubRelease, forgejoRelease := githubByID[githubID], forgejoByID[forgejoID]
		consumedGitHub[githubID], consumedForgejo[forgejoID] = true, true
		if !dryRun {
			mapping := releaseMapping(repository.GitHubID, githubRelease, forgejoRelease)
			mapping.LastStateHash = Hash(forgejoRelease)
			if err := r.store.UpsertReleaseMapping(ctx, mapping); err != nil {
				return err
			}
		}
	}

	for _, githubRelease := range unconsumed(githubReleases, consumedGitHub) {
		if dryRun {
			continue
		}
		created, err := r.forgejo.CreateRelease(ctx, repository.ForgejoOwner, repository.ForgejoName, githubRelease)
		if err != nil {
			return fmt.Errorf("create Forgejo release from GitHub release %d: %w", githubRelease.ID, err)
		}
		if err := validateRelease(created); err != nil {
			return fmt.Errorf("invalid created Forgejo release: %w", err)
		}
		if err := r.store.UpsertReleaseMapping(ctx, releaseMapping(repository.GitHubID, githubRelease, created)); err != nil {
			return err
		}
		if err := r.reconcileAssets(ctx, githubOwner, githubName, repository, githubRelease, created); err != nil {
			return err
		}
	}
	for _, forgejoRelease := range unconsumed(forgejoReleases, consumedForgejo) {
		if dryRun {
			continue
		}
		created, err := r.github.CreateRelease(ctx, githubOwner, githubName, forgejoRelease)
		if err != nil {
			return fmt.Errorf("create GitHub release from Forgejo release %d: %w", forgejoRelease.ID, err)
		}
		if err := validateRelease(created); err != nil {
			return fmt.Errorf("invalid created GitHub release: %w", err)
		}
		if err := r.store.UpsertReleaseMapping(ctx, releaseMapping(repository.GitHubID, created, forgejoRelease)); err != nil {
			return err
		}
		if err := r.reconcileAssets(ctx, githubOwner, githubName, repository, created, forgejoRelease); err != nil {
			return err
		}
	}
	return nil
}

// reconcileAssets ensures assets present on either side exist by name on both
// sides. Assets are never deleted: an asset that only exists on one side is
// uploaded to the other, and manual additions are preserved.
func (r *Reconciler) reconcileAssets(ctx context.Context, githubOwner, githubName string, repository model.RepositoryMapping, githubRelease, forgejoRelease model.Release) error {
	if githubRelease.ID <= 0 || forgejoRelease.ID <= 0 {
		return nil
	}
	forgejoAssets := map[string]bool{}
	for _, asset := range forgejoRelease.Assets {
		forgejoAssets[asset.Name] = true
	}
	githubAssets := map[string]bool{}
	for _, asset := range githubRelease.Assets {
		githubAssets[asset.Name] = true
	}
	for _, asset := range githubRelease.Assets {
		if forgejoAssets[asset.Name] {
			continue
		}
		if r.exceedsAssetLimit(ctx, repository, githubRelease.Tag, asset, "github", "forgejo") {
			continue
		}
		content, err := r.github.DownloadReleaseAsset(ctx, githubOwner, githubName, githubRelease.ID, asset.ID)
		if err != nil {
			return fmt.Errorf("download GitHub asset %q: %w", asset.Name, err)
		}
		if err := r.forgejo.UploadReleaseAsset(ctx, repository.ForgejoOwner, repository.ForgejoName, forgejoRelease.ID, asset.Name, content); err != nil {
			return fmt.Errorf("upload asset %q to Forgejo: %w", asset.Name, err)
		}
	}
	for _, asset := range forgejoRelease.Assets {
		if githubAssets[asset.Name] {
			continue
		}
		if r.exceedsAssetLimit(ctx, repository, forgejoRelease.Tag, asset, "forgejo", "github") {
			continue
		}
		content, err := r.forgejo.DownloadReleaseAsset(ctx, repository.ForgejoOwner, repository.ForgejoName, forgejoRelease.ID, asset.ID)
		if err != nil {
			return fmt.Errorf("download Forgejo asset %q: %w", asset.Name, err)
		}
		if err := r.github.UploadReleaseAsset(ctx, githubOwner, githubName, githubRelease.ID, asset.Name, content); err != nil {
			return fmt.Errorf("upload asset %q to GitHub: %w", asset.Name, err)
		}
	}
	return nil
}

// exceedsAssetLimit records a durable conflict for an asset too large to
// buffer and reports whether it must be skipped. Oversized assets are never
// downloaded, so a multi-gigabyte release artifact cannot stall or abort a
// reconciliation cycle.
func (r *Reconciler) exceedsAssetLimit(ctx context.Context, repository model.RepositoryMapping, tag string, asset model.ReleaseAsset, from, to string) bool {
	if r.maxAssetSize <= 0 || asset.Size <= r.maxAssetSize {
		return false
	}
	if err := r.store.AddConflict(ctx, model.Conflict{
		Kind: "release-asset-skipped", Repository: repository.GitHubFullName,
		ObjectKey:      tag + "/" + asset.Name,
		GitHubState:    fmt.Sprintf("%s %d bytes", from, asset.Size),
		ForgejoState:   fmt.Sprintf("limit %d bytes", r.maxAssetSize),
		LastKnownState: to + "-copy-skipped", CreatedAt: time.Now().UTC(),
	}); err != nil {
		return false
	}
	return true
}

func Hash(release model.Release) string {
	state := struct {
		Tag        string `json:"tag"`
		Name       string `json:"name"`
		Body       string `json:"body"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}{release.Tag, release.Name, release.Body, release.Draft, release.Prerelease}
	encoded, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func validateReleases(all []model.Release) error {
	seen := map[int64]bool{}
	for _, release := range all {
		if err := validateRelease(release); err != nil {
			return err
		}
		if seen[release.ID] {
			return fmt.Errorf("duplicate release ID %d", release.ID)
		}
		seen[release.ID] = true
	}
	return nil
}

func validateRelease(release model.Release) error {
	if release.ID <= 0 || release.Tag == "" {
		return errors.New("release identity or tag is empty")
	}
	return nil
}

func byID(all []model.Release) map[int64]model.Release {
	result := make(map[int64]model.Release, len(all))
	for _, release := range all {
		result[release.ID] = release
	}
	return result
}

func unconsumed(all []model.Release, consumed map[int64]bool) []model.Release {
	var result []model.Release
	for _, release := range all {
		if !consumed[release.ID] {
			result = append(result, release)
		}
	}
	return result
}

func uniqueHashPairs(githubReleases, forgejoReleases []model.Release) (map[int64]int64, map[int64]int64) {
	githubByHash := map[string][]int64{}
	forgejoByHash := map[string][]int64{}
	for _, release := range githubReleases {
		githubByHash[Hash(release)] = append(githubByHash[Hash(release)], release.ID)
	}
	for _, release := range forgejoReleases {
		forgejoByHash[Hash(release)] = append(forgejoByHash[Hash(release)], release.ID)
	}
	githubPairs := map[int64]int64{}
	forgejoPairs := map[int64]int64{}
	for hash, githubIDs := range githubByHash {
		forgejoIDs := forgejoByHash[hash]
		if len(githubIDs) == 1 && len(forgejoIDs) == 1 {
			githubPairs[githubIDs[0]] = forgejoIDs[0]
			forgejoPairs[forgejoIDs[0]] = githubIDs[0]
		}
	}
	return githubPairs, forgejoPairs
}

// tagPairs pairs leftovers by unique tag name; see the issues reconciler for
// why migrations require this recovery pass.
func tagPairs(githubReleases, forgejoReleases []model.Release) (map[int64]int64, map[int64]int64) {
	githubByTag := map[string][]model.Release{}
	forgejoByTag := map[string][]model.Release{}
	for _, release := range githubReleases {
		githubByTag[release.Tag] = append(githubByTag[release.Tag], release)
	}
	for _, release := range forgejoReleases {
		forgejoByTag[release.Tag] = append(forgejoByTag[release.Tag], release)
	}
	githubPairs := map[int64]int64{}
	forgejoPairs := map[int64]int64{}
	for tag, githubMatches := range githubByTag {
		forgejoMatches := forgejoByTag[tag]
		if len(githubMatches) != 1 || len(forgejoMatches) != 1 {
			continue
		}
		githubPairs[githubMatches[0].ID] = forgejoMatches[0].ID
		forgejoPairs[forgejoMatches[0].ID] = githubMatches[0].ID
	}
	return githubPairs, forgejoPairs
}

func releaseMapping(repositoryID int64, githubRelease, forgejoRelease model.Release) model.ReleaseMapping {
	return model.ReleaseMapping{
		RepositoryGitHubID: repositoryID, GitHubID: githubRelease.ID, ForgejoID: forgejoRelease.ID,
		Tag: githubRelease.Tag, LastStateHash: Hash(githubRelease), UpdatedAt: time.Now().UTC(),
	}
}

func stateOrMissing(release model.Release, found bool) string {
	if !found {
		return "missing"
	}
	return Hash(release) + ":" + strconv.FormatInt(release.ID, 10)
}
