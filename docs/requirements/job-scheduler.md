# Orbit — Job Scheduler Requirements

| | |
|---|---|
| Status | Draft (rev 6) |
| Author | aneesh-aparajit |
| Last updated | 2026-10-01 |
| Related | [design.md](design.md): code structure · [memory-optimizations.md](memory-optimizations.md): memory and storage work |

## 1. Overview

Orbit is a small, lightweight job scheduler written in Go. A client submits a job
over HTTP and gets back a reference ID. A job either runs **once** (optionally after
a delay) or runs on a **schedule** defined by a cron expression. Every execution is
stored as a row in a `tasks` table, and the client polls that row to follow its
progress. The work itself is Go code; the first handlers just print to the terminal.

This is a learning project. The goal is to understand how schedulers work
(timers, priority queues, worker pools, state machines, cron evaluation, and
execution guarantees), not to compete with production systems.

## 2. Goals

- Accept jobs over an HTTP API and return a unique reference ID.
- Let clients poll a task's status by ID.
- Run Go handlers: simple ones in v1, such as printing a message.
- Support one-off jobs with an optional delay.
- Support recurring jobs defined by cron expressions.
- Persist schedules and tasks, so a restart loses nothing.
- Never execute the same task twice, including when a schedule is cancelled
  concurrently or when fire times are missed.

## 3. Non-goals (for now)

- Running across multiple nodes, leader election, or distributed locking.
- Authentication, authorization, multi-tenancy.
- Running arbitrary untrusted code safely (sandboxing).
- A web UI.

## 4. Glossary

| Term | Meaning |
|---|---|
| **Handler** | A named piece of Go code the server can run (e.g. `print`). |
| **Run type** | `ONCE` or `SCHEDULE`. Decides whether a request takes a `delay` or a `cron`. |
| **Schedule** | A recurring definition: handler + payload + cron expression. Stored in `schedules`. |
| **Task** | One concrete execution at one point in time. Stored in `tasks`. A `ONCE` job creates exactly one task. A schedule creates one task per fire time. |
| **PQ** | The in-memory priority queue (min-heap) of upcoming tasks, ordered by `run_at`. It holds only tasks inside the read-ahead window. |
| **Read-ahead window** | How far ahead the dispatcher loads tasks into the PQ (`read_ahead`, default 60 s). Tasks due later stay only in the database until a refill reaches them (§6.2.1). |
| **Claim** | The atomic database update that moves a task from `SCHEDULED` to `QUEUED`. Only a claimed task may run. |

## 5. Functional requirements

### FR-1 Submit a job

Every job is submitted through one endpoint, and `run_type` decides what else the
request must contain:

| `run_type` | Required | Optional | Rejected |
|---|---|---|---|
| `ONCE` | `handler` | `payload`, `delay`, `timeout` | `cron`, `timezone` |
| `SCHEDULE` | `handler`, `cron` | `payload`, `timezone`, `timeout`, `misfire_policy` | `delay` |

- The server validates the request: the handler exists, the payload is valid JSON,
  the fields match the run type, the `delay` is within bounds, and the cron
  expression parses and has at least one future fire time.
- It returns `201 Created` without waiting for execution. On a validation failure
  it returns `400` with a readable error.
- **`ONCE`:** insert one `tasks` row with `run_at = now + delay`, then offer it to
  the dispatcher, which admits it to the PQ only if it falls inside the read-ahead
  window (§6.2.1).
- **`SCHEDULE`:** in one transaction, insert the `schedules` row and a `tasks` row
  for the first fire time, `run_at = Next(now)`. After the commit, offer the task
  to the dispatcher in the same way.

### FR-2 Reference IDs

- IDs are opaque, unique and sortable by time (UUIDv7 or ULID), with a type prefix:
  `tsk_...` for tasks and `sch_...` for schedules.
- A `ONCE` submission returns a `task_id`.
- A `SCHEDULE` submission returns a `schedule_id`, plus the `task_id` of the first
  execution.

### FR-3 Poll status

- `GET /tasks/{id}` returns a task's status and metadata: `id`, `schedule_id`,
  `handler`, `status`, `run_at`, `created_at`, `started_at`, `finished_at`,
  `attempts`, `output` and `error`.
