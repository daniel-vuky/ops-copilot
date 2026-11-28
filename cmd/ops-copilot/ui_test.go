package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/daniel-vuky/ops-copilot/internal/platform/adapters/httpserver"
	"github.com/daniel-vuky/ops-copilot/web"
)

var (
	// uiRefPattern matches the root-relative src and href values in
	// index.html.
	uiRefPattern = regexp.MustCompile(`(?:src|href)="(/[^"]*)"`)
	// uiFontPattern matches the font URLs in the built CSS.
	uiFontPattern = regexp.MustCompile(`url\((/assets/[^)]+\.woff2)\)`)
)

// uiContentTypes is the Content-Type each referenced file must be served
// with, by extension.
var uiContentTypes = map[string]string{
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".svg":   "image/svg+xml",
	".woff2": "font/woff2",
}

// TestEmbeddedUI checks the binary's embedded build end to end through the
// router: index.html, every file it references and every font the CSS
// references. It runs only with OC_REQUIRE_UI=1 (make verify-ui), after a
// frontend build; a plain go test skips it.
func TestEmbeddedUI(t *testing.T) {
	if os.Getenv("OC_REQUIRE_UI") != "1" {
		t.Skip("set OC_REQUIRE_UI=1 to check the embedded UI (make verify-ui)")
	}
	app := web.App()
	if app == nil {
		t.Fatal("web.App() is nil: the binary embeds no frontend build (run make build-web)")
	}
	router := httpserver.NewRouter(nil, nil, httpserver.SPAHandler(app))

	index := getUI(t, router, "/", "text/html; charset=utf-8")
	var css []string
	counts := map[string]int{}
	for _, m := range uiRefPattern.FindAllStringSubmatch(index, -1) {
		ref := m[1]
		ext := path.Ext(ref)
		want, ok := uiContentTypes[ext]
		if !ok {
			t.Errorf("index.html references %q with unexpected extension %q", ref, ext)
			continue
		}
		body := getUI(t, router, ref, want)
		counts[ext]++
		if ext == ".css" {
			css = append(css, body)
		}
	}
	if counts[".js"] == 0 || counts[".css"] == 0 {
		t.Fatalf("index.html references %d .js and %d .css files; want at least one of each", counts[".js"], counts[".css"])
	}

	var inter, mono bool
	for _, sheet := range css {
		for _, m := range uiFontPattern.FindAllStringSubmatch(sheet, -1) {
			font := m[1]
			inter = inter || strings.HasPrefix(font, "/assets/inter-")
			mono = mono || strings.HasPrefix(font, "/assets/jetbrains-mono-")
			getUI(t, router, font, uiContentTypes[".woff2"])
		}
	}
	if !inter {
		t.Error("the CSS references no url(/assets/inter-…woff2)")
	}
	if !mono {
		t.Error("the CSS references no url(/assets/jetbrains-mono-…woff2)")
	}
}

// getUI GETs target through h, checks a 200 with the wanted Content-Type and
// returns the body.
func getUI(t *testing.T, h http.Handler, target, contentType string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	res := rec.Result()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", target, err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET %s: status %d, want 200", target, res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != contentType {
		t.Errorf("GET %s: Content-Type %q, want %q", target, got, contentType)
	}
	return string(body)
}
