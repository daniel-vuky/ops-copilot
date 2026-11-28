# 1. Build Ops Copilot as one Go binary on SQLite

Date: 2026-11-27

## Status

Amended by [0004](0004-settle-open-points-in-adrs-0001-0003.md), [0005](0005-set-conventions-for-the-sqlite-store.md) and [0006](0006-set-conventions-for-the-job-scheduler.md)

## Context

Ops Copilot is a small internal tool for about 20 users and one engineer, Daniel. It has four jobs:

1. watch his systems (New Relic, Azure alerts) and the CVEs in the stacks he runs;
2. serve his own documents (Markdown, PDF, DOCX, draw.io; 50–100 MB in all, mostly English) to Claude through an MCP server;
3. investigate an alert with Claude, reading those documents, New Relic and the company's MCP server (static token, reachable from the internet), which is where code comes from, so nothing here indexes code;
4. suggest a fix for what it finds, and never act on it.

It first runs as a POC on Daniel's own server (8 GB RAM, mid-range CPU) behind an existing Cloudflare Tunnel, then on a server the company provides. It must not depend on Cloudflare.

An earlier attempt (the `opts-copilot` repository) built for an organisation: six services, Postgres with pgvector, Redis and asynq, SeaweedFS, Keycloak, two TEI embedding models, Caddy with an internal CA, a redaction sidecar and a separate React portal. It ran about 13 containers and needed a 32 GB machine. This repository starts over from what that attempt taught.

Sign-in and access are decided in [0002](0002-sign-in-with-google-and-an-embedded-oauth-server.md). The investigator, its tools and its reports are decided in [0003](0003-investigate-through-a-locked-down-tool-surface.md).

### Option A: Several services and managed infrastructure

- Pro: scales out, and each part can be replaced on its own.
- Con: many containers to run and upgrade, a JVM for the identity server and gigabytes of RAM for models. That is far beyond 8 GB and one engineer.

### Option B: One binary on SQLite (chosen)

- Pro: one container well under 1 GB, nothing to operate but a file, and the same image on the POC and the company server.
- Con: it cannot scale out (not needed), and some parts we would otherwise get from a product are ours to build.

## Decision

### Shape

1. **One Go binary, one running container.** Every feature lives in `cmd/ops-copilot`. Code follows ports and adapters: domain logic in `internal/<domain>/`, technology in `internal/<domain>/adapters/<tech>/`. The same source builds two images:
   - `ops-copilot`, distroless, for the `api` investigator;
   - `ops-copilot-cli`, which adds Node and a pinned Claude Code CLI and is not distroless, for the `cli` investigator.

   A deployment runs exactly one of the two, chosen by its investigator mode ([0003](0003-investigate-through-a-locked-down-tool-surface.md)).
2. **HTTP on one public port, plain HTTP.**
   - TLS belongs to whatever stands in front: the Cloudflare Tunnel for the POC, the company's proxy later.
   - `PUBLIC_URL` holds the external https origin. It is the OAuth issuer, the only origin reports link to, and the reason cookies are marked `Secure`.
   - Routes: `/api/*`, `/mcp`, `/oauth/*`, `/.well-known/*`, `/hooks/*` and `/healthz`; every other path returns the UI.
   - Times are stored in UTC and shown in `DISPLAY_TZ` (default `Asia/Ho_Chi_Minh`).

### Data

3. **SQLite** with the pure-Go driver `modernc.org/sqlite` (no CGO), in WAL mode, as one file on a volume. Uploaded files sit on the same volume.
   - One connection writes; four connections read. `busy_timeout` is 5 s. Every write transaction starts with `BEGIN IMMEDIATE`.
   - High-frequency bookkeeping, such as "last used" on tokens and keys, is written at most once a minute per item.
   - Migrations are numbered SQL files embedded in the binary and applied at start. They only move forward.
   - Before applying any pending migration, the binary writes a snapshot with `VACUUM INTO`. Rolling back means restoring that snapshot and running the previous image.
   - The binary refuses to start on a database whose schema is newer than the migrations it knows.
4. **Backups** are a daily `VACUUM INTO` snapshot plus a copy of the upload folder made with hard links (files are content-addressed and never change). Both are kept for 14 days on the volume. Copying the file of a running WAL database is not a backup. Moving snapshots off the machine is the host's job.
5. **Retention**, as env defaults:
   - investigations, their transcripts and reports: 90 days;
   - raw webhook payloads, stored after the secret filter: 30 days;
   - expired or revoked tokens: 30 days.

   OAuth clients that never finish an authorization follow their own 24-hour rule ([0002](0002-sign-in-with-google-and-an-embedded-oauth-server.md)).

   CVE findings stay while they are open.

### Jobs

6. **Jobs run on an in-process scheduler** backed by a job table. The jobs are OSV scans, document extraction, retention, backups, notifications and investigations.
   - A worker claims a job by setting it to `running` inside `BEGIN IMMEDIATE`.
   - Only one process runs, so at start every job still marked `running` goes back to `queued`. A job is tried at most 3 times, then it is marked failed. Notifications follow their own retry schedule instead (decision 12).
   - Investigations run at most `INVESTIGATION_CONCURRENCY` at a time (default 2), and at most 20 wait. Beyond that, an alert's investigation is created in the state `skipped`, and a notification says the queue was full.

### Projects

7. **A project groups what belongs to one system.** That is:
   - its New Relic app names and Azure resource groups;
   - its repositories on the company MCP;
   - its declared stack;
   - its report recipients.

   An alert joins the project whose app name or resource group matches it, otherwise the built-in project "Unassigned".

   Any member edits a project's name, New Relic apps, resource groups and stack. Only an admin edits:
   - its repositories, because they decide which code investigations may read ([0003](0003-investigate-through-a-locked-down-tool-surface.md));
   - its recipients. A recipient must be an allowlisted email ([0002](0002-sign-in-with-google-and-an-embedded-oauth-server.md)).
