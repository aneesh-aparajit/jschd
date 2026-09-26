# Orbit — Architecture Design (Hexagonal / Ports & Adapters)

| | |
|---|---|
| Status | Draft |
| Author | aneesh-aparajit |
| Last updated | 2026-09-27 |
| Related | [job-scheduler.md](job-scheduler.md): functional requirements, data model, task lifecycle |

## 1. Purpose

This document describes how Orbit's code is structured. It explains the hexagonal
architecture (also called **ports and adapters**) and how it maps onto the scheduler
described in `job-scheduler.md`: which parts are the core, which are ports, which are
adapters, and how a request flows through them.

## 2. The hexagonal pattern

### 2.1 The idea

Alistair Cockburn introduced the pattern in 2005. The rule it is built on:

> The application's core logic must not depend on anything external (HTTP, databases,
> clocks, frameworks). The core defines **interfaces** for what it offers and what it
> needs; the outside world plugs into those interfaces.

The "hexagon" is only a drawing convention. It shows that an application has many
sides, or ports, rather than just a top (UI) and a bottom (DB) as in classic
layered architecture.

### 2.2 Vocabulary

| Term | Meaning |
|---|---|
| **Core** | The business rules. Imports only the standard library's basic packages and its own types. |
| **Port** | An interface **owned by the core**. |
| **Driving (inbound) port** | How the outside world asks the core to do something, e.g. `SubmitJob`. |
| **Driven (outbound) port** | What the core needs the outside world to do for it, e.g. `TaskStore`, `Clock`. |
| **Adapter** | A concrete implementation that connects a port to a technology, e.g. an HTTP server or a Postgres store. |
| **Composition root** | The one place, `main.go`, where adapters are constructed and injected into the core. |

### 2.3 The dependency rule

```
   adapters ──imports──► core ◄──imports── adapters
```

- Adapters import the core. The core **never** imports an adapter.
- Every dependency arrow points inward.
- If `internal/core` ever imports `net/http`, `database/sql`, `pgx`, the sqlc `gen` package or any other driver,
  the rule is broken.

Go makes this cheap because interfaces are satisfied implicitly. The core declares
`TaskStore`, and the Postgres type implements it without ever referring to the core's
interface by name.

### 2.4 What it buys you

1. **Testability.** The core can be tested with an in-memory store and a fake clock,
   with no disk, network or `time.Sleep`.
2. **Replaceability.** Postgres can be swapped for an in-memory store in tests, and an HTTP API can gain a CLI,
   with no changes to the core.
3. **Clarity.** HTTP status codes, JSON tags and SQL live in adapters. Scheduling
   rules live in one place.

### 2.5 What it costs

- More interfaces and more types to map between, e.g. HTTP DTO ↔ core type ↔ DB row.
- Every driven port with two adapters (real + test) means writing the logic twice.
- Taken too far (separate `domain/`, `application/`, `ports/` and `infrastructure/`
  packages, a mapper per layer), it becomes ceremony that slows a small project down.

Orbit uses the **pragmatic version**: one `core` package, a few focused ports, and
adapters in their own packages.

## 3. Orbit mapped onto the hexagon

```
            ┌────────────────┐      ┌────────────────┐     ┌───────────┐
            │  HTTP adapter  │      │ CLI (phase 5)  │     │   tests   │
            └───────┬────────┘      └───────┬────────┘     └─────┬─────┘
                    │  driving port: core.Service                │
                    ▼                       ▼                    ▼
   ┌───────────────────────────────────────────────────────────────────────┐
   │                               CORE                                    │
   │                                                                       │
   │  Service (submit / get / cancel / pause / resume)                     │
   │  Dispatcher (min-heap + timer loop)     Worker pool                   │
   │  Task & Schedule state machines         Misfire policy (NextFireTime) │
   │                                                                       │
   │  uses ─► cron (pure library)                                          │
   └───────┬──────────────────┬──────────────────┬──────────────────┬──────┘
           │ TaskStore        │ Clock            │ HandlerRegistry  │ IDGenerator
           │ ScheduleStore    │                  │                  │
           │ Transactor       │                  │                  │
           ▼                  ▼                  ▼                  ▼
   ┌───────────────┐  ┌───────────────┐  ┌───────────────┐  ┌───────────────┐
   │ postgres/mem  │  │ real / fake   │  │ print, sleep  │  │ ULID / seq    │
   └───────────────┘  └───────────────┘  └───────────────┘  └───────────────┘
```