- `GET /schedules/{id}` returns the schedule's status, cron expression, `next_run_at`
  and its most recent tasks.
- An unknown ID returns `404`.

#### Task lifecycle

```
 create ─► SCHEDULED ──claim (CAS)──► QUEUED ──worker start (CAS)──► RUNNING ─┬─► SUCCEEDED
              │                         │                                     └─► FAILED
              └──────── cancel ─────────┴──────────► CANCELLED
```

| Status | Meaning |
|---|---|
| `SCHEDULED` | Stored in the database and waiting for `run_at`. It is in the PQ once `run_at` falls inside the read-ahead window (§6.2.1). |
| `QUEUED` | Claimed by the dispatcher; waiting for a free worker. |
| `RUNNING` | A worker is running it. |
| `SUCCEEDED` / `FAILED` | Terminal. The handler returned, or it errored, panicked or exceeded its execution `timeout` (`FAILED` with `error = "execution timeout"`). |
| `CANCELLED` | Terminal. Cancelled before a worker started it. |
| `TIMED_OUT` | Terminal. Never started: the dispatcher claimed it more than `misfire_threshold` after `run_at` under the `skip` misfire policy (§7.4). Not to be confused with a handler exceeding its execution timeout, which is `FAILED`. |

Every transition is a compare-and-set (CAS) update in the database (§6.3).
Terminal states never change again. For a task that belongs to a schedule,
**reaching any terminal state is what creates the schedule's next task** (§6.3).

#### Schedule lifecycle

`ACTIVE` ⇄ `PAUSED` → `CANCELLED` (terminal).

### FR-4 Handlers

Handlers are Go functions compiled into the server and registered by name:

```go
type Handler interface {
    Name() string
    Run(ctx context.Context, payload json.RawMessage) (output string, err error)
}
```

- v1 ships `print`, which writes `payload.message` to stdout, and `sleep`, which is
  useful for testing timeouts, misfires and cancellation.
- Handlers must respect `ctx`. A panic is recovered, and the task becomes `FAILED`.
- The server runs handlers on a fixed-size worker pool; the size is configurable,
  default 4. Each task has a timeout.
- Stretch goal: run external `.go` files from an allow-listed directory as
  subprocesses.

### FR-5 Delayed execution (`ONCE`)

- `delay` is a Go duration string such as `"30s"` or `"5m"`. When it is omitted,
  the task runs immediately.
- The server enforces a maximum delay (e.g. 30 days).
- A task should start within about 1 second of `run_at` when a worker is free.

### FR-6 Cron schedules (`SCHEDULE`)

- Each fire time produces its own task row, so clients poll every run the same way.
- **Runs of a schedule never overlap.** The next task is created only after the
  current one reaches a terminal state (`SUCCEEDED`, `FAILED`, `CANCELLED` or
  `TIMED_OUT`), whichever it is. A failed run does not stop the schedule.
- Because of this, a task that hangs would stall its schedule forever, so a
  `timeout` is **mandatory** for schedule tasks. The server applies a default when
  the client doesn't send one.
- A client can pause, resume and cancel a schedule, and list the tasks it has created.
- See §7 for how cron expressions are parsed and evaluated.

### FR-7 Cancellation

- `DELETE /tasks/{id}` cancels a single task that is `SCHEDULED` or `QUEUED`. If the
  task belongs to a schedule, this **skips that one occurrence**: the schedule
  stays `ACTIVE`, and its next task is created straight away.
- `DELETE /schedules/{id}` cancels a schedule and its pending task. No further
  tasks are created (§6.4).
- Cancelling a `RUNNING` task cancels its context, and the task ends as `FAILED`
  with `error = "cancelled"`. This is a best-effort stop.
- A cancelled entry is **not** removed from the PQ; it is dropped lazily when
  popped (§6.2).

## 6. Design

### 6.1 Data model (Postgres)

