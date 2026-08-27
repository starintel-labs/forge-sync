CREATE TABLE IF NOT EXISTS release_mappings (
    repository_github_id INTEGER NOT NULL REFERENCES repository_mappings(github_id),
    github_id INTEGER NOT NULL,
    forgejo_id INTEGER NOT NULL,
    tag TEXT NOT NULL,
    last_state_hash TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (repository_github_id, github_id),
    UNIQUE (repository_github_id, forgejo_id)
);