| Component | Role | Package |
|---|---|---|
| Task/Schedule types, statuses, allowed transitions | Core | `internal/core` |
| `Service`: submit, get, cancel, pause, resume | Core (implements the driving port) | `internal/core` |
| Dispatcher, heap, worker pool, startup recovery | Core | `internal/core` |
| `finalize`, cancel/pause/resume orchestration | Core, run inside a unit of work | `internal/core` |
| `NextFireTime` (cron + misfire policy) | Core | `internal/core` |
| Cron parser and `Next` | Pure library, used by the core; no port needed | `internal/cron` |
| HTTP API | Driving adapter | `internal/adapters/http` |
| Postgres stores + transactor (sqlc) | Driven adapter (`TaskStore`, `ScheduleStore`, `Transactor`) | `internal/adapters/postgres` |
| In-memory stores + transactor | Driven adapter (same three ports), for tests | `internal/adapters/memstore` |
| Real and fake clock | Driven adapter (`Clock`) | `internal/adapters/clock` |
| `print`, `sleep` handlers | Driven adapter (`HandlerRegistry`) | `internal/handler` |
| Wiring, flags, signals | Composition root | `main.go` |

**Why the heap and dispatcher are core, not adapters:** they *are* the scheduling
logic. Deciding when to fire and in what order is not an external technology. The
dispatcher touches the outside only through `Clock` (time) and the stores (state).

**Why cron has no port:** `cron.Parse` and `Next` are pure functions with no I/O and
no alternatives worth swapping. Putting an interface in front of them would be
indirection for its own sake.

## 4. Ports

All of these are declared in `internal/core/ports.go`.

### 4.1 Driving port: `Service`

The HTTP adapter (and later the CLI) depends on this interface only.

```go
type Service interface {
    Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error)

    GetTask(ctx context.Context, id string) (Task, error)
    CancelTask(ctx context.Context, id string) error

    GetSchedule(ctx context.Context, id string) (Schedule, error)
    ListScheduleTasks(ctx context.Context, id string, f TaskFilter) ([]Task, error)
    PauseSchedule(ctx context.Context, id string) error
    ResumeSchedule(ctx context.Context, id string) error
    CancelSchedule(ctx context.Context, id string) error

    Handlers() []string
}

type SubmitRequest struct {
    RunType       RunType         // ONCE | SCHEDULE
    Handler       string
    Payload       json.RawMessage
    Delay         time.Duration   // ONCE only
    Cron          string          // SCHEDULE only
    Timezone      string          // SCHEDULE only
    Timeout       time.Duration
    MisfirePolicy MisfirePolicy   // SCHEDULE only
}
```

The core validates `SubmitRequest` (run type vs. fields, delay bounds, cron parses).
The HTTP adapter validates only what is specific to HTTP, such as malformed JSON and
duration strings that fail to parse.

### 4.2 Driven ports: `TaskStore`, `ScheduleStore`, `Transactor`

Each table gets its own store, and each store knows only its own table. Several
operations must change **both** tables atomically, though:

- `finalize`: mark the task terminal, read the schedule, insert the next task,
  update `schedules.next_run_at`.
- Cancel/pause a schedule: update the schedule, then cancel its pending tasks.
- Create a schedule: insert the schedule and its first task.

A third port, the **`Transactor`**, provides that atomicity. This is the
*unit of work* pattern: the core says "run these store calls as one unit," and the
adapter decides how (a Postgres transaction, or a lock plus snapshot in memory).

#### Design rules

1. **Stores are per table and contain no business rules.** They do CRUD plus
   compare-and-set (CAS). They never decide *whether* to reschedule or *when*.
2. **Stores never start transactions.** Only the `Transactor` does.
3. **No transaction type in any signature.** The core never sees `pgx.Tx`. It gets
   a `Stores` value that happens to be bound to one.