```sql
CREATE TABLE schedules (
  id              TEXT PRIMARY KEY,          -- sch_...
  handler         TEXT NOT NULL,
  payload         JSONB,
  cron            TEXT NOT NULL,
  timezone        TEXT NOT NULL DEFAULT 'UTC',
  status          TEXT NOT NULL
                  CHECK (status IN ('ACTIVE', 'PAUSED', 'CANCELLED')),
  timeout_ms      BIGINT NOT NULL,           -- mandatory: a hung task would stall the schedule
  misfire_policy  TEXT NOT NULL DEFAULT 'fire_once'
                  CHECK (misfire_policy IN ('fire_once', 'fire_all', 'skip')),
  next_run_at     TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL,
  updated_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE tasks (
  id           TEXT PRIMARY KEY,             -- tsk_...
  schedule_id  TEXT REFERENCES schedules(id),-- NULL for ONCE
  run_type     TEXT NOT NULL CHECK (run_type IN ('ONCE', 'SCHEDULE')),
  handler      TEXT NOT NULL,
  payload      JSONB,
  run_at       TIMESTAMPTZ NOT NULL,         -- the intended fire time, never modified
  status       TEXT NOT NULL CHECK (status IN
                 ('SCHEDULED', 'QUEUED', 'RUNNING',
                  'SUCCEEDED', 'FAILED', 'CANCELLED', 'TIMED_OUT')),
  timeout_ms   BIGINT NOT NULL,
  attempts     INTEGER NOT NULL DEFAULT 0,
  output       TEXT,
  error        TEXT,
  created_at   TIMESTAMPTZ NOT NULL,
  started_at   TIMESTAMPTZ,
  finished_at  TIMESTAMPTZ,
  CHECK ((run_type = 'ONCE') = (schedule_id IS NULL))
);

-- Idempotent fire: a schedule can never have two tasks for the same fire time.
CREATE UNIQUE INDEX uq_tasks_schedule_fire ON tasks(schedule_id, run_at)
  WHERE schedule_id IS NOT NULL;

-- Invariant: at most one non-terminal task per schedule. This is what "no overlap"
-- means, and the database enforces it, not just application code.
CREATE UNIQUE INDEX uq_tasks_schedule_active ON tasks(schedule_id)
  WHERE schedule_id IS NOT NULL AND status IN ('SCHEDULED', 'QUEUED', 'RUNNING');

CREATE INDEX ix_tasks_pending ON tasks(status, run_at);
```

Type choices:

- **`TIMESTAMPTZ`, never `TIMESTAMP`.** It stores an absolute instant. Plain
  `TIMESTAMP` drops the offset, which breaks the time-zone and DST handling in §7.4.
- **`TEXT` + `CHECK` instead of Postgres `ENUM` types.** An enum value can be added
  but never removed or renamed without recreating the type, while a `CHECK` is one
  `ALTER TABLE`. The allowed values mirror the Go constants in `internal/core/types.go`.
- **`JSONB` for payloads,** so they can be queried and validated when needed.
- **Every timestamp is written by the application**, from the injected `Clock`. The
  schema has no `DEFAULT now()`, because the fake clock in tests would disagree with
  the database clock.

The **database is the source of truth**. The PQ is only an in-memory index of the
next few seconds of work (the read-ahead window, §6.2.1), and it can always be
rebuilt from `tasks WHERE status = 'SCHEDULED'`.

### 6.2 Priority queue

```go
type pqEntry struct {
    RunAt      time.Time // heap ordering key
    TaskID     string
    ScheduleID string    // empty for ONCE
}
```

- The heap must store `RunAt` because it orders entries by it. Everything else is
  read from the database when the entry is popped.
- A single dispatcher goroutine sleeps on a `time.Timer` set for the head's
  `RunAt`. It wakes when the timer fires, when a new entry becomes the head, when
  the refill timer fires (§6.2.1), or on shutdown.
- **Lazy deletion:** a cancelled task stays in the heap and is discarded when it is
  popped, because its claim fails (§6.3). Removing entries eagerly would need an
  index map plus `heap.Remove`. That isn't worth it, because the heap only holds
  one window of work, so stale entries are bounded by the window.

#### 6.2.1 Read-ahead window

Loading every `SCHEDULED` task into memory doesn't scale with the backlog. A job
due in 30 days would sit in the heap for 30 days, and startup would read the whole
backlog before dispatching anything. Instead, the PQ holds only tasks due within
the next `read_ahead` (the **window**). Tasks due later stay only in the database
until the window reaches them. Memory is bounded by the window, not by the total
number of tasks.

