package sqlite

import (
	"embed"
	"io/fs"
)

// migrationFiles holds the migrations directory; see migrations/README.md
// for the rules its files follow.
//
//go:embed migrations
var migrationFiles embed.FS

// EmbeddedMigrations returns the migrations built into the binary, with the
// NNNN_slug.sql files at its root.
func EmbeddedMigrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		// fs.Sub fails only on an invalid path, and "migrations" is valid.
		panic("sqlite: embedded migrations: " + err.Error())
	}
	return sub
}