4. **Multi-table orchestration lives in the core.** `finalize` is a core function
   that calls both stores inside `WithTx`, so cron, misfire and "is the schedule
   active?" logic all stay in the core.

#### Ports

```go
type TaskStore interface {
    // Create inserts with ON CONFLICT DO NOTHING and returns ErrDuplicate when no
    // row was inserted ((schedule_id, run_at) or one-active-task conflict). It never
    // lets a unique_violation abort the surrounding transaction.
    Create(ctx context.Context, t Task) error
    Get(ctx context.Context, id string) (Task, error)       // ErrNotFound
    ListBySchedule(ctx context.Context, scheduleID string, f TaskFilter) ([]Task, error)
    ListByStatus(ctx context.Context, statuses ...TaskStatus) ([]Task, error)

    // Transition is the CAS primitive: it updates the task to `to` only if it is
    // currently in one of `from`. ok=false means another actor got there first.
    Transition(ctx context.Context, id string, from []TaskStatus, to TaskStatus, u TaskUpdate) (ok bool, err error)

    // CancelPending moves the schedule's SCHEDULED/QUEUED tasks to CANCELLED.
    CancelPending(ctx context.Context, scheduleID string, now time.Time) (n int, err error)
}

// TaskUpdate carries the fields that change alongside a status transition.
type TaskUpdate struct {
    StartedAt  *time.Time
    FinishedAt *time.Time
    Output     *string
    Error      *string
    IncAttempt bool
}

type ScheduleStore interface {
    Create(ctx context.Context, s Schedule) error
    Get(ctx context.Context, id string) (Schedule, error)   // ErrNotFound

    // GetForUpdate reads the schedule and locks its row until the unit of work
    // ends. Only meaningful inside WithTx. Every unit of work that touches both
    // tables calls this before touching any task row (lock ordering).
    GetForUpdate(ctx context.Context, id string) (Schedule, error)

    // Transition is the CAS primitive for schedules, e.g. ACTIVE → PAUSED.
    Transition(ctx context.Context, id string, from []ScheduleStatus, to ScheduleStatus, now time.Time) (ok bool, err error)

    SetNextRunAt(ctx context.Context, id string, at time.Time) error
}

// Stores groups the per-table stores. Inside WithTx, every store in it shares
// the same transaction.
type Stores struct {
    Tasks     TaskStore
    Schedules ScheduleStore
}

type Transactor interface {
    // WithTx runs fn as one atomic unit of work. If fn returns an error, every
    // write made through s is rolled back. fn may be re-run on a transient
    // conflict (deadlock / serialization failure), so it must be safe to retry.
    WithTx(ctx context.Context, fn func(ctx context.Context, s Stores) error) error
}
```

The core gets both a plain `Stores` (for single reads such as `GET /tasks/{id}`)
and the `Transactor` (for anything that writes more than one row or table).

#### `finalize` in the core

```go
func (c *Core) finalize(ctx context.Context, req FinalizeRequest) (*Next, error) {
    var next *Next
    err := c.tx.WithTx(ctx, func(ctx context.Context, s Stores) error {
        next = nil // WithTx may retry fn on a deadlock / serialization failure

        task, err := s.Tasks.Get(ctx, req.TaskID) // plain read, no lock
        if err != nil {
            return err
        }

        // Lock ordering: the schedule row before any task row. Cancel, pause and
        // resume take the same lock, so they serialize with this unit of work.
        var sch Schedule
        if task.ScheduleID != "" {
            if sch, err = s.Schedules.GetForUpdate(ctx, task.ScheduleID); err != nil {
                return err
            }
        }

        ok, err := s.Tasks.Transition(ctx, req.TaskID, req.From, req.To, req.Update)
        if err != nil || !ok {
            return err // !ok: already finalized by someone else, nothing to do
        }
        if task.ScheduleID == "" || sch.Status != ScheduleActive {
            return nil // ONCE task, or paused / cancelled schedule: don't reschedule
        }

        at, err := NextFireTime(sch, task.RunAt, req.Now) // core rule: cron + misfire
        if err != nil {
            // Never roll back the task's result; stop the schedule instead.
            _, err := s.Schedules.Transition(ctx, sch.ID, []ScheduleStatus{ScheduleActive}, ScheduleCancelled, req.Now)
            return err
        }

        nt := c.newScheduledTask(sch, at)
        if err := s.Tasks.Create(ctx, nt); errors.Is(err, ErrDuplicate) {
            return nil // already rescheduled (idempotent retry)
        } else if err != nil {
            return err
        }
        if err := s.Schedules.SetNextRunAt(ctx, sch.ID, at); err != nil {
            return err
        }
        next = &Next{TaskID: nt.ID, ScheduleID: sch.ID, RunAt: at}
        return nil
    })
    return next, err // caller pushes `next` onto the heap *after* commit
}
```

