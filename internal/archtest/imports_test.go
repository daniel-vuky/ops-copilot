package archtest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// allowedThirdParty lists third-party import paths a domain package may use.
// It is empty: adding one needs an ADR (ADR 0001 §1).
var allowedThirdParty = map[string]bool{}

// Reasons reported in a violation.
const (
	reasonAdapter      = "domain package imports an adapter package"
	reasonThirdParty   = "domain package imports a third-party package"
	reasonWebNotStdlib = "web package imports a non-standard-library package"
)

// violation is one import that breaks the rule.
type violation struct {
	File   string // slash-separated path of the importing file
	Import string // the imported package path
	Reason string // why the import is not allowed
}

// walkRoots are the trees checkImports walks. Each must exist.
var walkRoots = []string{"internal", "cmd", "web"}

// skipPaths are directories below a walk root that are never walked: this
// package, and the frontend dependencies and build output under web, which
// hold no Go code of this module.
var skipPaths = map[string]bool{
	"internal/archtest": true,
	"web/node_modules":  true,
	"web/dist":          true,
}

// checkImports walks the internal, cmd and web trees of fsys and returns
// every import of a domain package that breaks the rule, in walk (lexical)
// order. module is the module path from go.mod.
func checkImports(fsys fs.FS, module string) ([]violation, error) {
	var out []violation
	fset := token.NewFileSet()

	for _, root := range walkRoots {
		err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if (p != root && skipDir(d.Name())) || skipPaths[p] {
					return fs.SkipDir
				}
				return nil
			}
			if skipFile(d.Name()) {
				return nil
			}

			src, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, p, src, parser.ImportsOnly)
			if err != nil {
				return fmt.Errorf("parse %s: %w", p, err)
			}
			web := root == "web"
			if root == "cmd" || (!web && hasSegment(path.Dir(p), "adapters")) {
				return nil
			}

			for _, spec := range f.Imports {
				imp, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return fmt.Errorf("%s: import %s: %w", p, spec.Path.Value, err)
				}
				reason := importViolation(imp, module)
				if web {
					reason = webImportViolation(imp, module)
				}
				if reason != "" {
					out = append(out, violation{File: p, Import: imp, Reason: reason})
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	return out, nil
}

// skipDir reports whether a directory below a walk root is left out. Like
// the go tool, it ignores testdata and names starting with "." or "_".
func skipDir(name string) bool {
	return name == "testdata" || hasIgnoredPrefix(name)
}

// skipFile reports whether a file is left out: anything that is not a
// non-test Go file, and, like the go tool, names starting with "." or "_".
func skipFile(name string) bool {
	return !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
		hasIgnoredPrefix(name)
}

