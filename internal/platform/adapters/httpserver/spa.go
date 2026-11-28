package httpserver

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/logging"
)

const (
	// spaIndex is the SPA entry point, served for every UI path that names no
	// file.
	spaIndex = "index.html"
	// spaAssetsPrefix is the URL prefix of the hashed build assets.
	spaAssetsPrefix = "/assets/"
	// cacheImmutable is the Cache-Control of hashed assets: their name changes
	// with their content.
	cacheImmutable = "public, max-age=31536000, immutable"
	// cachePublicFile is the Cache-Control of unhashed files at the root of the
	// build, such as favicon.svg.
	cachePublicFile = "public, max-age=3600"
	// cacheNoStore is the Cache-Control of index.html and of every 404, so a
	// new build is picked up at once.
	cacheNoStore = "no-store"
	// defaultContentType is sent for a file whose extension is not in
	// contentTypes.
	defaultContentType = "application/octet-stream"
)

// contentTypes maps the file extensions of the build output to their
// Content-Type. It is fixed, so the answer never depends on the host's MIME
// database.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".svg":         "image/svg+xml",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".json":        "application/json",
	".ico":         "image/x-icon",
	".png":         "image/png",
	".txt":         "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json",
}

// SPAHandler serves the built SPA from fsys, whose root holds index.html, the
// hashed files under assets/ and the unhashed public files. A nil fsys serves
// the built-in placeholder page instead.
//
//   - /assets/<file> serves the file with a one-year immutable Cache-Control,
//     or a text 404 with no-store when it is missing, a segment starts with
//     ".", the path ends in "/" or the cleaned path leaves /assets/.
//     /assets/ itself is a 404; /assets is the app.
//   - A path whose first segment starts with "." is a text 404 with no-store:
//     never the file, never the app.
//   - /<file> naming a regular file at the root of fsys (other than
//     index.html) serves it with a one-hour Cache-Control. With a trailing
//     slash (/<file>/) it is the app, never the file.
//   - Every other path serves index.html with no-store, so client-side routes
//     work as deep links.
//
// The security headers come from securityHeaders and the GET/HEAD check from
// uiOnlyGet. Responses are not compressed; that is the proxy's job.
func SPAHandler(fsys fs.FS) http.Handler {
	if fsys == nil {
		return placeholderUI()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The mux has cleaned the path except for a trailing slash, so the
		// raw prefix decides the branch: "/assets/" is an asset path (and a
		// 404), "/assets" is the app.
		if strings.HasPrefix(r.URL.Path, spaAssetsPrefix) {
			p := cleanPath(r.URL.Path)
			// An escaped slash such as /assets/..%2ffavicon.svg reaches
			// here unclean; refuse anything that cleans to outside assets/.
			// A trailing slash would alias the file under a second URL.
			if !strings.HasPrefix(p, spaAssetsPrefix) || hasDotSegment(p) || strings.HasSuffix(r.URL.Path, "/") ||
				serveFile(w, r, fsys, strings.TrimPrefix(p, "/"), cacheImmutable) != nil {
				spaNotFound(w)
			}
			return
		}

		name := strings.TrimPrefix(cleanPath(r.URL.Path), "/")
		first, _, nested := strings.Cut(name, "/")
		if strings.HasPrefix(first, ".") {
			spaNotFound(w)
			return
		}
		rootFile := !nested && name != "" && name != spaIndex && !strings.HasSuffix(r.URL.Path, "/")
		if rootFile && serveFile(w, r, fsys, name, cachePublicFile) == nil {
			return
		}
		if err := serveFile(w, r, fsys, spaIndex, cacheNoStore); err != nil {
			// web.App guarantees index.html; reaching here is a build bug.
			logging.FromContext(r.Context()).LogAttrs(r.Context(), slog.LevelError, "serve SPA index",
				slog.String("file", spaIndex),
				slog.Any("error", err),
			)
			w.Header().Set("Cache-Control", cacheNoStore)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	})
}

// errNotServable reports a path that exists in the SPA build but is not a
// regular, seekable file.
var errNotServable = errors.New("not a regular seekable file")

// serveFile serves the regular file name from fsys with the given
// Cache-Control, honouring HEAD and Range, and returns nil. When name is not a
// regular file that can be opened and seeked it returns why, having written
// nothing.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name, cacheControl string) error {
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return fmt.Errorf("stat SPA file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("SPA file %s: %w", name, errNotServable)
	}
	f, err := fsys.Open(name)
	if err != nil {
		return fmt.Errorf("open SPA file: %w", err)
	}
	defer func() { _ = f.Close() }()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// embed.FS and fstest.MapFS files are seekable; anything else is
		// treated as missing rather than read into memory.
		return fmt.Errorf("SPA file %s: %w", name, errNotServable)
	}

	ct, ok := contentTypes[path.Ext(name)]
	if !ok {
		ct = defaultContentType
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", cacheControl)
	// A zero modification time sends no Last-Modified: embedded files have
	// none worth sending, and hashed names make revalidation unnecessary.
	http.ServeContent(w, r, name, time.Time{}, rs)
	return nil
}

// spaNotFound answers a text 404 that caches must not keep.
func spaNotFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", cacheNoStore)
	http.Error(w, "404 page not found", http.StatusNotFound)
}

// hasDotSegment reports whether a segment of the cleaned path p starts with
// ".", such as a dot file under assets/.
func hasDotSegment(p string) bool {
	for s := range strings.SplitSeq(p, "/") {
		if strings.HasPrefix(s, ".") {
			return true
		}
	}
	return false
}