8. **One severity scale everywhere: critical, high, medium, low.**
   - New Relic priority maps one to one.
   - Azure Sev0 is critical, Sev1 high, Sev2 medium, and Sev3 or Sev4 low.
   - For OSV, the base score is computed from the advisory's CVSS vector, v3 if present, else v4. A score of 9.0 or more is critical, 7.0 or more high, 4.0 or more medium, and anything lower low. An advisory with no vector is medium.

### Features

9. **Documents** are uploaded through the UI: Markdown, PDF, DOCX and draw.io, up to 20 MB per file and 200 MB in total (env defaults).
   - Files are stored by SHA-256 hash; an identical upload is not indexed twice.
   - Extraction runs in a child process (the same binary with the `extract` subcommand), so a hostile file cannot stall or crash the server.
     - The child is killed after 60 s.
     - It runs with `GOMEMLIMIT=400MiB`. The parent checks the child's resident memory every second and kills it above 512 MB.
     - It gets an explicit, minimal environment and no secrets.
   - Any member can replace or delete a document. A deleted document keeps a record, so citations in old reports show it as deleted.
   - Markdown is used as is. PDF text comes from `github.com/ledongthuc/pdf`; a scanned PDF is marked "no text". DOCX text comes from `word/document.xml`. draw.io gives the labels of shapes and edges, per page.
   - Search is SQLite FTS5 (`porter unicode61`, BM25) over chunks split at headings. An `Embedder` port and a nullable vector column are reserved, so hybrid vector search can come later.
10. **Alerts arrive by webhook** from New Relic workflows and Azure Monitor action groups (authentication in [0002](0002-sign-in-with-google-and-an-embedded-oauth-server.md)).
    - A body is capped at 256 KB.
    - The dedupe key is the source plus the source's incident or alert id. An event whose key matches an investigation opened in the last 24 hours is attached to it instead of opening a new one. This is the only matching rule; [0003](0003-investigate-through-a-locked-down-tool-surface.md) uses it as is.
    - A "closed" or "resolved" event marks that investigation's alert as resolved; a running investigation continues.
11. **CVE scanning uses OSV** for the ecosystems it covers: npm, Packagist, Go and PyPI.
    - A project declares its stack by uploading `composer.lock`, `composer.json`, `package-lock.json`, `package.json`, `pnpm-lock.yaml`, `yarn.lock`, `go.mod` or `requirements.txt` (`go.sum` is not accepted, because it lists modules that are never built), or by entering packages by hand. A new upload of the same file name replaces the old one. Versions read from a manifest with ranges are marked as guessed.
    - Software outside those ecosystems, such as nginx or the PHP runtime, can be listed for reference but is not scanned, and the UI says so.
    - A scan runs daily and reports only new findings. Any member can start a scan, and can ignore a finding (giving a reason) or reopen it.
    - Repositories are not read directly, because the company GitLab needs a VPN.
12. **Notifications** go through a `Notifier` port to generic SMTP and to a Google Chat space incoming webhook.
    - A send is tried 4 times: at once, then after 1, 5 and 15 minutes. Then it is marked failed.
    - An admin can send a test message on each channel from Settings.
    - What a report may contain, and how it is rendered, is decided in [0003](0003-investigate-through-a-locked-down-tool-surface.md).

### UI

13. **The UI is a React + TypeScript single-page app built with Vite and embedded with `go:embed`.**
    - It uses React Router and TanStack Query, styled with `oc-glass.css` (a liquid-glass layer) over the SmartOSC BMS design-system tokens (`colors_and_type.css`), with no UI kit or CSS-in-JS.
    - Fonts (Inter, JetBrains Mono) are self-hosted from the binary.
    - API types are written by hand on both sides.
    - Every investigation, finding and document has its own URL, so a report can link straight to it.
    - The screen designs live in `design/ui/`.

### Delivery

14. **Build, test, lint and the frontend build run in a pinned Docker toolchain image.** The host needs only Docker, `make` and `git`, and CI runs the same targets.
15. **Images are pushed to GHCR with two tags each:** the commit SHA, and a moving `main` (or `main-cli`) tag for the latest build of `main`.
    - `deploy/install.sh` prepares a host: it writes the compose file, an env file from a template, and the data volume.
    - It also installs a systemd timer. Every 5 minutes the timer compares the digest of the chosen moving tag with the running image. If they differ, it pulls the new image, pinned by SHA, and restarts the container.
    - If `/healthz` does not answer within 60 s, the timer rolls back and stops updating until the next new image. If the failed image applied a migration, the timer first restores the pre-migration snapshot. It then starts the previous SHA.

### Out of scope

Actions, deploys and approvals, an audit chain, roles beyond those in [0002](0002-sign-in-with-google-and-an-embedded-oauth-server.md), a separate redaction service, and cloud infrastructure as code.

## Consequences

**Easier:**

- Runs on 8 GB with plenty of headroom. Backup and restore are a snapshot file plus the upload folder.
- No CA to install on clients, no JVM, and no model downloads; the stack starts in seconds.
- Moving from the POC to the company server is the same container with a different env.

**Harder:**

- One writer means a long write transaction blocks the others. Writes must stay short, and extraction never runs inside a transaction.
- Keyword search misses paraphrases until the optional vector work. Claude retrying with other words softens this.
- Declared stacks can drift from the code until someone uploads a new lockfile. Software outside OSV's ecosystems is not covered.
- Alerts that match no project land in "Unassigned" until someone maps them.
- A rollback after a failed migration loses whatever the failed image wrote in its short life.
- The repository has two languages, so CI builds the frontend before the Go binary.