**Dispatcher state** (owned by the dispatcher goroutine, never shared, NFR-3):

| Field | Meaning |
|---|---|
| `heap` | The PQ entries. |
| `loaded` | Set of task IDs currently in `heap`, so a task is never added twice. |
| `loadedUntil` | Every `SCHEDULED` task with `run_at < loadedUntil` is either in the heap or will be offered to it (see the proof below). |
| `full` | The last refill hit `max_loaded`, so the window is narrower than `read_ahead`. |

**Refill.** Runs at startup, every `refill_interval`, and whenever the heap drops
below `max_loaded / 2` while `full`:

```
target := clock.Now() + read_ahead          -- read the clock BEFORE the query
rows   := SELECT id, schedule_id, run_at FROM tasks
           WHERE status = 'SCHEDULED' AND run_at < :target
           ORDER BY run_at
           LIMIT :max_loaded
for each row not in loaded: push onto heap, add to loaded
if len(rows) < max_loaded:  loadedUntil = target;             full = false
else:                       loadedUntil = rows[last].run_at;  full = true
```

- The query selects only the columns the heap needs. The payload is read at claim
  time, so a large JSONB payload never enters the heap.
- `ix_tasks_pending (status, run_at)` serves it as an ordered range scan. The
  `LIMIT` stops the scan early and Postgres needs no sort.
- Overdue tasks (`run_at` in the past) sort first, so misfires after downtime are
  handled before anything else, as in §7.4.
- Each refill re-reads from the start of the window rather than continuing from
  `loadedUntil`. Rows already in `loaded` are skipped. This costs up to
  `max_loaded` rows per refill, but it avoids the tie problem at the boundary:
  several tasks can share the last row's `run_at`, and only some of them may fit
  under the `LIMIT`. Continuing from the last row (keyset pagination) is an
  optimization in `memory-optimizations.md`.

**Offer.** After committing a new task (FR-1, `finalize` in §6.3, resume in §6.4),
the caller *offers* it to the dispatcher through a channel. The dispatcher decides
whether to admit it:

| Dispatcher state | Admit when | Otherwise |
|---|---|---|
| Not `full` | `run_at < clock.Now() + read_ahead` | Drop it. A later refill loads it. |
| `full` | `run_at < loadedUntil` (strict) | Drop it. A refill loads it once the heap drains. |

**Why the rule uses `now + read_ahead` and not `loadedUntil`.** A refill's
`SELECT` reads a snapshot taken when the statement started (`READ COMMITTED`). A
task committed after that moment is invisible to that refill. If the offer for
such a task arrives before the refill result is applied, a rule based on the old
`loadedUntil` would drop it, and the refill wouldn't load it either. The task
would stay invisible until the refill after that. With the `now + read_ahead`
rule, no task is lost:

- **Admitted:** it's in the heap. If the refill also returns it, `loaded` skips
  the duplicate.
- **Dropped:** then `run_at ≥ t_offer + read_ahead`, and the offer happens after
  the commit. The first refill that covers `run_at` starts at or after
  `run_at − read_ahead ≥ t_offer`, so it starts after the commit and its snapshot
  sees the row. Refills run every `refill_interval`, so that refill starts no
  later than `run_at − read_ahead + refill_interval`. The task enters the heap at
  least `read_ahead − refill_interval` before it's due. This is why
  `refill_interval` must be less than `read_ahead`.

The claim's CAS (§6.3) is the final backstop: even if a task somehow entered the
heap twice, only one pop could claim it.

**Configuration:**

| Key | Default | Notes |
|---|---|---|
| `dispatcher.read_ahead` | `60s` | Larger means fewer refills but more memory and more stale entries from cancellations. |
| `dispatcher.refill_interval` | `30s` | Must be less than `read_ahead`. The gap is the safety margin for slow refill queries. |
| `dispatcher.max_loaded` | `10000` | Hard cap on heap size. Hitting it means more work is due within the window than one node should hold. |

### 6.3 Claiming and execution

The status check at pop time **must be an atomic compare-and-set**, not a read
followed by a write. Reading `status`, seeing `SCHEDULED`, and then running the
task leaves a window in which a concurrent cancel is lost.

