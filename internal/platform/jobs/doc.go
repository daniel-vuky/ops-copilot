// Package jobs is the in-process job scheduler domain (ADR 0001 §6,
// ADR 0006): CVE scans, document extraction, retention, backups,
// notifications and investigations run as jobs backed by the jobs table.
//
// The package knows no SQL and no driver. It imports only the standard
// library and the platform ports (store, clock); persistence goes through
// the Store port, which an adapter implements on top of the store's write
// transaction (ADR 0005 §2).
package jobs
