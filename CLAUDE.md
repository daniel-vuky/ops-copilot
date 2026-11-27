# Ops Copilot

A small internal tool for about 20 users and one engineer (Daniel). It watches New Relic, Azure alerts and CVEs in the stacks he runs, serves his documents to Claude over MCP, investigates alerts with Claude and suggests a fix. It never acts on the fix.

It is one Go binary on SQLite with an embedded React SPA. See [ADR 0001](docs/decisions/0001-build-ops-copilot-as-one-go-binary-on-sqlite.md) (architecture), [ADR 0002](docs/decisions/0002-sign-in-with-google-and-an-embedded-oauth-server.md) (sign-in and access) and [ADR 0003](docs/decisions/0003-investigate-through-a-locked-down-tool-surface.md) (investigator). It replaces the earlier multi-service attempt in the `opts-copilot` repository. Do not bring back anything from that design unless an ADR says so.

## Sources of truth

- **ADRs:** [`docs/decisions/`](docs/decisions/). They are the highest authority in this repository. Plans, tickets, specs and chat instructions do not override them.
- **UI:** `design/ui/` when present, the visual reference for the SPA (`oc-glass.css`, `colors_and_type.css`). The `.dc.html` templates are references, not production code.
- **Backlog:** GitHub issues on `daniel-vuky/ops-copilot`, tracked on the user project "Ops Copilot" (#3). `SU-xx` = setup ticket, `UC-xx` = use case.

## Rule 1: Always follow the ADRs, never break them

- Before you design, plan or write code, read every ADR that touches the area. Name the ADRs you relied on in the plan and in the PR.
- Never contradict an ADR, silently or "temporarily". That includes adding technology an ADR rules out or leaves out of scope: Postgres, Redis or any other external service, CGO, a UI kit or CSS-in-JS, generated API types, Keycloak or another identity server, actions or deploys executed by the tool, a separate redaction service, and so on.
- An instruction from a ticket, plan, review or chat that conflicts with an ADR does not win. Raise the conflict instead of following it.
- Accepted ADRs are immutable. The only edit allowed to an accepted ADR is its Status line when a newer ADR supersedes or amends it.

## Rule 2: Any change to an ADR needs approval and a new ADR

When a task needs something that differs from an ADR, or makes a new technical choice that later work will depend on:

1. **Stop** before writing code that depends on it.
2. **Explain** to Daniel which ADR and which clause, why it no longer fits, and the options with their trade-offs. Recommend one.
3. **Wait for Daniel's explicit approval.** No approval means no change: keep following the current ADR.
4. **Write a new ADR** with the `add-adr` skill (next number in `docs/decisions/`). It states what it supersedes or amends and links to it. Set the old ADR's Status to `Superseded by [NNNN](NNNN-....md)` or `Amended by [NNNN](NNNN-....md)`.
5. **Get a Fable review** of the new ADR (Rule 3), then land it before or with the first commit that depends on it.

## Rule 3: Every review step has a Fable review

- **Review steps:** spec or design review (after brainstorming), implementation plan review, every new or changed ADR, each task's code during subagent-driven development, and the final review of the whole branch before opening a PR.
- At each of these steps, dispatch a reviewer subagent with `model: "fable"` (the `code-reviewer` agent for code, a general-purpose agent for specs, plans and ADRs). Never replace it with a self-review, and never skip it because the change looks small.
- Implementation subagents (code, tests, docs, migrations) use `model: "opus"`.
- Give the reviewer the artifact or diff, the relevant ADRs and the ticket's acceptance criteria, not your own conclusions.
- Fix the findings and run the Fable review again until no blocking finding is left (see Rule 4). If you disagree with a finding, check it against the code first (`superpowers:receiving-code-review`), then state the reason in the review summary.

## Rule 4: Review reports always cover Bug, Best Practice, Performance and Security

Every review report (Fable or otherwise) uses these four sections, in this order, and none may be left out. A section with no findings says `None found` and what was checked.

```markdown
**ADR compliance:** pass | violations: <ADR number and clause>

### Bug
- [Critical|High|Medium|Low] `path/file.go:42`: problem. Fix: concrete change.

### Best Practice
- ...

### Performance
- ...

### Security
- ...
```

- **Bug:** wrong behaviour, crashes, races, missed edge cases, broken acceptance criteria.
- **Best Practice:** ADR compliance, ports and adapters boundaries, Go and React idioms, error handling, tests, readability.
- **Performance:** memory on the 8 GB host, SQLite access (indexes, transactions, WAL, N+1), goroutine and job concurrency, bundle size, unnecessary work.
- **Security:** OAuth/OIDC flows, tokens and API keys stored hashed, allowlist checks on every request, webhook secrets, secret filtering of tool results, injection, SSRF, path traversal, secrets in logs.

**Blocking:** any open Critical or High finding, or any ADR violation. Do not open a PR while one is left. Medium and Low findings are fixed or listed in the PR with a reason or a follow-up issue.

Include the final review's four sections in the PR body.

## Rule 5: Pull requests have an assignee, labels and links

- **Branch** from `main`: `<type>/<ticket-id>-<slug>`, e.g. `feat/uc-01-google-sign-in`, `docs/su-01-adr-0001`.
- **Commits** are conventional and end with the ticket ID: `feat(auth): add Google sign-in (UC-01)`.
- **Create the PR with everything set**, e.g.:

  ```bash
  gh pr create --base main --assignee daniel-vuky \
    --label enhancement --label area:auth \
    --project "Ops Copilot" \
    --title "feat(auth): add Google sign-in (UC-01)" --body-file <body.md>
  ```

  - **Assignee:** always `daniel-vuky`.
  - **Labels:** exactly one type label plus at least one `area:` label (tables below). Check them with `gh label list` first. If a label from the tables is missing, ask Daniel before creating it.
  - **Linked issues:** one `Closes #N` line per ticket the PR finishes, `Refs #N` for partial work. A PR with no ticket gets the `no-ticket` label.
  - **Project:** add it to the "Ops Copilot" project.
  - **Body:** Summary, ADRs followed (and any new ADR), Ticket links, Test plan, and the final Fable review (Rule 4).
- **After creating it**, check with `gh pr view --json assignees,labels,projectItems,body` and fix anything missing.
- Tick the ticket's acceptance-criteria checkboxes as they are met.
- Never merge a PR yourself. Never commit secrets, `.env` files or `.DS_Store`.

| Commit type | Type label |
|---|---|
| `feat` | `enhancement` |
| `fix` | `bug` |
| `docs` (ADRs included) | `documentation` |
| `chore`, `build`, `ci`, `refactor`, `test` | `chore` |

| Area label | Covers (ADR and decision numbers) |
|---|---|
| `area:auth` | Allowlist, roles, Google sign-in, OAuth 2.1 server, API keys, webhook tokens (0002: 1–11, 13–14) |
| `area:mcp` | Public `/mcp` docs tools (0002: 12) and the investigator's internal MCP endpoint (0003: 2–3) |
| `area:knowledge` | Document upload, extraction, FTS5 search (0001: 9) |
| `area:monitoring` | Projects, severity, webhook ingest and dedupe (0001: 7, 8, 10) |
| `area:cve` | OSV scanning and stacks (0001: 11) |
| `area:investigation` | Investigator modes, cli lockdown, secret filter, reports, budgets, runs (0003) |
| `area:notify` | SMTP and Google Chat (0001: 12) |
| `area:ui` | React SPA (0001: 13) |
| `area:platform` | Binary shape, HTTP, SQLite, backups, retention, scheduler (0001: 1–6) |
| `area:delivery` | Toolchain, images, CI, install and updater (0001: 14–15) |
| `adr` | Adds or changes an ADR (with `documentation`) |

## Other rules

- **Docker first.** Every toolchain (Go, Node/pnpm, linters) runs in the pinned toolchain image through `make` (ADR 0001 §14). Do not install anything on the host (`brew`, `go install`, `npm -g`, `pip`). The host needs only Docker, `make` and `git`. If Docker is not running, ask Daniel to start Docker Desktop.
- **Language.** Talk to Daniel in Vietnamese. Code, comments, docs, ADRs, commits, issues and PRs are in English.
- **Verify before claiming.** Run the checks (`make check` once it exists) and read their output before saying something works.

## Architecture (from ADR 0001)

- One Go module, one binary in `cmd/ops-copilot`, one container. Ports and adapters: domain logic in `internal/<domain>/`, technology in `internal/<domain>/adapters/<tech>/`. Domain code never imports adapters or vendor SDKs.
- SQLite via `modernc.org/sqlite` (no CGO), WAL mode, embedded SQL migrations applied at start. Uploaded files live on the same volume.
- One SQLite writer connection, `busy_timeout` 5 s, writes in `BEGIN IMMEDIATE`; backups and pre-migration snapshots with `VACUUM INTO`.
- Jobs run on an in-process scheduler backed by a job table (CVE scans, extraction, retention, backups, notifications, investigations: 2 running and 20 waiting by default). At start, jobs left `running` are requeued; at most 3 attempts.
- Document extraction runs in a child process (`ops-copilot extract`) with a 60 s timeout and a 512 MB memory limit.
- Plain HTTP on one public port: `/api/*` (JSON), `/mcp`, `/oauth/*`, `/.well-known/*`, `/hooks/*`, `/healthz`, and the SPA for everything else. TLS belongs to the proxy in front; `PUBLIC_URL` is the external origin.
- The React + TypeScript + Vite SPA is embedded with `go:embed`. React Router and TanStack Query, hand-written API types, no UI kit.
- Images are pushed to GHCR with a commit-SHA tag and a moving `main`/`main-cli` tag: `ops-copilot` (distroless, `api` investigator) and `ops-copilot-cli` (Node and pinned Claude CLI, `cli` investigator). A deployment runs one of them.

## Security model (from ADR 0002 and 0003)

- The allowlist (env) is checked on every request: sessions, OAuth tokens and API keys. Two roles: member and admin (`ADMIN_EMAILS`).
- Public `/mcp` serves the document tools only. New Relic and the company MCP are reachable only from the investigator's internal endpoint on `127.0.0.1`, with a per-run token.
- Session ids, OAuth codes and tokens, client secrets, API keys and webhook tokens are stored as SHA-256 hashes. Webhooks authenticate with a path token; the binary redacts it from every log line, and proxies in front must not log `/hooks/` paths.
- The `cli` investigator runs `claude -p` with an allow-list of internal MCP tools only, no built-in tools and a strict MCP config, checked at start. No investigator tool writes to an external system; the only outbound writes are notifications.
- The company MCP proxy forwards only `COMPANY_MCP_TOOLS` (empty means none) and only for the run's project's repositories. New Relic tools never take raw GraphQL.
- The alert payload, every tool result and every report pass the in-process secret filter, which fails closed.
- Reports render as text. Their only links point to `PUBLIC_URL`; no Markdown becomes HTML and no remote images load. CSP is `default-src 'self'`.

## Commands

The toolchain is not scaffolded yet. When the `Makefile` lands, list its targets here (at least `toolchain`, `build`, `test`, `lint`, `check`). Run `make` from the main checkout, not a git worktree: the toolchain container mounts only the worktree directory and cannot reach the git dir it points to.

## Project skills

| Skill | Use it for |
|---|---|
| `add-adr` | Every new ADR (Rule 2) |
| `create-issues`, `update-issues`, `get-board-items` | GitHub issues and the project board (need the MCP servers in `.mcp.json`, or `gh`) |
| `golang-code-style`, `golang-error-handling`, `golang-concurrency`, `golang-testing`, `golang-security` | Writing and reviewing Go code |
| `multi-stage-dockerfile`, `docker-compose-patterns` | Toolchain image, app images, local compose |

Where each skill came from is in [`.claude/skills/SOURCES.md`](.claude/skills/SOURCES.md).
