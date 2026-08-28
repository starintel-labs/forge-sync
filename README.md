# forge-sync

Deterministic, continuously running synchronization service between GitHub
(`starintel-labs`, `lost-rob0t`) and a Forgejo instance. The service is a pure
Go daemon: no LLM, model, agent, semantic, or inference component exists
anywhere in its dependency graph.

## Behavior

- **Discovery**: every accessible repository under the configured namespaces is
  discovered on each reconciliation cycle. New GitHub repositories are
  migrated into Forgejo automatically; no manual registration and no
  redeployment is required. Renames and transfers are detected and followed.
- **Visibility**: unknown or missing visibility always fails closed to
  *private*. An API failure is never interpreted as deletion or as a
  visibility change.
- **Refs**:
  - GitHub is authoritative for `main`, `master`, and `refs/tags/*`
    (fast-forward only).
  - Forgejo is authoritative for development branches matching
    `feature/*`, `fix/*`, `agent/*`, `rage/*` (fast-forward only).
  - Divergent history is never force-pushed; a conflict is recorded and both
    sides are left untouched.
- **Issues, comments, labels, milestones**: bidirectional reconciliation with
  durable stable-ID mappings, content state hashes, conflict recording, and
  loop suppression.
- **Pull requests**: metadata (title, body, state, head, base) is reconciled
  bidirectionally with durable mappings. PR conversation comments are
  synchronized using the issue-comment endpoints both APIs expose for PR
  threads.
- **Releases**: metadata is reconciled bidirectionally; release assets present
  on one side are ensured on the other by name. Assets are never deleted, and
  a mapped release that disappears from one side records a conflict instead of
  being deleted or recreated.
- **Webhooks**: verified HMAC-SHA256 webhooks from both forges trigger scoped
  reconciliation; delivery IDs are claimed in SQLite so replays are suppressed.
- **Periodic reconciliation** runs on a timer regardless of webhook delivery,
  providing self-healing and continuous discovery.

### Conflict resolution

The service never picks a winner. When both sides changed the same object
since the last synchronized state, the object's divergent hashes are recorded
in the `conflicts` table and surfaced by `forge-sync conflicts`, the `/metrics`
endpoint, and the `inspect` subcommand. Operators resolve manually; the next
reconciliation then treats the resolved state as canonical.

## Configuration

All configuration is environment-only. Secrets must be injected at runtime
(never baked into images or committed).

| Variable | Default | Description |
| --- | --- | --- |
| `FORGE_SYNC_GITHUB_TOKEN` | *(required)* | GitHub access token (read/write for repos, issues, PRs, releases) |
| `FORGE_SYNC_FORGEJO_API` | *(required)* | Forgejo base URL, e.g. `https://forge.example.org` |
| `FORGE_SYNC_FORGEJO_TOKEN` | *(required)* | Forgejo token with repo, issue, PR, release scopes |
| `FORGE_SYNC_GITHUB_WEBHOOK_SECRET` | *(required)* | HMAC secret for GitHub webhooks |
| `FORGE_SYNC_FORGEJO_WEBHOOK_SECRET` | *(required)* | HMAC secret for Forgejo webhooks |
| `FORGE_SYNC_NAMESPACES` | `starintel-labs,lost-rob0t` | Allowed namespaces (restricted to this set) |
| `FORGE_SYNC_STATE_PATH` | `/var/lib/forge-sync/forge-sync.db` | SQLite state file |
| `FORGE_SYNC_LISTEN_ADDR` | `127.0.0.1:8080` | Health/metrics/webhook listener |
| `FORGE_SYNC_RECONCILE_INTERVAL` | `5m` | Periodic full reconciliation interval (min 1m) |
| `FORGE_SYNC_REQUEST_TIMEOUT` | `30s` | Per-API-request timeout |
| `FORGE_SYNC_GIT_TIMEOUT` | `5m` | Per-repository git mirror timeout |
| `FORGE_SYNC_MAX_CONCURRENCY` | `4` | Parallel repository workers (1–32) |
| `FORGE_SYNC_MAX_WEBHOOK_BODY` | `1048576` | Webhook body cap (1 KiB–16 MiB) |
| `FORGE_SYNC_API_MAX_ATTEMPTS` | `4` | Bounded attempts per API call (incl. first) |
| `FORGE_SYNC_API_RETRY_BASE` | `1s` | Exponential backoff base delay |
| `FORGE_SYNC_API_RETRY_MAX` | `30s` | Backoff ceiling; also caps server `Retry-After` for ordinary transients |
| `FORGE_SYNC_FORGEJO_OWNER_MAP` | *(unset)* | Comma list of `github-namespace:forgejo-owner` redirects, e.g. `starintel-labs:nsaspy`; keys must be configured namespaces |

