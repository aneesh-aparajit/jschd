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
| **Adapter** | A concrete implementation that connects a port to a technology, e.g. an HTTP server or a SQLite store. |
| **Composition root** | The one place, `main.go`, where adapters are constructed and injected into the core. |

### 2.3 The dependency rule

```
   adapters ──imports──► core ◄──imports── adapters
```

- Adapters import the core. The core **never** imports an adapter.
- Every dependency arrow points inward.
- If `internal/core` ever imports `net/http`, `database/sql` or a driver package,
  the rule is broken.

Go makes this cheap because interfaces are satisfied implicitly. The core declares
`TaskStore`, and the SQLite type implements it without ever referring to the core's
interface by name.

### 2.4 What it buys you

1. **Testability.** The core can be tested with an in-memory store and a fake clock,
   with no disk, network or `time.Sleep`.
2. **Replaceability.** SQLite can become Postgres, and an HTTP API can gain a CLI,
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
           │ Store            │ Clock            │ HandlerRegistry  │ IDGenerator
           ▼                  ▼                  ▼                  ▼
   ┌───────────────┐  ┌───────────────┐  ┌───────────────┐  ┌───────────────┐
   │ sqlite / mem  │  │ real / fake   │  │ print, sleep  │  │ ULID / seq    │
   └───────────────┘  └───────────────┘  └───────────────┘  └───────────────┘
```

| Component | Role | Package |
|---|---|---|
| Task/Schedule types, statuses, allowed transitions | Core | `internal/core` |
| `Service`: submit, get, cancel, pause, resume | Core (implements the driving port) | `internal/core` |
| Dispatcher, heap, worker pool, startup recovery | Core | `internal/core` |
| `NextFireTime` (cron + misfire policy) | Core | `internal/core` |
| Cron parser and `Next` | Pure library, used by the core; no port needed | `internal/cron` |
| HTTP API | Driving adapter | `internal/adapters/http` |
| SQLite store | Driven adapter (`Store`) | `internal/adapters/sqlite` |
| In-memory store | Driven adapter (`Store`), for tests | `internal/adapters/memstore` |
| Real and fake clock | Driven adapter (`Clock`) | `internal/adapters/clock` |
| `print`, `sleep` handlers | Driven adapter (`HandlerRegistry`) | `internal/handler` |
| Wiring, flags, signals | Composition root | `main.go` |

**Why the heap and dispatcher are core, not adapters:** they *are* the scheduling
logic. Deciding when to fire and in what order is not an external technology. The
dispatcher touches the outside only through `Clock` (time) and `Store` (state).

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

### 4.2 Driven port: `Store`

The store has **one** interface, not a separate `TaskStore` and `ScheduleStore`.
Several operations must update both tables atomically (`Finalize`,
`CancelSchedule`), and a transaction can't span two independent interfaces without
leaking a transaction object into the core.

**Design rule: the port exposes whole atomic operations, never transactions.** There
is no `BeginTx` on the port. Each method is one unit of work, and the adapter decides
how to make it atomic: a SQL transaction in SQLite, a mutex in memory.

```go
type Store interface {
    // Creation
    CreateTask(ctx context.Context, t Task) error                          // ONCE
    CreateSchedule(ctx context.Context, s Schedule, first Task) error      // SCHEDULE + first task, atomically

    // Reads
    GetTask(ctx context.Context, id string) (Task, error)
    GetSchedule(ctx context.Context, id string) (Schedule, error)
    ListScheduleTasks(ctx context.Context, scheduleID string, f TaskFilter) ([]Task, error)
    ListByStatus(ctx context.Context, statuses ...Status) ([]Task, error)   // startup recovery

    // Transitions (all compare-and-set; ok=false means another actor won)
    Claim(ctx context.Context, id string) (ok bool, err error)              // SCHEDULED → QUEUED
    Start(ctx context.Context, id string, now time.Time) (Task, bool, error) // QUEUED → RUNNING
    Requeue(ctx context.Context, id string) (bool, error)                   // QUEUED → SCHEDULED (shutdown/recovery)

    // Terminal transition + reschedule, atomically (job-scheduler.md §6.3)
    Finalize(ctx context.Context, req FinalizeRequest, plan PlanNext) (*Next, error)

    // Schedule lifecycle
    CancelSchedule(ctx context.Context, id string, now time.Time) error
    PauseSchedule(ctx context.Context, id string, now time.Time) error
    ResumeSchedule(ctx context.Context, id string, first Task) error
}

type FinalizeRequest struct {
    TaskID string
    From   []Status // allowed current states, e.g. {RUNNING}
    To     Status   // SUCCEEDED | FAILED | CANCELLED | TIMED_OUT
    Output string
    Error  string
    Now    time.Time
}

