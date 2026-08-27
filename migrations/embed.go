package migrations

import "embed"

// Files contains the ordered database schema migrations.
//
//go:embed *.sql
var Files embed.FS
