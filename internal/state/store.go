package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/migrations"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Stats struct {
	Repositories int
	Issues       int
	PullRequests int
	Comments     int
	Releases     int
	Conflicts    int
	Deliveries   int
	Runs         int
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("state path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite state: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure sqlite state: %w", err)
	}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	queries := []struct {
		name  string
		query string
		value *int
	}{
		{"repositories", `SELECT COUNT(*) FROM repository_mappings`, nil},
		{"issues", `SELECT COUNT(*) FROM issue_mappings`, nil},
		{"pull_requests", `SELECT COUNT(*) FROM pull_request_mappings`, nil},
		{"comments", `SELECT COUNT(*) FROM comment_mappings`, nil},
		{"releases", `SELECT COUNT(*) FROM release_mappings`, nil},
		{"conflicts", `SELECT COUNT(*) FROM conflicts WHERE resolved_at IS NULL`, nil},
		{"deliveries", `SELECT COUNT(*) FROM webhook_deliveries`, nil},
		{"runs", `SELECT COUNT(*) FROM reconciliation_runs`, nil},
	}
	var stats Stats
	queries[0].value = &stats.Repositories
	queries[1].value = &stats.Issues
	queries[2].value = &stats.PullRequests
	queries[3].value = &stats.Comments
	queries[4].value = &stats.Releases
	queries[5].value = &stats.Conflicts
	queries[6].value = &stats.Deliveries
	queries[7].value = &stats.Runs
	for _, item := range queries {
		if err := s.db.QueryRowContext(ctx, item.query).Scan(item.value); err != nil {
			return Stats{}, fmt.Errorf("count %s: %w", item.name, err)
		}
	}
	return stats, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return fmt.Errorf("migration %q has no numeric prefix", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return fmt.Errorf("migration %q has invalid version: %w", entry.Name(), err)
		}
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %d: %w", version, err)
		}
		if exists != 0 {
			continue
		}
		body, err := fs.ReadFile(migrations.Files, entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %d: %w", version, err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, now()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
	}
	return nil
}

func (s *Store) UpsertRepository(ctx context.Context, mapping model.RepositoryMapping) error {
	if mapping.GitHubID <= 0 || mapping.GitHubFullName == "" || mapping.ForgejoOwner == "" || mapping.ForgejoName == "" {
		return errors.New("repository mapping is incomplete")
	}
	if mapping.UpdatedAt.IsZero() {
		mapping.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO repository_mappings
    (github_id, github_full_name, forgejo_owner, forgejo_name, visibility, archived, last_state_hash, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(github_id) DO UPDATE SET
    github_full_name = excluded.github_full_name,
    forgejo_owner = excluded.forgejo_owner,
    forgejo_name = excluded.forgejo_name,
    visibility = excluded.visibility,
    archived = excluded.archived,
    last_state_hash = excluded.last_state_hash,
    updated_at = excluded.updated_at`,
		mapping.GitHubID, mapping.GitHubFullName, mapping.ForgejoOwner, mapping.ForgejoName,
		mapping.Visibility, boolInt(mapping.Archived), mapping.LastStateHash, mapping.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert repository mapping: %w", err)
	}
	return nil
}

func (s *Store) RepositoryByGitHubID(ctx context.Context, githubID int64) (model.RepositoryMapping, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT github_id, github_full_name, forgejo_owner, forgejo_name, visibility, archived, last_state_hash, updated_at
FROM repository_mappings WHERE github_id = ?`, githubID)
	mapping, err := scanRepository(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.RepositoryMapping{}, false, nil
	}
	if err != nil {
		return model.RepositoryMapping{}, false, fmt.Errorf("get repository mapping: %w", err)
	}
	return mapping, true, nil
}

func (s *Store) RepositoriesByForgejoPath(ctx context.Context, owner, name string) ([]model.RepositoryMapping, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT github_id, github_full_name, forgejo_owner, forgejo_name, visibility, archived, last_state_hash, updated_at
FROM repository_mappings WHERE lower(forgejo_owner) = lower(?) AND lower(forgejo_name) = lower(?) ORDER BY github_full_name`, owner, name)
	if err != nil {
		return nil, fmt.Errorf("find repository mappings by Forgejo path: %w", err)
	}
	defer rows.Close()
	var result []model.RepositoryMapping
	for rows.Next() {
		mapping, err := scanRepository(rows)
		if err != nil {
			return nil, fmt.Errorf("scan repository mapping: %w", err)
		}
		result = append(result, mapping)
	}
	return result, rows.Err()
}

func (s *Store) ListRepositories(ctx context.Context) ([]model.RepositoryMapping, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT github_id, github_full_name, forgejo_owner, forgejo_name, visibility, archived, last_state_hash, updated_at
FROM repository_mappings ORDER BY github_full_name`)
	if err != nil {
		return nil, fmt.Errorf("list repository mappings: %w", err)
	}
	defer rows.Close()
	var result []model.RepositoryMapping
	for rows.Next() {
		mapping, err := scanRepository(rows)
		if err != nil {
			return nil, fmt.Errorf("scan repository mapping: %w", err)
		}
		result = append(result, mapping)
	}
	return result, rows.Err()
}

