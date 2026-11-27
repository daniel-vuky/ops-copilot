# Skill sources

Where each project skill came from, so it can be updated or audited later. All of them were moved from the earlier `opts-copilot` repository on 2026-11-27.

## Copied from sales-and-ordering-guide (internal)

| Skill | Notes |
|---|---|
| add-adr | Uses `.adr-dir` at repo root (`docs/decisions`) |
| create-issues | Needs the `github` MCP server (`.mcp.json`) |
| update-issues | Needs the `github` MCP server |
| get-board-items | Needs the `gh-projects` MCP server, or `gh` CLI |

## Third-party (reviewed 2026-11-23 before install)

| Skill | Source | Commit | License |
|---|---|---|---|
| golang-code-style | samber/cc-skills-golang | d68d80a | MIT |
| golang-testing | samber/cc-skills-golang | d68d80a | MIT |
| golang-error-handling | samber/cc-skills-golang | d68d80a | MIT |
| golang-security | samber/cc-skills-golang | d68d80a | MIT |
| golang-concurrency | samber/cc-skills-golang | d68d80a | MIT |
| multi-stage-dockerfile | github/awesome-copilot | f10fdb8 | MIT |
| docker-compose-patterns | docker/skills | caeee20 | Apache-2.0 |

Local changes from upstream:
- `evals/` folders and `docker-compose-patterns/agents/` removed (authoring and test data, not used at runtime).
- `allowed-tools` removed from the five `golang-*` skills. Upstream pre-approves `Bash(git:*)`, `WebFetch`, `Agent` and others while the skill is active; removing it keeps the session's normal permission prompts.

Not carried over from `opts-copilot`:
- `pgvector-semantic-search`: ADR 0001 replaces Postgres + pgvector with SQLite FTS5.
