// Package archtest holds tests that enforce the ports and adapters import
// rule of ADR 0001 §1 on the whole repository.
//
// Domain logic lives in internal/<domain>/ and technology in
// internal/<domain>/adapters/<tech>/. A domain package (any package under
// internal/ whose directory path has no "adapters" segment) may import the
// standard library and non-adapter packages of this module only. It must not
// import an adapter package (an import path with an "adapters" segment) or a
// third-party package (a path whose first segment contains a dot and is
// outside this module), unless that package is explicitly allowed. Adapter
// packages and the commands under cmd/ are exempt. Test files are not checked.
//
// The rule walks only internal/ and cmd/. A Go package added later outside
// them (for example a top-level package for go:embed) must be added to the
// walk in checkImports, or the rule will not see it.
//
// The package has no non-test code; the rule runs as part of go test ./...
package archtest