func (s *Store) UpsertIssueMapping(ctx context.Context, mapping model.IssueMapping) error {
	if mapping.RepositoryGitHubID <= 0 || mapping.GitHubID <= 0 || mapping.ForgejoID <= 0 || mapping.GitHubIndex <= 0 || mapping.ForgejoIndex <= 0 || mapping.LastStateHash == "" {
		return errors.New("issue mapping is incomplete")
	}
	if mapping.UpdatedAt.IsZero() {
		mapping.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO issue_mappings
    (repository_github_id, github_id, forgejo_id, github_index, forgejo_index, last_state_hash, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repository_github_id, github_id) DO UPDATE SET
    forgejo_id = excluded.forgejo_id,
    github_index = excluded.github_index,
    forgejo_index = excluded.forgejo_index,
    last_state_hash = excluded.last_state_hash,
    updated_at = excluded.updated_at`,
		mapping.RepositoryGitHubID, mapping.GitHubID, mapping.ForgejoID, mapping.GitHubIndex,
		mapping.ForgejoIndex, mapping.LastStateHash, mapping.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert issue mapping: %w", err)
	}
	return nil
}

func (s *Store) ListIssueMappings(ctx context.Context, repositoryGitHubID int64) ([]model.IssueMapping, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT repository_github_id, github_id, forgejo_id, github_index, forgejo_index, last_state_hash, updated_at
FROM issue_mappings WHERE repository_github_id = ? ORDER BY github_id`, repositoryGitHubID)
	if err != nil {
		return nil, fmt.Errorf("list issue mappings: %w", err)
	}
	defer rows.Close()
	var result []model.IssueMapping
	for rows.Next() {
		var mapping model.IssueMapping
		var updated string
		if err := rows.Scan(&mapping.RepositoryGitHubID, &mapping.GitHubID, &mapping.ForgejoID, &mapping.GitHubIndex, &mapping.ForgejoIndex, &mapping.LastStateHash, &updated); err != nil {
			return nil, fmt.Errorf("scan issue mapping: %w", err)
		}
		mapping.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, fmt.Errorf("parse issue mapping time: %w", err)
		}
		result = append(result, mapping)
	}
	return result, rows.Err()
}

func (s *Store) UpsertPullRequestMapping(ctx context.Context, mapping model.PullRequestMapping) error {
	if mapping.RepositoryGitHubID <= 0 || mapping.GitHubID <= 0 || mapping.ForgejoID <= 0 || mapping.GitHubIndex <= 0 || mapping.ForgejoIndex <= 0 || mapping.LastStateHash == "" {
		return errors.New("pull request mapping is incomplete")
	}
	if mapping.UpdatedAt.IsZero() {
		mapping.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO pull_request_mappings
    (repository_github_id, github_id, forgejo_id, github_index, forgejo_index, last_state_hash, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repository_github_id, github_id) DO UPDATE SET
    forgejo_id = excluded.forgejo_id,
    github_index = excluded.github_index,
    forgejo_index = excluded.forgejo_index,
    last_state_hash = excluded.last_state_hash,
    updated_at = excluded.updated_at`,
		mapping.RepositoryGitHubID, mapping.GitHubID, mapping.ForgejoID, mapping.GitHubIndex,
		mapping.ForgejoIndex, mapping.LastStateHash, mapping.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert pull request mapping: %w", err)
	}
	return nil
}

func (s *Store) ListPullRequestMappings(ctx context.Context, repositoryGitHubID int64) ([]model.PullRequestMapping, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT repository_github_id, github_id, forgejo_id, github_index, forgejo_index, last_state_hash, updated_at
FROM pull_request_mappings WHERE repository_github_id = ? ORDER BY github_id`, repositoryGitHubID)
	if err != nil {
		return nil, fmt.Errorf("list pull request mappings: %w", err)
	}
	defer rows.Close()
	var result []model.PullRequestMapping
	for rows.Next() {
		var mapping model.PullRequestMapping
		var updated string
		if err := rows.Scan(&mapping.RepositoryGitHubID, &mapping.GitHubID, &mapping.ForgejoID, &mapping.GitHubIndex, &mapping.ForgejoIndex, &mapping.LastStateHash, &updated); err != nil {
			return nil, fmt.Errorf("scan pull request mapping: %w", err)
		}
		mapping.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, fmt.Errorf("parse pull request mapping time: %w", err)
		}
		result = append(result, mapping)
	}
	return result, rows.Err()
}