Compared with passing a `PlanNext` function into one big `Store.Finalize`, this
keeps the whole rescheduling rule readable in one core function. The adapters shrink
to simple, table-shaped CRUD.

#### Rules for code inside `WithTx`

- **Use only the `Stores` passed to `fn`.** The outer, non-transactional stores use a
  different pool connection. A write through them to a row this transaction has
  locked waits for the transaction, while the transaction waits for `fn` to return.
  That's a deadlock Postgres **cannot detect**, because one side of it is Go code.
  It also uses up a second pool connection.
- **No side effects outside the database.** Don't push to the heap, send on
  channels or run handlers inside `fn`. It may be rolled back or retried. Return
  what's needed (like `next`) and act on it after `WithTx` returns.
- **Keep it short.** Row locks are held until commit, so a long `fn` blocks every
  other unit of work on the same schedule.
- **No nesting.** Calling `WithTx` inside `fn` is not supported and returns an error.

#### Adapter implementations

**Postgres + sqlc.** The SQL lives in `.sql` files, and sqlc generates type-safe
Go from them. With `sql_package: pgx/v5`, sqlc generates a `Queries` type built on
a `DBTX` interface. Both `*pgxpool.Pool` and `pgx.Tx` satisfy that interface, and
`Queries.WithTx(tx)` rebinds the queries to a transaction. That's exactly the
shape the `Transactor` needs:

```go
// internal/adapters/postgres/transactor.go
func (t *Transactor) WithTx(ctx context.Context, fn func(context.Context, core.Stores) error) error {
    return retryTransient(ctx, 3, func() error { // 40P01 / 40001 only, with jitter
        return pgx.BeginFunc(ctx, t.pool, func(tx pgx.Tx) error { // commit on nil, rollback on error
            q := t.q.WithTx(tx)
            return fn(ctx, core.Stores{Tasks: &taskStore{q}, Schedules: &scheduleStore{q}})
        })
    })
}

// Outside a transaction the same store types wrap Queries bound to the pool.
func NewStores(pool *pgxpool.Pool) core.Stores {
    q := gen.New(pool)
    return core.Stores{Tasks: &taskStore{q}, Schedules: &scheduleStore{q}}
}
```

Each store method is a thin mapping between the sqlc types and the core types:

```go
func (s *taskStore) Transition(ctx context.Context, id string, from []core.TaskStatus,
    to core.TaskStatus, u core.TaskUpdate) (bool, error) {
    n, err := s.q.TransitionTask(ctx, gen.TransitionTaskParams{
        ID: id, From: toStrings(from), To: string(to),
        StartedAt: u.StartedAt, FinishedAt: u.FinishedAt,
        Output: u.Output, Error: u.Error, IncAttempt: u.IncAttempt,
    })
    return n == 1, err
}
```

The queries use sqlc's annotations. `:execrows` returns the affected row count,
which is the CAS result:

