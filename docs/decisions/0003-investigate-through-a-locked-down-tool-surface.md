# 3. Investigate through a locked-down tool surface

Date: 2026-11-27

## Status

Amended by [0004](0004-settle-open-points-in-adrs-0001-0003.md)

## Context

When an alert arrives ([0001](0001-build-ops-copilot-as-one-go-binary-on-sqlite.md)), Ops Copilot asks Claude to investigate it. Claude uses three sources:

- Daniel's documents;
- New Relic, read-only;
- the company's MCP server, which holds the code and is reached with a static token.

The result is a report with a suggested fix. Nothing is ever executed.

For the POC, Daniel wants to run Claude Code headless on his own subscription. For production, once the company approves a budget, the tool should call the Anthropic API with a company key.

Everything Claude reads is untrusted input that may try to steer it: alert payloads, logs, documents and code. Claude Code ships with tools that act, such as a shell, file edits and web fetch. A report is also a way out: text an attacker plants in a log could make Claude write a link or an image that leaks code when someone opens the report. "Never acts" and "leaks nothing" therefore have to be enforced, not just asked for in a prompt.

### Option A: API only

- Pro: one path, and the tool loop runs in-process.
- Con: no API budget exists yet for the POC.

### Option B: Claude Code CLI only

- Pro: no tool loop to write.
- Con: needs Node and a credential in the image, and depends on CLI behaviour that changes between versions.

### Option C: Both, behind one port, with the same tools (chosen)

- Pro: the POC runs today on the subscription; production switches by env and image, with no code change.
- Con: two adapters to keep equivalent.

## Decision

### Modes

1. **An `Investigator` port with two adapters**, chosen by `INVESTIGATOR`:
   - `cli` (POC) runs `claude -p` headless on Daniel's subscription, in the `ops-copilot-cli` image ([0001](0001-build-ops-copilot-as-one-go-binary-on-sqlite.md)).
   - `api` (production) runs the tool loop in-process with the Anthropic Go SDK and a company API key.

   The model is `MODEL`, default `claude-opus-5-5`.

### Tools

2. **One internal MCP endpoint serves every tool.** It listens on `127.0.0.1` on its own port, never on the public one. Each run gets a random token, held in memory for that run only; there is no stored key for the investigator. The `api` adapter calls the same tools in-process.
3. **The tools, and their limits:**
   - **Documents:** `search_docs` and `get_doc`. `get_doc` returns one section or page at a time.
   - **New Relic:**
     - NRQL queries through the NRQL query endpoint, with `LIMIT 100` added when the query has none;
     - fixed lookups of entities, deployments and alert incidents.

     No tool accepts raw NerdGraph or GraphQL, because the User API key is not read-only.
   - **Company MCP proxy:**
     - It forwards only the tool names listed in `COMPANY_MCP_TOOLS`; empty means none.
     - It forwards only calls whose repository argument, named by `COMPANY_MCP_REPO_ARG`, exactly matches one of the run's project's repositories. A call without that argument is rejected.
     - For a run in the "Unassigned" project, no company MCP tool is forwarded.
   - **Every tool result** is cut at 32,000 characters, with a note saying so. Tools that can return more take an offset or a page.
4. **The `cli` adapter is locked down by an allow-list.**
   - Each run passes an explicit list of allowed tools: the internal server's tools only.
   - It also passes an empty set of built-in tools, a strict MCP config naming only the internal endpoint, and no user or project settings.
   - The exact flags are pinned in code together with the CLI version.
   - The CLI reports its tool list when a session starts. If that list holds anything outside the internal server, the run is aborted before any tool call.
   - The same check runs once when the binary starts; if it fails, investigations are disabled and admins are notified.
   - The CLI runs as a non-root user, in an empty working directory, on a read-only root filesystem. The `claude login` credential directory is its only writable mount, so it can refresh its token.
   - The CLI process gets an explicit, minimal environment: `HOME`, `PATH`, the credential directory and the per-run token. It never inherits the server's secrets, such as the company MCP token, the New Relic key, the SMTP password or the Google client secret.
   - Sessions are not kept: after each run the binary deletes any session files the CLI wrote.
   - When the credential has expired, the run fails with the reason "CLI credential expired" and admins are notified.