func (s *Store) UpsertCommentMapping(ctx context.Context, mapping model.CommentMapping) error {
	if mapping.RepositoryGitHubID <= 0 || mapping.IssueGitHubID <= 0 || mapping.GitHubID <= 0 || mapping.ForgejoID <= 0 || mapping.LastStateHash == "" {
		return errors.New("comment mapping is incomplete")
	}
	if mapping.UpdatedAt.IsZero() {
		mapping.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO comment_mappings
    (repository_github_id, issue_github_id, github_id, forgejo_id, last_state_hash, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(repository_github_id, github_id) DO UPDATE SET
    issue_github_id = excluded.issue_github_id,
    forgejo_id = excluded.forgejo_id,
    last_state_hash = excluded.last_state_hash,
    updated_at = excluded.updated_at`,
		mapping.RepositoryGitHubID, mapping.IssueGitHubID, mapping.GitHubID, mapping.ForgejoID,
		mapping.LastStateHash, mapping.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert comment mapping: %w", err)
	}
	return nil
}

func (s *Store) ListCommentMappings(ctx context.Context, repositoryGitHubID, issueGitHubID int64) ([]model.CommentMapping, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT repository_github_id, issue_github_id, github_id, forgejo_id, last_state_hash, updated_at
FROM comment_mappings WHERE repository_github_id = ? AND issue_github_id = ? ORDER BY github_id`, repositoryGitHubID, issueGitHubID)
	if err != nil {
		return nil, fmt.Errorf("list comment mappings: %w", err)
	}
	defer rows.Close()
	var result []model.CommentMapping
	for rows.Next() {
		var mapping model.CommentMapping
		var updated string
		if err := rows.Scan(&mapping.RepositoryGitHubID, &mapping.IssueGitHubID, &mapping.GitHubID, &mapping.ForgejoID, &mapping.LastStateHash, &updated); err != nil {
			return nil, fmt.Errorf("scan comment mapping: %w", err)
		}
		mapping.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, fmt.Errorf("parse comment mapping time: %w", err)
		}
		result = append(result, mapping)
	}
	return result, rows.Err()
}

type scanner interface {
	Scan(...any) error
}

