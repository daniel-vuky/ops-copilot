package web

import (
	"io/fs"
	"testing"
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