```sql
-- internal/adapters/postgres/queries/tasks.sql

-- name: TransitionTask :execrows
UPDATE tasks SET
    status      = @to,
    started_at  = COALESCE(sqlc.narg('started_at'), started_at),
    finished_at = COALESCE(sqlc.narg('finished_at'), finished_at),
    output      = COALESCE(sqlc.narg('output'), output),
    error       = COALESCE(sqlc.narg('error'), error),
    attempts    = attempts + CASE WHEN @inc_attempt::bool THEN 1 ELSE 0 END
WHERE id = @id AND status = ANY(@from::text[]);

-- name: InsertTask :execrows
INSERT INTO tasks (id, schedule_id, run_type, handler, payload, run_at, status, timeout_ms, created_at)
VALUES (@id, @schedule_id, @run_type, @handler, @payload, @run_at, @status, @timeout_ms, @created_at)
ON CONFLICT DO NOTHING;                      -- 0 rows → store returns core.ErrDuplicate

-- name: CancelPendingTasks :execrows
UPDATE tasks SET status = 'CANCELLED', finished_at = @now
WHERE schedule_id = @schedule_id AND status IN ('SCHEDULED', 'QUEUED');
```

```sql
-- internal/adapters/postgres/queries/schedules.sql

-- name: GetScheduleForUpdate :one
SELECT * FROM schedules WHERE id = @id FOR UPDATE;
```

```yaml
# internal/adapters/postgres/sqlc.yaml
version: "2"
sql:
  - engine: postgresql
    schema: migrations          # sqlc reads the goose migrations as the schema
    queries: queries
    gen:
      go:
        package: gen
        out: gen
        sql_package: pgx/v5
        emit_pointers_for_null_types: true   # nullable columns → *string, *time.Time
        overrides:
          - db_type: timestamptz
            go_type: time.Time
          - db_type: timestamptz
            nullable: true
            go_type: { type: time.Time, pointer: true }
```

sqlc rules for this project:

- **Generated code stays inside the adapter.** `gen.Task`, `gen.TransitionTaskParams`
  and every `pgtype.*` value are mapped to core types in `taskStore` and
  `scheduleStore`. They never appear in a port signature. The core can't import
  `gen`, because that would break the dependency rule.
- **sqlc generates the code, but the stores still hold the adapter logic:**
  mapping `pgx.ErrNoRows` → `core.ErrNotFound`, 0 affected rows → `core.ErrDuplicate`,
  and the type conversions.
- **Migrations are the schema.** One set of goose migration files is both what runs
  against the database and what sqlc compiles against, so they can't drift apart.
- **Generated code is committed**, and CI runs `sqlc diff` to fail the build if it's
  stale.

**In-memory.** One mutex guards both maps. `WithTx` takes the lock, copies the
maps, runs `fn` against stores that read and write the copies, and swaps the copies
in only if `fn` returns nil. That gives real rollback semantics, so tests of
"finalize failed halfway" behave exactly as they would with Postgres.
`GetForUpdate` is the same as `Get`, because the one mutex already serializes
units of work.

#### Trade-off vs. a single `Store`

| | One `Store` with atomic methods | Per-table stores + `Transactor` (chosen) |
|---|---|---|
| Where multi-table logic lives | Adapter (with a `PlanNext` callback for the rules) | Core |
| Adapter complexity | Higher: every operation is hand-written per adapter | Lower: table-shaped CRUD + CAS |
| Risk | Business rules drift into SQL | Core misuses a transaction (outer store in `fn`, side effects in `fn`); the rules above guard against it |
| Store interface size | Grows with every use case | Stable; new use cases compose existing methods |

### 4.3 Driven port: `Clock`

```go
type Clock interface {
    Now() time.Time
    NewTimer(d time.Duration) Timer
}

type Timer interface {
    C() <-chan time.Time
    Stop() bool
    Reset(d time.Duration) bool
}
```

- **Real adapter:** a thin wrapper over `time.Now` and `time.NewTimer`.
- **Fake adapter:** holds a current time and a list of pending timers.
  `Advance(d)` moves the time forward and fires every timer that is now due.
  Tests use it to check delays, misfires and DST without sleeping.

### 4.4 Driven port: `HandlerRegistry`

```go
type Handler interface {
    Name() string
    Run(ctx context.Context, payload json.RawMessage) (output string, err error)
}

type HandlerRegistry interface {
    Get(name string) (Handler, bool)
    Names() []string
}
```

The built-in handlers (`print`, `sleep`) live in `internal/handler`. The external
Go-script runner (a stretch goal) would be another `Handler` implementation. The
core doesn't change for it.

### 4.5 Driven port: `IDGenerator`