When the dispatcher pops an entry, it claims the task. Zero rows means the task
was cancelled or already claimed, so it skips it:

```sql
UPDATE tasks SET status = 'QUEUED'
 WHERE id = :task_id AND status = 'SCHEDULED';
```

Then it sends the task to the worker channel. In the worker:

```sql
UPDATE tasks SET status = 'RUNNING', started_at = now, attempts = attempts + 1
 WHERE id = :task_id AND status = 'QUEUED';            -- zero rows: cancelled while queued, skip
run handler(ctx, payload)
finalize(task_id, from = 'RUNNING', to = 'SUCCEEDED' | 'FAILED', output, error)
```

#### `finalize`: the one path into a terminal state

A task can reach a terminal state in four places, not just the worker:

| Where | Transition |
|---|---|
| Worker | `RUNNING` → `SUCCEEDED` / `FAILED` |
| Cancel API (`DELETE /tasks/{id}`) | `SCHEDULED` / `QUEUED` → `CANCELLED` |
| Dispatcher, `skip` misfire policy (too late to run) | `SCHEDULED` → `TIMED_OUT` |
| Startup recovery | `RUNNING` → `FAILED` (interrupted) |

If rescheduling happened only in the worker, cancelling one occurrence of a
schedule (or a crash in the middle of a run) would leave the schedule with no next
task, and it would silently stop. So **every** terminal transition goes through a
single function that marks the task terminal and reschedules it, in one transaction:

```sql
BEGIN                                                    -- READ COMMITTED (default)
  SELECT schedule_id, run_at FROM tasks WHERE id = :task_id;   -- plain read, no lock

  IF schedule_id IS NOT NULL:
    -- Lock the schedule FIRST (lock ordering, §6.7). Cancel/pause/resume wait here.
    SELECT status, cron, timezone, misfire_policy FROM schedules
     WHERE id = :schedule_id FOR UPDATE;

  UPDATE tasks SET status = :to, output, error, finished_at = :now
   WHERE id = :task_id AND status = ANY(:from);        -- CAS; zero rows: someone else won, stop

  IF schedule.status = 'ACTIVE':                       -- PAUSED / CANCELLED: do not reschedule
     next := NextFireTime(schedule, task.run_at, now)  -- see §7.4
     INSERT INTO tasks (... run_at = next, status = 'SCHEDULED')
       ON CONFLICT DO NOTHING;                         -- zero rows: already rescheduled
     UPDATE schedules SET next_run_at = next WHERE id = :schedule_id;
COMMIT
offer(next entry) to the dispatcher                      -- admitted only if inside the window (§6.2.1)
```

Notes:

- **Terminal state and next task commit together.** The one-active-task index
  (§6.1) requires the old task to leave `RUNNING` before the new one can be
  inserted, and doing both in one transaction means a crash can't leave one
  without the other.
- **Choosing `next`.** It starts from the task's **intended** `run_at`, so late
  dispatch doesn't make the schedule drift. A run that takes longer than the
  interval means `Next(run_at)` is already in the past, and the misfire policy
  (§7.4) decides what happens.
- **Crash between commit and offer.** The next task is still in the database, and
  the startup refill (§6.5) recovers it. The same holds when the dispatcher drops the
  offer because the task is outside the window: a later refill loads it.
- **Retries (phase 4).** A failed attempt that will be retried is not terminal. It
  goes back to `SCHEDULED` with a backoff `run_at`, and `finalize` runs only after
  the last attempt.

### 6.4 Cancelling a schedule

```sql
BEGIN
  UPDATE schedules SET status = 'CANCELLED'                 -- takes the schedule's row lock
   WHERE id = :id AND status != 'CANCELLED';
  UPDATE tasks SET status = 'CANCELLED'
   WHERE schedule_id = :id AND status IN ('SCHEDULED', 'QUEUED');
COMMIT
-- optional: cancel the ctx of any RUNNING task belonging to this schedule
```

This does **not** call `finalize`'s reschedule step. The schedule is no longer
`ACTIVE`, so nothing would be rescheduled anyway. The dispatcher later pops the
stale PQ entry, its claim updates zero rows, and it drops the entry. A task that
was `RUNNING` finishes (or is cancelled through its context), and `finalize` sees
the schedule is `CANCELLED` and stops there.

