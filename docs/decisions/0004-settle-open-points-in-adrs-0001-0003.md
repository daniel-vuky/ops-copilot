# 4. Settle open points in ADRs 0001–0003

Date: 2026-11-27

## Status

Accepted

Amends [0001](0001-build-ops-copilot-as-one-go-binary-on-sqlite.md), [0002](0002-sign-in-with-google-and-an-embedded-oauth-server.md) and [0003](0003-investigate-through-a-locked-down-tool-surface.md).

## Context

Breaking ADRs 0001–0003 into backlog tickets surfaced seven points the ADRs leave open. Each blocks a ticket, because two builders could reasonably build it two ways and later work depends on the choice:

1. 0002 §13 stores API keys only as hashes, but the Settings design shows each key's prefix so a person can tell keys apart. ([UC-08 #23](https://github.com/daniel-vuky/ops-copilot/issues/23))
2. 0001 §7 says an alert joins "the project" whose New Relic app name or Azure resource group matches, without saying whether two projects may list the same name. ([UC-10 #27](https://github.com/daniel-vuky/ops-copilot/issues/27))
3. 0001 §10 says what a "closed" or "resolved" event does when it matches an investigation, not when it matches none. ([UC-13 #30](https://github.com/daniel-vuky/ops-copilot/issues/30))
4. 0001 §11 sets no size limit for stack file uploads. ([UC-14 #31](https://github.com/daniel-vuky/ops-copilot/issues/31))
5. 0001 §11 says a scan "reports only new findings" without saying how, and names no state for a finding that stops appearing. ([UC-16 #33](https://github.com/daniel-vuky/ops-copilot/issues/33))
6. 0001 §6 requeues a job left `running` at start (at most 3 attempts), and 0003 §9 sets per-run budgets, but neither says whether a requeued run starts its budget again. ([UC-21 #38](https://github.com/daniel-vuky/ops-copilot/issues/38))
7. 0001 §5 deletes "expired or revoked tokens" after 30 days, without saying whether API keys and replaced webhook tokens count. ([SU-12 #43](https://github.com/daniel-vuky/ops-copilot/issues/43))

Daniel chose each answer below from proposed options.

## Decision

1. **API key display prefix (amends 0002 §13).** A key is stored as its SHA-256 hash plus a display prefix: `oc_` and the first 4 characters after it. Settings shows the prefix; the full key is still shown only once.
2. **One project per alert source name (amends 0001 §7).** A New Relic app name or an Azure resource group belongs to at most one project. Saving a name that another project already has is refused. When one alert names resources of more than one project (several New Relic entities, or several Azure targets), the first resource in the alert's own order that matches a project decides the project.
3. **Unmatched "resolved" events (clarifies 0001 §5, §10).** A "closed" or "resolved" event that matches no investigation opened in the last 24 hours opens nothing. It is logged by source and dedupe key, never with its payload, and its payload is not stored.
4. **Stack file size (amends 0001 §11).** A stack file upload is limited to 10 MB per file, as an env default. A larger upload is refused while it is read.
5. **New CVE findings and resolved findings (amends 0001 §11).**
   - After a scan that completes, if a project has at least one new finding, one notification goes to that project's recipients through the `Notifier` (0001 §12). It holds the project, the number of new findings by severity, and one link to the project's CVE page on `PUBLIC_URL`. As for report notifications (0003 §6), every other URL is replaced by `[link removed]` and Chat fields are HTML-escaped. The notification is queued in the same write transaction that stores the scan's findings, so a retried scan cannot lose it. A scan with no new finding sends nothing, and ignored findings are never notified.
   - An open finding that a later completed scan no longer finds becomes **resolved** and leaves the open list. If a later scan finds it again, it becomes open again, is marked New on the CVE screen and is notified as new. An ignored finding stays ignored whether or not later scans find it.
6. **Budgets across a requeue (amends 0003 §9).** A run requeued after a restart keeps its recorded spend: token and turn counts, recorded after each turn, carry over, and the 10-minute limit counts the recorded running time of every attempt. Because sessions are not kept (0003 §4) and both adapters behave alike (0003 Consequences), a requeued run starts its investigation again within what is left of its budget.
7. **Retention of keys and webhook tokens (clarifies 0001 §5).** Retention also deletes API keys that expired or were revoked, and webhook tokens that were replaced by rotation, after the same 30-day period as tokens (0001 §5).

## Consequences

**Easier:**

- The seven tickets above can be built without guessing, and their acceptance criteria can be stated exactly.
- A New Relic app or Azure resource group can no longer be routed to two projects by its configuration.
- A run's recorded cost cannot restart from zero after a restart.
- People learn about new CVEs without opening the UI, and the CVE list shows only what is still open.

**Harder:**

- Two systems that share a New Relic app name cannot both have it; the alerts land in one project.
- An alert that names resources of two projects is investigated in one of them only, the one whose resource the alert lists first.
- A requeued run that had spent most of its budget may fail soon after it restarts, with a short partial finding. Spend not yet recorded when the process died is lost.
- Resolved findings are kept with no retention limit; 0001 §5 covers only open findings, and a later decision may set one.
- A 10 MB cap limits upload size, not the parser's work on a hostile file; the parser must bound nesting and expansion.