```go
type IDGenerator interface {
    TaskID() string     // tsk_<ulid>
    ScheduleID() string // sch_<ulid>
}
```

This is a small port, but it lets tests use predictable IDs (`tsk_1`, `tsk_2`, …)
so assertions stay simple.

### 4.6 Not ports

| Thing | Why it isn't a port |
|---|---|
| Logging | Pass a `*slog.Logger` in. `slog` is the standard library, and its `Handler` is already a pluggable interface. |
| Config | Plain struct values passed to constructors from `main.go`. |
| Cron | A pure library, see §3. |

## 5. Package layout

```
orbit/
├── main.go                          # composition root: flags, wiring, signals
├── internal/
│   ├── core/
│   │   ├── types.go                 # Task, Schedule, Status, RunType, MisfirePolicy
│   │   ├── transitions.go           # allowed state transitions, IsTerminal()
│   │   ├── ports.go                 # Service, TaskStore, ScheduleStore, Transactor, Clock, …
│   │   ├── errors.go                # ErrNotFound, ErrValidation, ErrConflict, ErrDuplicate
│   │   ├── service.go               # implements Service
│   │   ├── dispatcher.go            # heap, timer loop, claim
│   │   ├── worker.go                # worker pool, Start → Run → Finalize
│   │   ├── finalize.go              # finalize: terminal transition + reschedule, in WithTx
│   │   ├── plan.go                  # NextFireTime (cron + misfire)
│   │   └── recovery.go              # startup recovery
│   ├── cron/
│   │   ├── parse.go
│   │   └── next.go
│   ├── handler/
│   │   ├── registry.go
│   │   ├── print.go
│   │   └── sleep.go
│   └── adapters/
│       ├── http/                    # router, DTOs, error → status mapping
│       ├── postgres/
│       │   ├── migrations/          # goose migrations, also the schema sqlc compiles against
│       │   ├── queries/             # tasks.sql, schedules.sql (sqlc input)
│       │   ├── gen/                 # sqlc output: committed, never edited by hand
│       │   ├── sqlc.yaml
│       │   ├── task_store.go        # core.TaskStore: gen types ↔ core types, error mapping
│       │   ├── schedule_store.go    # core.ScheduleStore
│       │   └── transactor.go        # core.Transactor: pgx.BeginFunc + Queries.WithTx + retry
│       ├── memstore/                # same three ports in memory, for tests
│       ├── clock/                   # Real, Fake
│       └── ids/                     # ULID, Sequential
└── docs/requirements/
```

Everything is under `internal/` so no external module can import it. Go enforces
this at compile time.

## 6. Request flows

### 6.1 `POST /jobs` (SCHEDULE)

```
HTTP adapter                    core.Service                         postgres adapter
─────────────                   ────────────                         ──────────────
decode JSON → SubmitRequest ──► validate run type, handler, cron
                                first := cron.Next(clock.Now())
                                build Schedule + first Task
                                  (ids.ScheduleID(), ids.TaskID())
                                tx.WithTx(func(s Stores) {    ─────► BEGIN
                                  s.Schedules.Create(sch)     ─────►   INSERT schedule
                                  s.Tasks.Create(first)       ─────►   INSERT task
                                })                            ─────► COMMIT
                                dispatcher.Push(first)  (after commit)
SubmitResult → 201 JSON  ◄───── return {schedule_id, next_task_id}
```

### 6.2 Dispatcher tick → worker → finalize

```
Dispatcher (core)                    Worker (core)                          postgres adapter
─────────────────                    ─────────────                          ──────────────
timer fires (Clock)
pop heap entry
Tasks.Transition(SCHEDULED→QUEUED) ─────────────────────────────────────►  CAS UPDATE
  ok=false → drop (cancelled)
  ok=true  → send to worker chan ──► Tasks.Transition(QUEUED→RUNNING) ───►  CAS UPDATE
                                     h := registry.Get(task.Handler)
                                     ctx, cancel := timeout(task.Timeout)
                                     out, err := h.Run(ctx, payload)
                                     finalize(req):
                                       tx.WithTx(func(s Stores) { ─────────► BEGIN
                                         s.Tasks.Get(...)  ────────────────►   SELECT
                                         s.Schedules.GetForUpdate(...) ────►   SELECT … FOR UPDATE
                                         s.Tasks.Transition(→terminal) ────►   CAS UPDATE
                                         NextFireTime(...)  (core, no I/O)
                                         s.Tasks.Create(next) ─────────────►   INSERT
                                         s.Schedules.SetNextRunAt(...) ────►   UPDATE
                                       })  ────────────────────────────────► COMMIT
                                     if next != nil:
Push(next) ◄──────────────────────── dispatcher.Push(next)
```

