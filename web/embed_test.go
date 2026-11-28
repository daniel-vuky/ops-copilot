package web

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestAppHasIndexOrNil(t *testing.T) {
	app := App()
	if app == nil {
		t.Log("no frontend build embedded; App() is nil")
		return
	}
	if _, err := fs.Stat(app, "index.html"); err != nil {
		t.Fatalf("App() is not nil but has no index.html: %v", err)
	}
	// The sub-tree is dist/app, not dist: the tracked dist/.gitkeep must not
	// be visible at its root.
	if _, err := fs.Stat(app, ".gitkeep"); err == nil {
		t.Fatal("App() exposes .gitkeep; want the dist/app sub-tree, not dist")
	}
}

func TestAppFrom(t *testing.T) {
	t.Run("with a build", func(t *testing.T) {
		app := appFrom(fstest.MapFS{
			"dist/.gitkeep":                {},
			"dist/app/index.html":          {Data: []byte("<!doctype html>")},
			"dist/app/assets/index-x1.js":  {Data: []byte("x")},
			"dist/app/assets/index-x1.css": {Data: []byte("x")},
		})
		if app == nil {
			t.Fatal("appFrom() = nil, want the dist/app sub-tree")
		}
		if _, err := fs.Stat(app, "index.html"); err != nil {
			t.Errorf("index.html not at the root: %v", err)
		}
		if _, err := fs.Stat(app, "assets/index-x1.js"); err != nil {
			t.Errorf("assets/index-x1.js not under the root: %v", err)
		}
		if _, err := fs.Stat(app, ".gitkeep"); err == nil {
			t.Error(".gitkeep is visible; want the dist/app sub-tree, not dist")
		}
	})

	tests := []struct {
		name string
		fsys fstest.MapFS
	}{
		{"only .gitkeep", fstest.MapFS{"dist/.gitkeep": {}}},
		{"app without index.html", fstest.MapFS{"dist/.gitkeep": {}, "dist/app/assets/index-x1.js": {}}},
		{"index.html in dist, not dist/app", fstest.MapFS{"dist/index.html": {}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Compared to an untyped nil: a typed nil inside the interface
			// would defeat the caller's nil check.
			if app := appFrom(tt.fsys); app != nil {
				t.Errorf("appFrom() = %#v, want nil", app)
			}
		})
	}
}
