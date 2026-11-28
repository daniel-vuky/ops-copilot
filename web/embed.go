// Package web embeds the Vite build of the React SPA in the binary (ADR 0001
// §13). It lives here, outside internal/, because go:embed cannot reach a
// parent directory, so the embedding package must sit above web/dist
// (ADR 0001 §1). It uses the standard library only.
package web

import (
	"embed"
	"io/fs"
)

// dist is the whole web/dist tree. The all: prefix also embeds names that
// start with "." or "_": the tracked dist/.gitkeep keeps the pattern matching
// before the first frontend build, and Vite may emit chunks whose names start
// with "_". scripts/check-dist.mjs rejects dotfiles inside dist/app, and App
// serves only that sub-tree.
//
//go:embed all:dist
var dist embed.FS

// appDir is the directory Vite writes the build into (web/vite.config.ts).
const appDir = "dist/app"

// App returns the build output, rooted at dist/app so index.html is at its
// root, or nil when the binary embeds no build (no dist/app/index.html), in
// which case the server shows its placeholder page.
func App() fs.FS {
	return appFrom(dist)
}

// appFrom returns the dist/app sub-tree of fsys, or an untyped nil when it
// has no index.html.
func appFrom(fsys fs.FS) fs.FS {
	app, err := fs.Sub(fsys, appDir)
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(app, "index.html"); err != nil {
		return nil
	}
	return app
}