**Race: cancel vs finalize.** `finalize` reads the schedule's status and inserts
the next task. If a cancel commits between that read and the insert, the result is
an orphaned `SCHEDULED` task. Both transactions therefore take the **schedule's
row lock** before doing anything else. `finalize` uses `SELECT … FOR UPDATE`, and
cancel's `UPDATE schedules` takes the same lock implicitly. Whichever comes second
waits:

- **Cancel first:** `finalize` unblocks, reads `CANCELLED`, and doesn't reschedule.
- **`finalize` first:** it commits the next task. Cancel's `UPDATE tasks` is a new
  statement, so under `READ COMMITTED` it sees that freshly committed row and
  cancels it too.

Pause is the same as cancel, except the schedule goes to `PAUSED`. Resume sets it
back to `ACTIVE` and inserts a fresh task with `run_at = Next(now)`. If a task was
still `RUNNING` at resume time, the insert fails on the one-active-task index. In
that case resume does nothing more, and that task's `finalize` creates the next one.

### 6.5 Startup recovery

1. Run the first read-ahead refill (§6.2.1). Only tasks due within the window
   are loaded, so startup cost doesn't grow with the backlog. Tasks whose `run_at`
   is in the past sort first and are misfires, handled by the policy in §7.4.
2. Tasks left `QUEUED` were claimed but never started, so it is safe to set them
   back to `SCHEDULED` and requeue them.
3. Tasks left `RUNNING` were interrupted mid-execution. This is the one case where
   the delivery guarantee (§6.6) matters. Default: `finalize` them as `FAILED` with
   `error = "interrupted by restart"`. Using `finalize` means their schedule
   gets its next task.
4. Safety net: for each `ACTIVE` schedule with no non-terminal task, insert one
   with `run_at = Next(now)`. This should never find anything, and if it does,
   log a warning, because it points to a bug in `finalize`.

### 6.6 Execution guarantee: what "only once" really means

The design guarantees the following:

- **No duplicate occurrences.** The unique index on `(schedule_id, run_at)` means
  a fire time produces at most one task, even when `finalize` is retried.
- **No overlap.** The one-active-task index means a schedule never has two tasks
  in flight at once.
- **No duplicate claims.** The CAS updates mean only one dispatcher and one worker
  can move a task forward. A cancel and a claim can't both succeed.
- **No run after cancel.** Once a cancel commits, the task can't start. A task
  that is already running is stopped only on a best-effort basis, through its context.

It cannot guarantee **exactly-once execution** across crashes. If the process dies
while a handler is running, the scheduler can't know whether the side effect
happened. It has to choose between two options:

| Policy on restart | Guarantee | Risk |
|---|---|---|
| Mark interrupted `RUNNING` tasks `FAILED` (**default**) | **At-most-once** | The work may never have happened. |
| Put them back in the queue | **At-least-once** | The work may happen twice. |

Exactly-once *effect* is possible only when handlers are **idempotent**, e.g. they
use `task_id` as an idempotency key for external writes. At-least-once delivery
plus idempotent handlers is what real systems use in practice. Orbit exposes
`task_id` to handlers through `ctx` for this purpose.

### 6.7 Postgres concurrency notes

All transactions run at the default **`READ COMMITTED`** isolation level, and
correctness comes from CAS updates plus explicit row locks rather than from
`SERIALIZABLE`.

1. **CAS is safe under concurrency.** When two transactions `UPDATE` the same row,
   the second one waits for the first to commit and then **re-checks its `WHERE`
   clause against the new row**. So `WHERE status = 'SCHEDULED'` can't match a row
   that another transaction just moved to `CANCELLED`, and `RowsAffected()` tells
   the caller who won.
2. **Lock ordering: schedule row, then task rows.** Every transaction that touches
   both tables locks the schedule first. `finalize`, cancel, pause and resume all
   follow this order. The opposite order in one code path would allow deadlocks.
3. **Never let an expected constraint violation abort the transaction.** In
   Postgres, any error inside a transaction puts it into an aborted state, and
   every later statement fails until rollback. If `finalize`'s insert hit
   `unique_violation` (23505), the terminal status update in the same transaction
   would be lost too. So expected conflicts use `INSERT … ON CONFLICT DO NOTHING`
   and check the number of affected rows. They never catch 23505.