Transient API failures (HTTP `429`, `5xx`, network errors) are retried with a
deterministic exponential backoff that honors a longer server-provided
`Retry-After` when present. Exhausted rate limits (429, or 403 with exhausted
rate headers) are obeyed rather than hammered: the client waits out the
documented reset time (capped by a dedicated rate-limit ceiling of 15 minutes)
before the next request. Other `4xx` failures fail immediately.

## Usage

```text
forge-sync {status|bootstrap [--dry-run]|discover|reconcile [owner/repo]|inspect owner/repo|conflicts|serve}
```

- `bootstrap --dry-run` — inventory both forges and report drift without any
  mutation. Safe to run at any time; this is the pre-flight gate for real
  bootstrap.
- `bootstrap` — discover, migrate missing repositories, and run one full
  reconciliation cycle (idempotent; safe to re-run after a crash).
- `serve` — run the daemon: startup reconciliation, periodic reconciliation,
  webhook receiver, `/healthz`, and `/metrics`.
- `status` — mapping counts (repositories, issues, PRs, comments, releases,
  conflicts, webhook deliveries, runs).
- `inspect owner/repo` — mappings and conflicts for one repository.
- `conflicts` — unresolved conflict list in JSON.

### Endpoints (serve mode)

- `GET /healthz` — liveness plus SQLite reachability.
- `GET /metrics` — text exposition: `forge_sync_repositories`,
  `forge_sync_issues`, `forge_sync_pull_requests`, `forge_sync_comments`,
  `forge_sync_releases`, `forge_sync_conflicts`,
  `forge_sync_webhook_deliveries`, `forge_sync_reconciliation_runs`.
- `POST /webhooks/github`, `POST /webhooks/forgejo` — verified webhooks.

### Webhook wiring

GitHub repository/web/organization webhook → `https://<host>/webhooks/github`,
content type `application/json`, secret `FORGE_SYNC_GITHUB_WEBHOOK_SECRET`,
events: push, repository, issues, issue_comment, pull_request, release,
milestone, label.

Forgejo webhook per organization (or repo) → `https://<host>/webhooks/forgejo`,
content type `application/json`, secret `FORGE_SYNC_FORGEJO_WEBHOOK_SECRET`,
events: push, repository, issues, issue_comment, pull_request, release.

Missed webhooks are fully covered by periodic reconciliation; webhooks only
reduce latency.

## State and crash safety

All durable state lives in a single SQLite database (`FORGE_SYNC_STATE_PATH`,
WAL mode). Every external mutation is preceded or followed by mapping upserts
so that:

- re-running any reconciliation is a no-op when nothing changed (idempotent),
- a crash between "remote mutation" and "mapping write" self-heals on restart
  by pairing identical state hashes, never duplicating objects,
- webhook redelivery is suppressed by `(forge, delivery_id)` claims.

Keep the state file on persistent storage. Losing it forces re-pairing (safe
but noisy: identical objects re-pair by hash; divergent objects need manual
resolution).

## Building

- Nix: `nix build .#` produces the static binary; `nix build .#docker` builds
  the container image through the Nix store.
- Docker: `docker build -t forge-sync .` (multi-stage Go build, distroless-style
  runtime).
- Go: `go build ./cmd/forge-sync`.

## Deployment

The service runs as a dedicated non-root user with a writable state directory
only. See `deploy/` for the reference systemd unit and the
`starintel-infra` deployment which provides secrets, persistent state,
webhook ingress, health checks, and resource limits.

## Development

```bash
go build ./...      # compile
go vet ./...        # static checks
go test ./...       # unit + local git integration tests (no network)
```

Migrations are embedded SQL files under `migrations/` applied in order on
startup; they are transactional and versioned.