// Next is what the caller pushes onto the heap after the commit.
type Next struct {
    TaskID     string
    ScheduleID string
    RunAt      time.Time
}
```

#### Keeping rules in the core while atomicity lives in the adapter

`Finalize` needs to compute the next fire time, which depends on the cron expression,
the misfire policy and the time zone. That is a **business rule**, so it must not be
written in SQL inside the adapter. But the computation has to happen **inside** the
transaction, because it reads the schedule row under the same lock.

The solution is to pass the rule in as a function:

```go
// PlanNext is core logic; the adapter calls it inside its transaction.
// It returns ok=false to mean "do not reschedule", e.g. when the cron has no future time.
type PlanNext func(s Schedule, finished Task, now time.Time) (runAt time.Time, ok bool, err error)
```

Inside `Finalize`, the SQLite adapter:

1. `BEGIN IMMEDIATE`
2. CAS-updates the task to `To`. Zero rows means another actor already finished it:
   roll back and return `nil`.
3. If the task has a schedule, loads it. If the schedule is not `ACTIVE`, commits
   without rescheduling.
4. Calls `plan(schedule, task, now)`. On an error or `ok=false`, stops the schedule
   and commits, so the task's result is **never** rolled back.
5. Inserts the next task with `ON CONFLICT DO NOTHING`.
6. Commits and returns `*Next`.

The core owns *what* the next time is; the adapter owns *how* it's persisted
atomically. The in-memory adapter runs the same steps under a mutex.

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
│   │   ├── ports.go                 # Service, Store, Clock, HandlerRegistry, IDGenerator
│   │   ├── errors.go                # ErrNotFound, ErrValidation, ErrConflict
│   │   ├── service.go               # implements Service
│   │   ├── dispatcher.go            # heap, timer loop, claim
│   │   ├── worker.go                # worker pool, Start → Run → Finalize
│   │   ├── plan.go                  # NextFireTime / PlanNext (cron + misfire)
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
│       ├── sqlite/                  # Store impl, migrations, transactions
│       ├── memstore/                # Store impl for tests
│       ├── clock/                   # Real, Fake
│       └── ids/                     # ULID, Sequential
└── docs/requirements/
```

Everything is under `internal/` so no external module can import it. Go enforces
this at compile time.

## 6. Request flows

### 6.1 `POST /jobs` (SCHEDULE)

```
HTTP adapter                    core.Service                         Store (sqlite)
─────────────                   ────────────                         ──────────────
decode JSON → SubmitRequest ──► validate run type, handler, cron
                                first := cron.Next(clock.Now())
                                build Schedule + first Task
                                  (ids.ScheduleID(), ids.TaskID())
                                store.CreateSchedule(s, first) ────► BEGIN; INSERT schedule;
                                                                     INSERT task; COMMIT
                                dispatcher.Push(first)  (after commit)
SubmitResult → 201 JSON  ◄───── return {schedule_id, next_task_id}
```

### 6.2 Dispatcher tick → worker → finalize

```
Dispatcher (core)                    Worker (core)                         Store
─────────────────                    ─────────────                         ─────
timer fires (Clock)
pop heap entry
store.Claim(id) ───────────────────────────────────────────────────────►  CAS SCHEDULED→QUEUED
  ok=false → drop (cancelled)
  ok=true  → send to worker chan ──► store.Start(id) ────────────────────► CAS QUEUED→RUNNING
                                     h := registry.Get(task.Handler)
                                     ctx, cancel := timeout(task.Timeout)
                                     out, err := h.Run(ctx, payload)
                                     store.Finalize(req, core.PlanNext) ──► BEGIN IMMEDIATE
                                                                            CAS RUNNING→terminal
                                                                            plan(...) → next run_at
                                                                            INSERT next task
                                                                            COMMIT
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
| `adapters/sqlite` | A **shared contract test suite** run against both `sqlite` and `memstore` | Both stores behave identically: CAS semantics, atomic `Finalize`, the one-active-task constraint, idempotent inserts |
| `adapters/http` | `httptest` + a fake `Service` | Routing, JSON shape, error → status mapping |
| End-to-end | Real binary, real SQLite, short `@every` or seconds-field cron | Everything wired together; a handful of smoke tests |

**The contract suite is what makes having two stores safe.** Write the store tests
once as `func TestStore(t *testing.T, newStore func() core.Store)`, and call it from
both adapter packages. If `memstore` passes and `sqlite` passes the same tests, the
fast core tests can be trusted.

## 8. Build order

This follows the phased plan in `job-scheduler.md`:

1. `core/types.go`, `transitions.go`, `ports.go`: the vocabulary.
2. `adapters/clock` (real + fake), `adapters/ids`, `adapters/memstore`.
3. `core` service, dispatcher and worker for `ONCE` tasks, tested against memstore.
4. `adapters/http` + `main.go`: the first runnable binary (phase 1).
5. `adapters/sqlite` + the contract suite (phase 2).
6. `cron` package, then `PlanNext` and schedules in the core (phase 3).

Everything up to step 4 runs with no database at all.

## 9. Guardrails

- **Import check:** fail the build if the core imports adapter or I/O packages.
  ```sh
  go list -deps ./internal/core | grep -E 'net/http|database/sql|internal/adapters' && exit 1
  ```
- **No SQL types in port signatures** (`sql.Tx`, `sql.NullString`, …).
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
