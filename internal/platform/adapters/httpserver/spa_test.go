package httpserver

import (
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	indexData      = "<!doctype html><title>app</title>"
	assetData      = "console.log('app');"
	fontData       = "wOF2-font-bytes"
	faviconData    = "<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>"
	hiddenData     = "secret-hidden-bytes"
	immutableCache = "public, max-age=31536000, immutable"
	publicCache    = "public, max-age=3600"
	noStore        = "no-store"
)

// testSPAFS is the built SPA the tests serve: index.html, hashed assets, a
// root public file and dot files that must never be served.
func testSPAFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte(indexData)},
		"assets/index-AbC123.js": {Data: []byte(assetData)},
		"assets/inter-latin-wght-normal-Xy9.woff2": {Data: []byte(fontData)},
		"favicon.svg":       {Data: []byte(faviconData)},
		".hidden":           {Data: []byte(hiddenData)},
		"assets/.hidden.js": {Data: []byte(hiddenData)},
	}
}

// newSPARouter returns a router whose UI is SPAHandler over testSPAFS.
func newSPARouter() *Router {
	return NewRouter(nil, nil, SPAHandler(testSPAFS()))
}

func TestSPADeepLinksServeIndex(t *testing.T) {
	t.Parallel()

	r := newSPARouter()
	for _, target := range []string{"/", "/index.html", "/favicon.svg/", "/favicon.svg%2f.", "/investigations/1", "/knowledge/documents/x", "/cve/findings/9", "/mcp/x", "/assets"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			rec := serve(r, http.MethodGet, target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := body(t, rec); got != indexData {
				t.Errorf("body = %q, want %q", got, indexData)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("Content-Type = %q, want text/html; charset=utf-8", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != noStore {
				t.Errorf("Cache-Control = %q, want %q", got, noStore)
			}
		})
	}
}

func TestSPAAssetImmutable(t *testing.T) {
	t.Parallel()

	r := newSPARouter()
	tests := []struct {
		target, wantType, wantBody string
	}{
		{"/assets/index-AbC123.js", "text/javascript; charset=utf-8", assetData},
		{"/assets/inter-latin-wght-normal-Xy9.woff2", "font/woff2", fontData},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			t.Parallel()

			rec := serve(r, http.MethodGet, tt.target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := body(t, rec); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != tt.wantType {
				t.Errorf("Content-Type = %q, want %q", got, tt.wantType)
			}
			if got := rec.Header().Get("Cache-Control"); got != immutableCache {
				t.Errorf("Cache-Control = %q, want %q", got, immutableCache)
			}
		})
	}
}

func TestSPAMissingAssetIs404NoStore(t *testing.T) {
	t.Parallel()

	r := newSPARouter()
	for _, target := range []string{"/assets/gone-1.js", "/assets/", "/assets/..%2ffavicon.svg", "/assets/..%2findex.html", "/assets/.hidden.js", "/assets/index-AbC123.js/", "/assets/index-AbC123.js%2f.", "/assets/x%2f..%2findex-AbC123.js"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			rec := serve(r, http.MethodGet, target, nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != noStore {
				t.Errorf("Cache-Control = %q, want %q", got, noStore)
			}
			if got := body(t, rec); got == faviconData || got == indexData || got == hiddenData || got == assetData {
				t.Errorf("body = %q, want no file contents", got)
			}
		})
	}
}

func TestSPARootPublicFile(t *testing.T) {
	t.Parallel()

	r := newSPARouter()

	t.Run("/favicon.svg", func(t *testing.T) {
		t.Parallel()

		rec := serve(r, http.MethodGet, "/favicon.svg", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := body(t, rec); got != faviconData {
			t.Errorf("body = %q, want %q", got, faviconData)
		}
		if got := rec.Header().Get("Content-Type"); got != "image/svg+xml" {
			t.Errorf("Content-Type = %q, want image/svg+xml", got)
		}
		if got := rec.Header().Get("Cache-Control"); got != publicCache {
			t.Errorf("Cache-Control = %q, want %q", got, publicCache)
		}
	})

	for _, target := range []string{"/.hidden", "/.env", "/.git/config", "/.x%2f..%2ffavicon.svg"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			rec := serve(r, http.MethodGet, target, nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != noStore {
				t.Errorf("Cache-Control = %q, want %q", got, noStore)
			}
			if got := body(t, rec); got == hiddenData || got == indexData || got == faviconData {
				t.Errorf("body = %q, want neither the file nor the app", got)
			}
		})
	}
}

func TestSPAHead(t *testing.T) {
	t.Parallel()

	r := newSPARouter()
	rec := serve(r, http.MethodHead, "/assets/index-AbC123.js", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := body(t, rec); got != "" {
		t.Errorf("body = %q, want empty", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/javascript; charset=utf-8", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != immutableCache {
		t.Errorf("Cache-Control = %q, want %q", got, immutableCache)
	}
}

func TestSPARangeNotSatisfiable(t *testing.T) {
	t.Parallel()

	r := newSPARouter()
	rec := serve(r, http.MethodGet, "/assets/index-AbC123.js", http.Header{"Range": {"bytes=999999-"}})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", rec.Code)
	}
	h := rec.Header()
	if got := h.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("Content-Security-Policy = %q, want %q", got, contentSecurityPolicy)
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := h.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy = %q, want same-origin", got)
	}
}

func TestSPAMissingIndexIs500(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{"assets/index-AbC123.js": {Data: []byte(assetData)}}
	r, buf := newTestRouter(t, true, SPAHandler(fsys))
	rec := serve(r, http.MethodGet, "/investigations/1", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	h := rec.Header()
	if got := h.Get("Cache-Control"); got != noStore {
		t.Errorf("Cache-Control = %q, want %q", got, noStore)
	}
	if got := h.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("Content-Security-Policy = %q, want %q", got, contentSecurityPolicy)
	}
	var logged bool
	for _, l := range logLines(t, buf) {
		if l["msg"] == "serve SPA index" {
			logged = true
			if e, _ := l["error"].(string); !strings.Contains(e, "index.html") {
				t.Errorf("error attr = %q, want the cause naming index.html", e)
			}
		}
	}
	if !logged {
		t.Error("no \"serve SPA index\" log line")
	}
}

func TestSPANilFSServesPlaceholder(t *testing.T) {
	t.Parallel()

	r := NewRouter(nil, nil, SPAHandler(nil))
	rec := serve(r, http.MethodGet, "/investigations/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := body(t, rec); !strings.Contains(got, "not built into this binary") {
		t.Errorf("body = %q, want the placeholder page", got)
	}
}

func TestSPAPostIs405(t *testing.T) {
	t.Parallel()

	r := newSPARouter()
	rec := serve(r, http.MethodPost, "/investigations/1", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
