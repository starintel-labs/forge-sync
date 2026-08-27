CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS repository_mappings (
    github_id INTEGER PRIMARY KEY,
    github_full_name TEXT NOT NULL,
    forgejo_owner TEXT NOT NULL,
    forgejo_name TEXT NOT NULL,
    visibility TEXT NOT NULL CHECK (visibility IN ('private', 'internal', 'public')),
    archived INTEGER NOT NULL CHECK (archived IN (0, 1)),
    last_state_hash TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (forgejo_owner, forgejo_name)
);

CREATE TABLE IF NOT EXISTS issue_mappings (
    repository_github_id INTEGER NOT NULL REFERENCES repository_mappings(github_id),
    github_id INTEGER NOT NULL,
    forgejo_id INTEGER NOT NULL,
    github_index INTEGER NOT NULL,
    forgejo_index INTEGER NOT NULL,
    last_state_hash TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (repository_github_id, github_id),
    UNIQUE (repository_github_id, forgejo_id)
);

CREATE TABLE IF NOT EXISTS comment_mappings (
    repository_github_id INTEGER NOT NULL REFERENCES repository_mappings(github_id),
    issue_github_id INTEGER NOT NULL,
    github_id INTEGER NOT NULL,
    forgejo_id INTEGER NOT NULL,
    last_state_hash TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (repository_github_id, github_id),
    UNIQUE (repository_github_id, forgejo_id)
);

CREATE TABLE IF NOT EXISTS pull_request_mappings (
    repository_github_id INTEGER NOT NULL REFERENCES repository_mappings(github_id),
    github_id INTEGER NOT NULL,
    forgejo_id INTEGER NOT NULL,
    github_index INTEGER NOT NULL,
    forgejo_index INTEGER NOT NULL,
    last_state_hash TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (repository_github_id, github_id),
    UNIQUE (repository_github_id, forgejo_id)
);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    forge TEXT NOT NULL CHECK (forge IN ('github', 'forgejo')),
    delivery_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    received_at TEXT NOT NULL,
    processed_at TEXT,
    error TEXT,
    PRIMARY KEY (forge, delivery_id)
);

CREATE TABLE IF NOT EXISTS conflicts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    repository TEXT NOT NULL,
    object_key TEXT NOT NULL,
    github_state TEXT NOT NULL,
    forgejo_state TEXT NOT NULL,
    last_known_state TEXT NOT NULL,
    resolved_at TEXT,
    created_at TEXT NOT NULL,
    UNIQUE (kind, repository, object_key, github_state, forgejo_state)
);

CREATE TABLE IF NOT EXISTS reconciliation_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scope TEXT NOT NULL,
    dry_run INTEGER NOT NULL CHECK (dry_run IN (0, 1)),
    started_at TEXT NOT NULL,
    completed_at TEXT,
    status TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
    summary_json TEXT,
    error TEXT
);
