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
// The rule walks internal/, cmd/ and web/, and each must exist. The web
// package (the go:embed of the SPA build, ADR 0001 §13) is held to the
// domain rule: it is neither an adapter nor a command, and it needs only the
// standard library. web/node_modules and web/dist are not walked. A Go
// package added later outside these trees must be added to walkRoots in
// checkImports, or the rule will not see it.
//
// The package has no non-test code; the rule runs as part of go test ./...
package archtest