The heap is owned by the dispatcher goroutine. Workers send `Next` entries back
through a channel instead of touching the heap (NFR-3 in `job-scheduler.md`).

### 6.3 Error mapping

The core returns typed errors, and only the HTTP adapter knows about status codes:

| Core error | HTTP |
|---|---|
| `ErrValidation` | 400 |
| `ErrNotFound` | 404 |
| `ErrConflict` (e.g. cancelling a terminal task) | 409 |
| anything else | 500 (logged, generic message to the client) |

## 7. Testing strategy

| Layer | Test with | What it proves |
|---|---|---|
| `cron` | Table tests, with `robfig/cron` as a reference to compare against | Parsing, `Next`, DOM/DOW rule, DST, leap years |
| `core` | `memstore` + fake clock + sequential IDs | State machine, dispatcher timing, misfire policies, `finalize` from all four callers, recovery. Fast and deterministic. |
| `adapters/postgres` | A **shared contract test suite** run against both `postgres` (a real Postgres from `docker compose` or testcontainers-go) and `memstore` | Both adapters behave identically: CAS semantics, `ErrDuplicate` on the unique indexes, the one-active-task constraint, and `WithTx` rollback (an error from `fn` undoes writes to *both* stores) |
| `adapters/http` | `httptest` + a fake `Service` | Routing, JSON shape, error → status mapping |
| End-to-end | Real binary, real Postgres, short `@every` or seconds-field cron | Everything wired together; a handful of smoke tests |

**The contract suite is what makes having two stores safe.** Write the store tests
once as `func TestStores(t *testing.T, open func() (core.Stores, core.Transactor))`, and call it from
both adapter packages. If `memstore` passes and `postgres` passes the same tests, the
fast core tests can be trusted.

## 8. Build order

This follows the phased plan in `job-scheduler.md`:

1. `core/types.go`, `transitions.go`, `ports.go`: the vocabulary.
2. `adapters/clock` (real + fake), `adapters/ids`, `adapters/memstore`.
3. `core` service, dispatcher and worker for `ONCE` tasks, tested against memstore.
4. `adapters/http` + `main.go`: the first runnable binary (phase 1).
5. `docker-compose.yml`, goose migrations, sqlc queries, `adapters/postgres` + the contract suite (phase 2).
6. `cron` package, then `NextFireTime`, `finalize` and schedules in the core (phase 3).

Everything up to step 4 runs with no database at all.

## 9. Guardrails

- **Import check:** fail the build if the core imports adapter or I/O packages.
  ```sh
  go list -deps ./internal/core | grep -E 'net/http|database/sql|jackc/pgx|internal/adapters' && exit 1
  ```
- **No database types in port signatures** (`pgx.Tx`, `pgtype.*`, sqlc `gen.*`).
- **`sqlc diff` runs in CI**, so generated code can't go stale.
- **Inside `WithTx`, use only the `Stores` passed to `fn`**, and cause no side effects outside the database (§4.2).
- **No JSON or HTTP tags on core types.** The HTTP adapter has its own DTOs.
- **One composition root.** Only `main.go` constructs adapters.

## 10. Open questions

1. Should `Service` be an interface or just the concrete `*core.Scheduler` type?
   An interface is only needed if the HTTP adapter tests want a fake. Default: an
   interface, because those tests do want one.
2. Should the dispatcher and the worker pool be split into their own package
   (`internal/core/engine`) once `core` grows past about 1,500 lines?
3. Should the domain types (`Task`, `Schedule`) be split from the service code, as
   `core/domain` and `core/app`? Not until there's a concrete reason.