func scanRepository(row scanner) (model.RepositoryMapping, error) {
	var mapping model.RepositoryMapping
	var visibility string
	var archived int
	var updated string
	if err := row.Scan(&mapping.GitHubID, &mapping.GitHubFullName, &mapping.ForgejoOwner, &mapping.ForgejoName, &visibility, &archived, &mapping.LastStateHash, &updated); err != nil {
		return model.RepositoryMapping{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return model.RepositoryMapping{}, fmt.Errorf("parse mapping time: %w", err)
	}
	mapping.Visibility = model.Visibility(visibility)
	mapping.Archived = archived != 0
	mapping.UpdatedAt = parsed
	return mapping, nil
}

func (s *Store) UpsertReleaseMapping(ctx context.Context, mapping model.ReleaseMapping) error {
	if mapping.RepositoryGitHubID <= 0 || mapping.GitHubID <= 0 || mapping.ForgejoID <= 0 || mapping.Tag == "" || mapping.LastStateHash == "" {
		return errors.New("release mapping is incomplete")
	}
	if mapping.UpdatedAt.IsZero() {
		mapping.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO release_mappings
    (repository_github_id, github_id, forgejo_id, tag, last_state_hash, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(repository_github_id, github_id) DO UPDATE SET
    forgejo_id = excluded.forgejo_id,
    tag = excluded.tag,
    last_state_hash = excluded.last_state_hash,
    updated_at = excluded.updated_at`,
		mapping.RepositoryGitHubID, mapping.GitHubID, mapping.ForgejoID, mapping.Tag,
		mapping.LastStateHash, mapping.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert release mapping: %w", err)
	}
	return nil
}

func (s *Store) ListReleaseMappings(ctx context.Context, repositoryGitHubID int64) ([]model.ReleaseMapping, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT repository_github_id, github_id, forgejo_id, tag, last_state_hash, updated_at
FROM release_mappings WHERE repository_github_id = ? ORDER BY github_id`, repositoryGitHubID)
	if err != nil {
		return nil, fmt.Errorf("list release mappings: %w", err)
	}
	defer rows.Close()
	var result []model.ReleaseMapping
	for rows.Next() {
		var mapping model.ReleaseMapping
		var updated string
		if err := rows.Scan(&mapping.RepositoryGitHubID, &mapping.GitHubID, &mapping.ForgejoID, &mapping.Tag, &mapping.LastStateHash, &updated); err != nil {
			return nil, fmt.Errorf("scan release mapping: %w", err)
		}
		mapping.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, fmt.Errorf("parse release mapping time: %w", err)
		}
		result = append(result, mapping)
	}
	return result, rows.Err()
}

func (s *Store) ClaimWebhookDelivery(ctx context.Context, forge, deliveryID, eventType, payloadHash string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
INSERT INTO webhook_deliveries(forge, delivery_id, event_type, payload_hash, received_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(forge, delivery_id) DO NOTHING`, forge, deliveryID, eventType, payloadHash, now())
	if err != nil {
		return false, fmt.Errorf("claim webhook delivery: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count claimed deliveries: %w", err)
	}
	return count == 1, nil
}

func (s *Store) MarkWebhookProcessed(ctx context.Context, forge, deliveryID string, processErr error) error {
	var errorText any
	if processErr != nil {
		errorText = processErr.Error()
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE webhook_deliveries SET processed_at = ?, error = ? WHERE forge = ? AND delivery_id = ?`, now(), errorText, forge, deliveryID)
	if err != nil {
		return fmt.Errorf("mark webhook processed: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("webhook delivery was not claimed")
	}
	return nil
}

func (s *Store) AddConflict(ctx context.Context, conflict model.Conflict) error {
	if conflict.CreatedAt.IsZero() {
		conflict.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO conflicts(kind, repository, object_key, github_state, forgejo_state, last_known_state, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(kind, repository, object_key, github_state, forgejo_state) DO NOTHING`,
		conflict.Kind, conflict.Repository, conflict.ObjectKey, conflict.GitHubState, conflict.ForgejoState,
		conflict.LastKnownState, conflict.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("add conflict: %w", err)
	}
	return nil
}

func (s *Store) ListConflicts(ctx context.Context) ([]model.Conflict, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT kind, repository, object_key, github_state, forgejo_state, last_known_state, created_at
FROM conflicts WHERE resolved_at IS NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list conflicts: %w", err)
	}
	defer rows.Close()
	var result []model.Conflict
	for rows.Next() {
		var conflict model.Conflict
		var created string
		if err := rows.Scan(&conflict.Kind, &conflict.Repository, &conflict.ObjectKey, &conflict.GitHubState, &conflict.ForgejoState, &conflict.LastKnownState, &created); err != nil {
			return nil, fmt.Errorf("scan conflict: %w", err)
		}
		conflict.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse conflict time: %w", err)
		}
		result = append(result, conflict)
	}
	return result, rows.Err()
}

func (s *Store) BeginRun(ctx context.Context, scope string, dryRun bool) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
INSERT INTO reconciliation_runs(scope, dry_run, started_at, status) VALUES (?, ?, ?, 'running')`, scope, boolInt(dryRun), now())
	if err != nil {
		return 0, fmt.Errorf("begin reconciliation run: %w", err)
	}
	return result.LastInsertId()
}

func (s *Store) CompleteRun(ctx context.Context, id int64, summary string, runErr error) error {
	status := "succeeded"
	var errorText any
	if runErr != nil {
		status = "failed"
		errorText = runErr.Error()
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE reconciliation_runs SET completed_at = ?, status = ?, summary_json = ?, error = ? WHERE id = ? AND status = 'running'`,
		now(), status, summary, errorText, id)
	if err != nil {
		return fmt.Errorf("complete reconciliation run: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("reconciliation run is absent or already completed")
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