// hasIgnoredPrefix reports whether the go tool ignores a file or directory
// with this name.
func hasIgnoredPrefix(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// importViolation returns why a domain package may not import imp, or ""
// when the import is allowed.
func importViolation(imp, module string) string {
	if hasSegment(imp, "adapters") {
		return reasonAdapter
	}
	first, _, _ := strings.Cut(imp, "/")
	inModule := imp == module || strings.HasPrefix(imp, module+"/")
	if strings.Contains(first, ".") && !inModule && !allowedThirdParty[imp] {
		return reasonThirdParty
	}
	return ""
}

// webImportViolation returns why a package under web may not import imp, or
// "" when the import is allowed. web may use the standard library only: no
// package of this module and no path whose first segment contains a dot.
func webImportViolation(imp, module string) string {
	first, _, _ := strings.Cut(imp, "/")
	if imp == module || strings.HasPrefix(imp, module+"/") || strings.Contains(first, ".") {
		return reasonWebNotStdlib
	}
	return ""
}

// hasSegment reports whether one of the slash-separated segments of p is seg.
func hasSegment(p, seg string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

// TestRepositoryFollowsImportRule checks the real repository: no domain
// package imports an adapter or a third-party package.
func TestRepositoryFollowsImportRule(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	fsys := os.DirFS(root)

	gomod, err := fs.ReadFile(fsys, "go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	module := modulePath(t, gomod)

	violations, err := checkImports(fsys, module)
	if err != nil {
		t.Fatalf("checkImports: %v", err)
	}
	for _, v := range violations {
		t.Errorf("%s imports %s: %s", v.File, v.Import, v.Reason)
	}
}

// TestImportRuleCatchesViolations checks the rule against a synthetic tree.
func TestImportRuleCatchesViolations(t *testing.T) {
	t.Parallel()

	const module = "example.test/m"
	fsys := fstest.MapFS{
		"internal/billing/domain.go": goFile("billing",
			module+"/internal/billing/adapters/sql",
			module+"/internal/billing/adapters",
			"github.com/vendor/sdk",
		),
		"internal/billing/domain_test.go": goFile("billing",
			module+"/internal/billing/adapters/sql",
		),
		"internal/billing/adapters/sql/sql.go": goFile("sql",
			"github.com/vendor/sdk",
		),
		"cmd/app/main.go": goFile("main",
			module+"/internal/billing/adapters/sql",
		),
		"internal/billing/ok.go": goFile("billing",
			"fmt",
			module+"/internal/platform/clock",
		),
		// Ignored like the go tool does: none of these may report.
		"internal/billing/_gen.go":          goFile("billing", "github.com/vendor/sdk"),
		"internal/billing/.hidden.go":       goFile("billing", "github.com/vendor/sdk"),
		"internal/_scratch/x.go":            goFile("x", "github.com/vendor/sdk"),
		"internal/.cache/x.go":              goFile("x", "github.com/vendor/sdk"),
		"internal/billing/testdata/x.go":    goFile("x", "github.com/vendor/sdk"),
		"internal/billing/notes.txt":        &fstest.MapFile{Data: []byte("not go")},
		"internal/archtest/fixture_rule.go": goFile("archtest", "github.com/vendor/sdk"),
		// web is walked with the stdlib-only rule and no adapters exemption;
		// its node_modules and dist are not walked.
		"web/embed.go": goFile("web",
			"embed",
			module+"/internal/platform/clock",
			"github.com/vendor/sdk",
		),
		"web/adapters/x/x.go":     goFile("x", "github.com/vendor/sdk"),
		"web/embed_test.go":       goFile("web", "github.com/vendor/sdk"),
		"web/node_modules/x.go":   goFile("x", "github.com/vendor/sdk"),
		"web/node_modules/p/y.go": goFile("y", "github.com/vendor/sdk"),
		"web/dist/x.go":           goFile("x", "github.com/vendor/sdk"),
		"web/dist/app/y.go":       goFile("y", "github.com/vendor/sdk"),
	}

	got, err := checkImports(fsys, module)
	if err != nil {
		t.Fatalf("checkImports: %v", err)
	}

	want := []struct {
		name, file, imp, reason string
	}{
		{"adapter subpackage", "internal/billing/domain.go", module + "/internal/billing/adapters/sql", reasonAdapter},
		{"adapters segment at end", "internal/billing/domain.go", module + "/internal/billing/adapters", reasonAdapter},
		{"vendor sdk", "internal/billing/domain.go", "github.com/vendor/sdk", reasonThirdParty},
		{"web adapter not exempt", "web/adapters/x/x.go", "github.com/vendor/sdk", reasonWebNotStdlib},
		{"web module package", "web/embed.go", module + "/internal/platform/clock", reasonWebNotStdlib},
		{"web vendor sdk", "web/embed.go", "github.com/vendor/sdk", reasonWebNotStdlib},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d violations, want %d: %+v", len(got), len(want), got)
	}
	for i, tt := range want {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := got[i]
			if v.File != tt.file {
				t.Errorf("File = %q, want %q", v.File, tt.file)
			}
			if v.Import != tt.imp {
				t.Errorf("Import = %q, want %q", v.Import, tt.imp)
			}
			if v.Reason != tt.reason {
				t.Errorf("Reason = %q, want %q", v.Reason, tt.reason)
			}
		})
	}
}

// TestImportRuleReportsMissingRoot checks that a missing internal, cmd or
// web directory is an error, not a silent pass.
func TestImportRuleReportsMissingRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fsys fstest.MapFS
	}{
		{"no internal", fstest.MapFS{"cmd/app/main.go": goFile("main"), "web/embed.go": goFile("web")}},
		{"no cmd", fstest.MapFS{"internal/a/a.go": goFile("a"), "web/embed.go": goFile("web")}},
		{"no web", fstest.MapFS{"internal/a/a.go": goFile("a"), "cmd/app/main.go": goFile("main")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := checkImports(tt.fsys, "example.test/m")
			if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("err = %v, want fs.ErrNotExist", err)
			}
		})
	}
}

// goFile returns the source of a Go file in package pkg importing imports.
func goFile(pkg string, imports ...string) *fstest.MapFile {
	var b strings.Builder
	b.WriteString("package " + pkg + "\n\nimport (\n")
	for _, imp := range imports {
		b.WriteString("\t_ \"" + imp + "\"\n")
	}
	b.WriteString(")\n")
	return &fstest.MapFile{Data: []byte(b.String())}
}

// moduleRoot walks up from the working directory to the directory holding
// go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// modulePath returns the path on the module line of a go.mod file.
func modulePath(t *testing.T, gomod []byte) string {
	t.Helper()

	sc := bufio.NewScanner(bytes.NewReader(gomod))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan go.mod: %v", err)
	}
	t.Fatal("no module line in go.mod")
	return ""
}
