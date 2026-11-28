# 6. Set conventions for the job scheduler

Date: 2026-11-28

## Status

Accepted

Amends [0001](0001-build-ops-copilot-as-one-go-binary-on-sqlite.md) §4, §6 and §11, and clarifies 0001 §5.

## Context

Building the job scheduler ([SU-06 #12](https://github.com/daniel-vuky/ops-copilot/issues/12)) raised points that 0001 §6 leaves open. Six later tickets register job kinds on it: document extraction ([UC-02 #17](https://github.com/daniel-vuky/ops-copilot/issues/17)), backups ([SU-10 #25](https://github.com/daniel-vuky/ops-copilot/issues/25)), notifications ([UC-15 #32](https://github.com/daniel-vuky/ops-copilot/issues/32)), OSV scans ([UC-16 #33](https://github.com/daniel-vuky/ops-copilot/issues/33)), investigations ([UC-21 #38](https://github.com/daniel-vuky/ops-copilot/issues/38)) and retention ([SU-12 #43](https://github.com/daniel-vuky/ops-copilot/issues/43)). Each handler is written against the scheduler's contract, so changing it later means reviewing all of them:

1. 0001 §6 requeues a job left `running` at start, but does not say whether a handler can run twice for the same work, or whether the interrupted attempt counts toward the 3.
2. 0001 §4 and §11 need daily jobs, but nothing says what "daily" is aligned to, or what happens to a period the process was down for, or to a period whose job is still running when the next one starts.
3. 0001 §6 limits investigations to 20 waiting without saying what counts as waiting.
4. 0001 §6 sets 3 attempts but no delay between them, and no bound on how long one attempt may run.
5. Nothing says what a job's payload and error may hold, though both are stored, logged and copied into every backup.

### Option A: Settle each point in the code of SU-06

- Pro: no new document.
- Con: six tickets would build on rules no ADR records, and a handler written without knowing it may run twice can send a notification twice or corrupt its own rows.

### Option B: One ADR for the scheduler contract (chosen)

- Pro: every job kind has one place to read what the scheduler promises and what it asks of handlers.
- Con: one more ADR.

Daniel chose option B and approved the decisions below, which were proposed in the design of SU-06.

## Decision

1. **Delivery is at least once (amends 0001 §6).** A handler may run again for work it already did: after a restart, after a shutdown that cut it off, or when its finish could not be recorded. Every handler must be idempotent, for example by checking its own row's state before acting. The scheduler never runs one job in two handlers at the same time: a claim sets the row to `running` in one `BEGIN IMMEDIATE` transaction (0001 §6), and a `running` row whose handler has not returned is never requeued, except at start.
2. **An interrupted attempt counts (clarifies 0001 §6).** Attempts are counted when a job is claimed. An attempt cut off by a restart, by shutdown or by a lost finish stays counted, so a job that crashes the process cannot loop forever, and a requeued investigation keeps its spend (0004 §6). A job requeued after its last attempt is marked failed without running.
3. **A failed or cancelled job tells its owner in the same transaction (amends 0001 §6).** A job kind may give a hook that runs inside the write transaction that marks the job failed or cancelled, so the owning row (a document, a notification, an investigation) reaches its final state atomically. The hook does database work only (0005 §2).
   - Because delivery is at least once (decision 1), a job can be marked failed after its last attempt did its work but its finish was lost. The hook must therefore move the owning row only if that row is not already final.
   - A queued job is cancelled in the caller's write transaction; a running one is stopped through its context. Neither is retried, and both end as cancelled, unless the handler returns, or had already returned, without error, which is recorded as succeeded.
   - States in between, such as "Retrying" with a retry count or an investigation back in the queue after a restart, are the owning domain's to record: the handler records its attempt in its own write transaction, and the domain resets its own rows at start before the scheduler runs.
4. **Attempts, delays and time limits (amends 0001 §6).** The default is 3 attempts (0001 §6), 30 s and then 2 min apart. Any job kind may replace them with its own list of delays: notifications use 1, 5 and 15 min, so 4 attempts (0001 §12). One attempt runs at most 15 min by default; a job kind may set its own limit. The limit cancels the handler's context; a handler that ignores it keeps its running slot until it returns or the process restarts. A handler error marked permanent skips the remaining attempts.
5. **Recurring jobs (amends 0001 §4, §6 and §11).** A recurring kind has a period (at least 1 minute) and an offset, both in whole seconds; periods are aligned to the Unix epoch in UTC, so a daily job at 03:00 in `Asia/Ho_Chi_Minh` is a 24 h period with a 20 h offset.
   - The job table holds at most one job per kind and period, enforced by a unique index, so a restart or a repeated check never queues a second job for a period.
   - Only the current period is queued. Periods that passed while the process was down are not caught up; the current one runs as soon as the process is back.
   - A period is skipped while an earlier job of the same kind and key is still queued or running, so a slow job does not pile up. The skip is not recorded: if the process restarts later in that period and no such job is active any more, the period's job is queued then.
   - A recurring kind can also be queued by hand, for example "Scan now" (0001 §11); a job queued with the same key as an active one is refused, so a manual and a scheduled run never overlap.
   - A forward clock jump can run a period early, and that period is then not run again when real time reaches it.
6. **Waiting means queued and not about to start (clarifies 0001 §6).** A kind's waiting count is its queued jobs, including those held back by a retry delay, minus its free running slots. So investigations allow 2 running and 20 waiting even in a burst (0001 §6). Beyond the limit, queueing is refused and the caller decides what to record (0001 §6: the investigation is created `skipped`).
7. **Payloads and errors.** A payload is at most 64 KiB and holds references (ids), not content, and is never logged. A handler's error text (or panic value) is stored as the job's last error and written to the log, cut to 1 KiB. Before that, the scheduler scrubs it: anything shaped like a URL loses its user info, path and query, since Go's HTTP errors quote the full request URL, and a Google Chat webhook URL carries its key and token. When the secret filter ([UC-11 #28](https://github.com/daniel-vuky/ops-copilot/issues/28)) lands, it is applied to error text as well; UC-11 decides whether the URL scrubber stays. Handlers still must not put secrets in errors; the scrubber is the second line.
8. **Shutdown (clarifies 0001 §6).** On SIGTERM the scheduler stops claiming at once, gives running handlers 6 s to finish, then cancels them and waits 2 s more. 8 s in all fits Docker's default 10 s stop grace, alongside the HTTP drain. A handler still running then is requeued at the next start (decision 2).
9. **Finished job rows are kept (clarifies 0001 §5)** until a later ADR sets their retention ([#50](https://github.com/daniel-vuky/ops-copilot/issues/50)). That ADR must not delete a recurring job's row before its period has ended, or a restart would queue that period again.

## Consequences

**Easier:**

- Every job kind is written against one stated contract, and the database itself enforces "one job per period" and "one active job per key".
- A process that dies, or an update that restarts it mid-job, loses no queued job.
- The owning domain's rows never stay stuck in "Extracting", "Retrying" or "Investigating" after the job gave up.

**Harder:**

- Every handler must be idempotent; a notification handler that cannot tell whether its last send went through may send twice.
- A job that needs a fourth try after three restarts fails, even if the restarts were planned updates.
- A missed daily period is not caught up: a host down at 03:00 runs that day's backup when it comes back, but a host down for two days loses one day's run.
- Handlers carry the burden of keeping secrets out of their error text until UC-11 adds a filter.
- Finished job rows, with their payloads and last errors, grow and are copied into backups until #50 lands.
- A fixed UTC offset drifts from a local wall-clock time if `DISPLAY_TZ` ever uses daylight saving time; `Asia/Ho_Chi_Minh` does not.
- A manual run still active at a period boundary takes the place of that period's scheduled run.
- A handler that ignores its context keeps its slot past the time limit; with one slot, its kind stops until a restart.
- Queued jobs of a kind no longer registered stay queued, with only a warning at start.
- Owner hooks must use conditional updates, and owning domains record their own in-between states.
