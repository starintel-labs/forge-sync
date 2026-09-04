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
- **Refs** (true two-way sync, Forgejo is the master forge):
  - Every branch and tag ref on either forge participates; there is no
    prefix allow-list.
  - A ref that is behind on one forge fast-forwards from the other, in
    whichever direction that is (GitHub-only changes are pulled into
    Forgejo; Forgejo-only changes are pushed to GitHub).
  - A ref missing on one forge is copied from the other. Refs are never
    deleted by the policy.
  - When histories truly diverge, Forgejo (`git.starintel.actor`) wins: its
    state is force-enforced on the GitHub mirror and the overridden GitHub
    SHA is recorded as a `git-ref-override` audit entry for recovery.
    Forgejo history is never overwritten.
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
- **Actions secrets**: explicitly configured runtime secret values are written
  to the mapped Forgejo repository without being persisted or logged. Secret
  values are never deleted automatically.
- **Webhooks**: verified HMAC-SHA256 webhooks from both forges trigger scoped
  reconciliation; delivery IDs are claimed in SQLite so replays are suppressed.
- **Periodic reconciliation** runs on a timer regardless of webhook delivery,
  providing self-healing and continuous discovery.

### Conflict resolution

Git refs are self-healing: a behind ref fast-forwards in whichever direction
is needed, and on true divergence the master forge (Forgejo) is enforced on
the GitHub mirror, with the overridden GitHub SHA recorded as a
`git-ref-override` entry so it remains recoverable. For every other object
the service never picks a winner: when both sides changed the same object
since the last synchronized state, the object's divergent hashes are recorded
in the `conflicts` table and surfaced by `forge-sync conflicts`, the
`/metrics` endpoint, and the `inspect` subcommand. Operators resolve manually;
the next reconciliation then treats the resolved state as canonical.

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
| `FORGE_SYNC_GITHUB_MIN_INTERVAL` | `0` (off) | Minimum spacing between GitHub API requests to protect quota (e.g. `1500ms`) |
| `FORGE_SYNC_FORGEJO_MIN_INTERVAL` | `0` (off) | Minimum spacing between Gitea API requests |
| `FORGE_SYNC_MAX_REF_SIZE_MB` | `8192` | Ref sync skips repositories whose GitHub size exceeds this (recorded as `ref-sync-skipped`; never deleted) |
| `FORGE_SYNC_MAX_ASSET_SIZE_MB` | `512` | Release assets larger than this are skipped and recorded as `release-asset-skipped` instead of being buffered into memory |
| `FORGE_SYNC_FORGEJO_OWNER_MAP` | *(identity)* | Comma list of `github-namespace:forgejo-owner` redirects, e.g. `lost-rob0t:nsaspy`; unset keys keep their GitHub namespace |
| `FORGE_SYNC_ACTION_SECRET_MAP` | *(unset)* | Comma list of `github-owner/repository:SECRET_NAME=RUNTIME_ENV_VAR` mappings for Forgejo Actions secrets |

GitHub's Actions API exposes secret names but not their plaintext values. To
reuse an existing GitHub key without creating another one, inject that same
value into the forge-sync runtime and map it explicitly:

```text
FORGE_SYNC_ACTION_SECRET_MAP=lost-rob0t/prolog-rlm:API_KEY=PROLOG_RLM_API_KEY
PROLOG_RLM_API_KEY=<the existing API key value>
```

The source variable is read at startup, held only in memory, and sent over the
authenticated Forgejo API. It is not written to SQLite, logs, or repository
files. Removing a mapping does not delete the Forgejo secret.

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