4. **Retry on `deadlock_detected` (40P01) and `serialization_failure` (40001).**
   These shouldn't happen given rule 2, but the transaction runner retries a few
   times with jitter anyway. Retrying is safe because every step is a CAS or an
   idempotent insert.
5. **Keep transactions short.** A row lock is held until commit, so never run a
   handler, do network I/O or wait on a channel inside a transaction.
6. **Looking ahead: multiple nodes.** Postgres is also what makes multiple Orbit
   instances possible later (a non-goal for now). The in-memory heap would give way
   to polling with `SELECT … FOR UPDATE SKIP LOCKED`. The CAS transitions, lock
   ordering and `finalize` would stay the same.

## 7. Cron expressions

### 7.1 Syntax

Use the standard 5-field format:

```
┌───────────── minute        (0–59)
│ ┌─────────── hour          (0–23)
│ │ ┌───────── day of month  (1–31)
│ │ │ ┌─────── month         (1–12 or JAN–DEC)
│ │ │ │ ┌───── day of week   (0–6 or SUN–SAT; 7 = SUN too)
* * * * *
```

Each field supports `*`, single values, lists (`1,15,30`), ranges (`9-17`), steps
(`*/15`, `0-30/10`) and names (`MON-FRI`). Macros: `@yearly`, `@monthly`,
`@weekly`, `@daily`, `@hourly`.

Stretch goals: an optional 6th seconds field, which makes testing faster, and
`@every <duration>`. Quartz extensions (`L`, `W`, `#`, `?`) are out of scope.

### 7.2 Parsing

Parse each field into a `uint64` bitset:

```go
type CronSpec struct {
    Minute, Hour, Dom, Month, Dow uint64
    DomStar, DowStar bool
}
```

**The DOM/DOW rule:** when *both* day-of-month and day-of-week are restricted,
a day matches if **either** one matches. `0 0 1 * MON` means "the 1st of the month
**or** any Monday". The `*Star` flags exist to implement this.

### 7.3 `Next(after)`

Skip forward field by field, the way `robfig/cron` does:

1. If the month doesn't match, move to the 1st of the next month at 00:00 and restart.
2. If the day doesn't match (using the DOM/DOW rule), move to the next day at 00:00
   and restart.
3. If the hour doesn't match, move to the next hour at :00 and restart.
4. If the minute doesn't match, move to the next minute and restart.
5. Everything matches, so return the time.

Give up after searching 5 years ahead, so impossible expressions such as
`0 0 31 2 *` return an error. Such expressions are also rejected when a schedule
is created.

### 7.4 Scheduling semantics

| Concern | Decision |
|---|---|
| **Time zone** | Each schedule has a `timezone` (default UTC); `Next` runs in `time.LoadLocation(tz)`. |
| **DST** | On spring-forward, a time that doesn't exist is skipped. On fall-back, a repeated time fires once. Both cases need tests. |
| **Overlap** | Not possible by design. The next task is created in `finalize`, so runs are always sequential (§6.3). |
| **Misfire** | A misfire happens when `Next(run_at)` is already in the past at finalize time. Causes: a server outage, a busy worker pool, or **a run that took longer than the cron interval**. A schedule only ever has one active task, so a misfire affects a single row, never a pile of them. `fire_once` (default) skips the missed times: `next = Next(max(run_at, now))`. `fire_all` catches up: `next = Next(run_at)`, and each missed time runs one after another until the schedule is current again. `skip` behaves like `fire_once`, and additionally, at claim time, a task more than `misfire_threshold` late is finalized as `TIMED_OUT` instead of running. |
| **Misfire threshold** | A task claimed more than `misfire_threshold` (default 1 minute) late counts as a misfire. Anything less is just normal lateness. |

`NextFireTime(schedule, run_at, now)` in §6.3 is `Next` with the misfire policy applied.

Example: `*/5 * * * *` with `fire_once`. A task scheduled for 10:00 runs from
10:00 to 10:07. At finalize, `Next(10:00) = 10:05` is in the past, so
`next = Next(10:07) = 10:10`. The 10:05 occurrence is skipped. Under `fire_all`,
10:05 would run immediately, followed by 10:10 on time.