### What goes in and what comes out

5. **A secret filter runs in-process, and fails closed.**
   - It runs on:
     - the alert payload, before it is stored or put into the first prompt;
     - every tool result, before Claude sees it;
     - every report, before it is stored or sent.
   - It matches:
     - known token formats (AWS, GitHub, GitLab, Azure, Google, Slack, private keys, JWTs);
     - values of keys named like password, secret, token or key;
     - connection strings with credentials;
     - high-entropy strings of 32 characters or more in free text.

     Fields our own tools return as identifiers (trace IDs, commit SHAs, entity GUIDs) are exempt from the entropy rule, so Claude can use them in its next call. So are 40-character hexadecimal strings, which are commit SHAs, in any tool's output.
   - Each match becomes `[REDACTED:<kind>]`.
   - If the filter fails:
     - on a tool result, the result is dropped and the step is recorded as failed;
     - on an alert payload or a report, nothing is stored or sent and the run fails.
6. **A report is structured, and rendered as text.**
   - A report has four parts:
     - a summary;
     - the finding;
     - the evidence, which is the list of tool calls with their results;
     - a suggested fix, as text or a unified diff.
   - The UI shows every part as text, and a diff as a diff. No Markdown is rendered as HTML. The UI applies exactly two transforms:
     - text in backticks is shown in the mono font;
     - `doc N` becomes a link to that document's page.

     Any other URL in Claude's output is shown as plain text, never as a link or an image.
   - Notifications carry less than the report:
     - Email (plain text) and the Google Chat card hold only the title, severity, project and summary, plus one link to the investigation's page on `PUBLIC_URL`.
     - Before sending, every URL that is not on `PUBLIC_URL` is replaced by `[link removed]`, and Chat fields are HTML-escaped. A URL here includes a bare domain name with no scheme, which mail clients also turn into links.
     - The finding, the evidence and the fix are read in the UI only.
   - No tool anywhere writes to a system, so a suggested fix stays a suggestion.

### Runs

7. **An investigation has one or more runs.**
   - A webhook alert creates an investigation and its first run, unless it matches an investigation under the dedupe rule of [0001](0001-build-ops-copilot-as-one-go-binary-on-sqlite.md). In that case it is attached to that investigation.
   - The investigation keeps its id and URL. Its state is the state of its latest run.
   - Any member can:
     - stop a queued or investigating run;
     - run the investigation again, which adds a new run;
     - resend the latest finished run's report to the project's recipients.

     Each of these is recorded on the run, with the person's email.
   - There is no manual start without an alert.
8. **Run states:** queued, investigating, reported, failed, stopped and skipped (the queue was full).
9. **Budgets per run**, as env defaults:
   - 200k tokens, counting output tokens plus input tokens not read from the prompt cache (the `api` adapter uses prompt caching, so both adapters count alike);
   - 30 turns;
   - 10 minutes.

   A run that reaches one ends as failed and keeps its partial finding.

## Consequences

**Easier:**

- Prompt injection in an alert, a log or a file cannot run a command, fetch a URL or reach a repository outside the project, because Claude has no such tools.
- A report cannot leak data through a link or an image, because nothing in it is rendered as a link except links to Ops Copilot itself.
- Both modes see the same filtered data, so a report from the POC is a fair preview of production.
- Moving to production is an image and env change.

**Harder:**

- **In `cli` mode, content goes to Anthropic under Daniel's personal subscription**, including code read from the company MCP. Daniel accepts this for the POC. Production uses `api` with a company key.
- The two adapters must stay equivalent; a shared test set runs against both.
- The secret filter will miss some secrets and mask some harmless strings. Patterns need tuning as reports come in.
- A CLI upgrade can change flags or tools, so a version bump is a reviewed change, and the start-up check guards against a silent change.
- The repository-argument rule depends on how the company MCP names that argument. If its tools change, `COMPANY_MCP_REPO_ARG` and `COMPANY_MCP_TOOLS` change with them.