### 7.5 Build or buy?

Write our own parser under `internal/cron` for the learning, and use
`github.com/robfig/cron/v3` as a test oracle to compare results against.

## 8. API sketch

```http
POST /jobs
{ "run_type": "ONCE", "handler": "print", "payload": {"message": "hi"}, "delay": "10s" }
→ 201 { "task_id": "tsk_01J...", "status": "SCHEDULED", "run_at": "..." }

POST /jobs
{ "run_type": "SCHEDULE", "handler": "print", "payload": {"message": "tick"},
  "cron": "*/5 * * * *", "timezone": "Asia/Kolkata" }
→ 201 { "schedule_id": "sch_01J...", "next_task_id": "tsk_01J...", "next_run_at": "..." }
```

```http
GET    /tasks/{id}
DELETE /tasks/{id}                  # cancel one task
GET    /schedules/{id}
GET    /schedules/{id}/tasks?status=&limit=
POST   /schedules/{id}/pause
POST   /schedules/{id}/resume
DELETE /schedules/{id}              # cancel schedule and its pending task
GET    /handlers                    # registered handler names
GET    /healthz
```

## 9. Non-functional requirements

| ID | Requirement |
|---|---|
| NFR-1 | Single binary. Standard library plus `github.com/jackc/pgx/v5` (`pgxpool`) for Postgres. A `docker-compose.yml` runs Postgres locally. Schema changes are versioned SQL migrations. |
| NFR-2 | Every status transition is a CAS `UPDATE ... WHERE status = ?`, and code checks the number of affected rows. |
| NFR-3 | `go test -race` passes. The heap, the `loaded` set and `loadedUntil` are owned by the dispatcher goroutine and are not shared. |
| NFR-4 | Graceful shutdown: stop accepting jobs, let `RUNNING` tasks finish within a grace period, then cancel their contexts. `QUEUED` tasks go back to `SCHEDULED`. |
| NFR-5 | Structured `slog` logs for every transition: `task_id`, `schedule_id`, from→to, duration. |
| NFR-6 | `time.Now` and timers sit behind a `Clock` interface, so tests can drive the dispatcher with a fake clock. |
| NFR-7 | Tests cover the cron parser and `Next` (DST, leap years, DOM/DOW), the CAS claim, `finalize` from all four callers, the cancel-vs-finalize race, and startup recovery. |

## 10. Phased plan

| Phase | Scope |
|---|---|
| **1: MVP** | `POST /jobs` with `ONCE` and no delay, `GET /tasks/{id}`, Postgres `tasks` table, worker pool, `print`/`sleep` handlers. |
| **2: Delays** | `delay`, PQ and dispatcher with the read-ahead window, CAS claim, cancelling a single task, startup recovery. |
| **3: Cron** | Parser and `Next`, `SCHEDULE` run type, `schedules` table, `finalize` with rescheduling, cancel/pause/resume. |
| **4: Semantics** | Time zones and DST, misfire policies, retries with backoff (`max_attempts`). |
| **5: Stretch** | External Go script handlers, a CLI client, a seconds field in cron, pruning old tasks. |

## 11. Open questions

1. When a running task is cancelled, should it be `FAILED` (current choice) or a
   separate `CANCELLED` state that can only be reached from `RUNNING`?
2. Should retries of a failed task reuse the same row (`attempts++`) or create a new
   row? Reusing the row keeps the ID stable for polling.
3. What is the retention policy for finished tasks of long-running schedules?
4. Should `SCHEDULE` also accept `start_at` / `end_at` / `max_runs`?
5. Should a schedule pause itself after N consecutive `FAILED` runs? Right now it
   keeps rescheduling forever.

## 12. Suggested layout

```
orbit/
├── main.go
├── internal/
│   ├── api/          # HTTP handlers
│   ├── store/        # schedules/tasks repository, Postgres impl, CAS helpers
│   ├── scheduler/    # PQ, dispatcher, worker pool, recovery, clock
│   ├── cron/         # parser + Next
│   └── handler/      # Handler interface, registry, print/sleep
└── docs/requirements/
```
