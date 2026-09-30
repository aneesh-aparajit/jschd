# Orbit — Memory Optimizations

| | |
|---|---|
| Status | Draft |
| Author | aneesh-aparajit |
| Last updated | 2026-10-01 |
| Related | [job-scheduler.md](job-scheduler.md): requirements and data model · [design.md](design.md): ports and adapters |

## 1. Purpose

This document lists memory and storage optimizations to make **after** Orbit works
end to end. Each one exists to teach a specific database or operating-system
concept, so every item says:

- **Concept:** what you learn by doing it.
- **Change:** what to build.
- **Measure:** how to prove it helped. An optimization without a before/after
  number doesn't count.

The read-ahead window (`job-scheduler.md` §6.2.1) is the baseline. It already
bounds the dispatcher's heap by time rather than by backlog size. Everything below
builds on it.

Rule for the whole document: **measure first, then change one thing, then measure
again.** Several items below are deliberately small wins. The goal is to understand
why they help, not to squeeze out every byte.

## 2. Summary

| # | Optimization | Area | Concept | Phase |
|---|---|---|---|---|
| M0 | Measurement harness | Both | RSS vs Go heap, `pprof`, `EXPLAIN (ANALYZE, BUFFERS)` | Before any other item |
| M1 | Pointer-free heap entries | Go | Struct layout, `time.Time` size, GC scanning of pointer-free memory | After phase 2 |
| M2 | Shrink the heap slice and the `loaded` set | Go | Slices and maps that never give memory back | After phase 2 |
| M3 | Keyset refill from `loadedUntil` | Postgres | Keyset pagination, ties at a page boundary | After phase 2 |
| M4 | Partial and covering index for the refill | Postgres | Partial indexes, index-only scans, the visibility map | After phase 2 |
| M5 | Set-based recovery | Postgres | Pushing work into SQL instead of loading rows into Go | After phase 2 |
| M6 | Compact row layout | Postgres | Tuple headers, alignment padding, rows per 8 KB page | Phase 4 |
| M7 | Cap `output` size, compress payloads | Both | TOAST, `lz4` compression, bounded buffers | Phase 4 |
| M8 | MVCC bloat and autovacuum tuning | Postgres | MVCC tuple versions, HOT updates, `fillfactor`, autovacuum | Phase 4 |
| M9 | Retention: hot/cold split or partitioning | Postgres | `DELETE` vs `DROP PARTITION`, uniqueness across partitions | Phase 5 |
| M10 | Connection pool sizing | Both | Process-per-connection, `work_mem`, shared memory | Phase 4 |
| M11 | `GOMEMLIMIT` and container limits | Go / OS | cgroups, the OOM killer, the Go GC pacer, returning memory to the OS | Phase 4 |
| M12 | Bounded handler output and subprocesses | Go / OS | Pipes, backpressure, `rlimit`, cgroup limits for child processes | Phase 5 (with external handlers) |

## 3. M0: Measurement harness

Build this first. Every other item is judged by it.

**Concept.** A process's memory has several layers, and each tool sees a different one:

| Layer | What it is | How to see it |
|---|---|---|
| Go live heap | Objects reachable right now | `pprof` heap profile, `inuse_space` |
| Go allocations | Everything allocated since start, including garbage | `pprof` `alloc_space`, `runtime/metrics` `/gc/heap/allocs:bytes` |
| Go runtime total | Heap + stacks + runtime metadata + freed memory not yet returned | `runtime/metrics` `/memory/classes/total:bytes` |
| RSS | Physical pages the OS has mapped for the process | `ps -o rss`, `/proc/<pid>/status` (`VmRSS`) |
| Postgres | Shared buffers, per-connection memory, OS page cache | `pg_stat_*` views, `EXPLAIN (ANALYZE, BUFFERS)` |

RSS is usually larger than the live heap. The gap is garbage waiting for the next
GC, memory the runtime freed but hasn't returned to the OS yet, and goroutine
stacks. Learning to explain that gap is half of this document.

**Change.**
- Expose `net/http/pprof` on a separate, local-only port.
- Log a few `runtime/metrics` values every 10 s: live heap, total runtime memory,
  GC cycles, goroutine count, heap size in entries.
- A load generator (`cmd/loadgen`) that submits N `ONCE` tasks spread over a
  configurable time range, plus M schedules.
- A SQL script that prints table and index sizes and dead-tuple counts:

  ```sql
  SELECT relname,
         pg_size_pretty(pg_relation_size(relid))       AS table,
         pg_size_pretty(pg_indexes_size(relid))        AS indexes,
         n_live_tup, n_dead_tup, n_tup_upd, n_tup_hot_upd
    FROM pg_stat_user_tables;
  ```

**Measure.** Record a baseline: 1M tasks spread over 30 days plus 1k schedules
firing every minute. Note RSS, live heap, heap entries, table and index sizes, and
refill query time. Every later item compares against this.

## 4. Go process memory

### M1: Pointer-free heap entries

**Concept.** The current entry is:

```go
type pqEntry struct {
    RunAt      time.Time // 24 bytes: wall uint64, ext int64, loc *Location
    TaskID     string    // 16-byte header + a separate ~30-byte allocation
    ScheduleID string    // 16-byte header + another ~30-byte allocation
}
```

That's 56 bytes inline plus two extra heap allocations per entry. It also
contains **pointers** (`loc`, and each string's data pointer). The Go garbage
collector has to scan every object that contains pointers on every cycle. Objects
with no pointers are allocated in "noscan" spans, which the GC skips entirely.

**Change.**

```go
type pqEntry struct {
    RunAt  int64    // unix nanoseconds, UTC
    TaskID [16]byte // ULID bytes; the "tsk_" prefix is added at the API boundary
}                   // 24 bytes, no pointers
```

- Drop `ScheduleID`. The claim reads the full row anyway (§6.2 already says
  everything but `RunAt` is read at pop time).
- Store `[]pqEntry` as values, never `[]*pqEntry`. The backing array is then one
  pointer-free allocation.
- Convert to `time.Time` only when resetting the timer.

**Measure.** Heap profile at 10k and 100k entries, before and after. Also compare
GC CPU time (`GODEBUG=gctrace=1` prints how long each cycle took) with the heap
full. Expect roughly a 3–5× drop in bytes per entry. The GC improvement is the
more interesting number.

### M2: Shrink the heap slice and the `loaded` set

**Concept.** Go gives memory back less often than you'd expect:

- **Slices.** Popping from a heap reduces `len` but never `cap`. After a burst of
  100k tasks drains, the backing array still has room for 100k.
- **Maps.** Deleting keys doesn't shrink a Go map. As of Go 1.25, the table keeps
  its peak size until the map itself is garbage.
- **Stale pointers.** If an entry held pointers, a popped slot past `len` would
  still reference the old object and keep it alive. M1 removes the pointers, which
  removes this leak too. That's worth writing down as a lesson.

**Change.** After each refill, if `cap(heap) > 4 * max(len(heap), minCap)`, copy
into a right-sized slice. Rebuild `loaded` the same way when
`len(loaded) < peak / 4`. Store `loaded` as `map[[16]byte]struct{}` rather than
`map[string]struct{}`, so its keys have no pointers either.

**Measure.** Submit 100k tasks due in the next minute, let them drain, and compare
the live heap after draining with and without shrinking.

## 5. Postgres: queries and indexes

### M3: Keyset refill from `loadedUntil`

**Concept.** The refill re-reads the window from the start each time (§6.2.1).
Continuing from where the last refill stopped is **keyset pagination**:
`WHERE run_at >= :last_run_at`, as opposed to `OFFSET`, which scans and discards
rows. The subtle part is **ties**. If the last row had `run_at = T` and several
more tasks also have `run_at = T`, then `run_at > T` skips them, and
`run_at >= T` re-reads the ones already loaded.

**Change.** Make the sort key unique by paginating on `(run_at, id)`:

```sql
SELECT id, schedule_id, run_at FROM tasks
 WHERE status = 'SCHEDULED'
   AND (run_at, id) > (:last_run_at, :last_id)   -- row-value comparison
   AND run_at < :target
 ORDER BY run_at, id
 LIMIT :max_loaded;
```

This needs an index on `(run_at, id)` for the `SCHEDULED` rows (see M4). A keyset
refill alone would miss tasks committed after the previous refill with
`run_at < last_run_at`, such as a `delay: 0` job. Those are covered by the offer
path, and a full re-read every N refills is a cheap safety net. Write out why that
is enough, in the style of the proof in §6.2.1.

**Measure.** Rows read per refill (`EXPLAIN (ANALYZE, BUFFERS)`, `rows` and
`shared hit`) with a full window, before and after.

### M4: Partial and covering index for the refill

**Concept.** `ix_tasks_pending (status, run_at)` indexes **every** task, including
the millions of `SUCCEEDED` rows that will never be scheduled again. A **partial
index** only contains rows matching its `WHERE`, so its size tracks pending work,
not history. A **covering index** (`INCLUDE`) stores extra columns so the query
can be answered from the index alone (an **index-only scan**). There's a catch:
Postgres can only skip the table for pages the **visibility map** marks
all-visible. `VACUUM` sets those bits, and every `UPDATE` clears them for the page
it touches. On a table with many updates, "index-only" scans still read many table
pages.

**Change.**

```sql
CREATE INDEX ix_tasks_due ON tasks (run_at, id)
  INCLUDE (schedule_id)
  WHERE status = 'SCHEDULED';
```

Then check whether `ix_tasks_pending` is still needed by anything else.

**Measure.**
- `pg_relation_size('ix_tasks_due')` vs `ix_tasks_pending` with 1M finished tasks.
- `EXPLAIN (ANALYZE, BUFFERS)` of the refill: does it say `Index Only Scan`, and
  what is `Heap Fetches`? Run it before and after `VACUUM tasks` to see the
  visibility map at work.

### M5: Set-based recovery

**Concept.** Startup recovery (§6.5 steps 2–3) as written loads `QUEUED` and
`RUNNING` rows into Go through `ListByStatus`, then updates them one by one. That
is unbounded memory in Go and one round trip per row. A single SQL statement does
the same work inside the database with no transfer.

**Change.**
- Step 2: `UPDATE tasks SET status = 'SCHEDULED' WHERE status = 'QUEUED';`
- Step 3 still needs `finalize` per task, because it reschedules. Page through the
  rows with `LIMIT 500` and keyset pagination, so memory stays bounded.

**Measure.** Recovery time and peak RSS with 50k tasks stuck in `QUEUED` and
`RUNNING`, before and after.

## 6. Postgres: storage

### M6: Compact row layout

**Concept.** Every Postgres row has a **23-byte tuple header** (padded to 24)
before any data. Columns are then stored in declaration order, each aligned to its
type: `bigint` and `timestamptz` on 8 bytes, `integer` on 4. A 4-byte column
followed by an 8-byte one wastes 4 bytes of padding. Rows live in 8 KB pages, so
smaller rows mean more rows per page, fewer pages to read, and a better cache hit
rate.

**Change.**
- Store IDs as `uuid` (16 bytes) instead of `TEXT` (`tsk_` + 26 characters is
  30 bytes of data plus a length header). A ULID fits in a `uuid` column. The
  prefix is added at the API boundary. Trade-off: raw IDs in `psql` stop being
  readable. A `tsk_id(uuid)` SQL function helps with that.
- Order columns from widest alignment to narrowest: `uuid` and 8-byte columns
  first, then `integer`, then variable-length (`text`, `jsonb`).
- Store `status` as `smallint` or `"char"`? **Probably not.** Measure first. The
  `TEXT` + `CHECK` choice (§6.1) is about schema evolution, and a short text value
  costs only a few bytes. Knowing when *not* to optimize is part of the lesson.

**Measure.** `pg_column_size(t.*)` averaged over a sample of rows, and
`pg_relation_size` for 1M rows, before and after.

### M7: Cap `output`, compress payloads

**Concept.** Postgres stores values larger than about 2 KB out of line in a
**TOAST** table, compressed and split into chunks. Reading such a value means
extra lookups and decompression. `SELECT *` pays that cost even when the caller
doesn't need the column. Since Postgres 14, TOAST can use `lz4`, which is much
faster than the default `pglz`.

**Change.**
- Truncate handler `output` to a configured cap (e.g. 64 KB) before writing, and
  mark it as truncated.
- `ALTER TABLE tasks ALTER COLUMN payload SET COMPRESSION lz4;` (the same for
  `output`).
- Make sure no hot path uses `SELECT *` on `tasks`. The refill already selects
  only three columns.

**Measure.** Table plus TOAST size (`pg_total_relation_size`) and
`GET /tasks/{id}` latency with 10 KB payloads, before and after.

### M8: MVCC bloat and autovacuum tuning

**Concept.** This is the most important Postgres lesson in the project.

- Postgres never updates a row in place. Every `UPDATE` writes a **new tuple
  version** and marks the old one dead (**MVCC**). A task passes through
  `SCHEDULED → QUEUED → RUNNING → terminal`, which is at least three updates, so it
  leaves at least three dead tuples behind.
- Every index normally needs a new entry pointing at the new version too. A
  **HOT update** (heap-only tuple) avoids that. But HOT is only possible when no
  indexed column changes, including columns used in a partial index's `WHERE`,
  and there's free space on the same page.
- `status` appears in `ix_tasks_pending` and in the predicates of the partial
  indexes, so **every status transition is a non-HOT update**. That's a direct
  consequence of the index design, and worth seeing in the numbers.
- Dead tuples are reclaimed by **VACUUM**. Autovacuum triggers when dead tuples
  exceed `autovacuum_vacuum_threshold + autovacuum_vacuum_scale_factor × rows`.
  The default scale factor is 0.2, so a 10M-row table waits for about 2M dead
  tuples.

**Change.**
- Per-table autovacuum settings for `tasks`:
  ```sql
  ALTER TABLE tasks SET (autovacuum_vacuum_scale_factor = 0.01,
                         autovacuum_vacuum_threshold    = 1000);
  ```
- Experiment with `fillfactor = 80` on `tasks`. It leaves free space on each page
  for updates that *can* be HOT, such as a future lease heartbeat that only touches
  an unindexed `lease_until` column. Do not index `lease_until` for that reason.
  The reaper can scan the small partial index on `status IN ('QUEUED','RUNNING')`
  instead.

**Measure.** Run a steady load for 30 minutes, then compare `n_dead_tup`,
`n_tup_hot_upd / n_tup_upd`, table size and index size, with default settings vs
tuned ones.

### M9: Retention: hot/cold split or partitioning

**Concept.** Finished tasks pile up forever (open question 3 in
`job-scheduler.md`). Deleting old rows with `DELETE` creates dead tuples on every
page it touches, and vacuum has to clean them up afterwards. Dropping a whole
**partition** removes the files with no dead tuples. Partitioning has a catch,
though: **a unique index on a partitioned table must include the partition key.**
`uq_tasks_schedule_active (schedule_id)`, the one-active-task rule, cannot include
a time column without losing its meaning. Partitioning would silently weaken the
no-overlap guarantee.

**Change.** Pick one, and write down why:

| Option | How | Keeps both unique indexes? | Cost |
|---|---|---|---|
| **Hot/cold split** | Active tasks in `tasks`. `finalize` moves the row to `task_history` in the same transaction. Purge history by partition. | Yes. They stay on the small `tasks` table. | `GET /tasks/{id}` checks two tables. `finalize` does one more write. |
| **Partition `tasks` by `created_at`** | Monthly partitions, `DROP` old ones. | No. The one-active-task rule needs another mechanism. | Uniqueness moves into application logic or a separate table. |
| **Batched `DELETE`** | `DELETE … WHERE id IN (SELECT … LIMIT 5000)` in a loop | Yes | Creates bloat; relies on M8's vacuum tuning. |

The hot/cold split is the recommended option. It keeps the database enforcing
the invariants from §6.1, and the hot table stays small enough to stay in cache.

**Measure.** After 10M finished tasks: `tasks` size, refill query time, and the
time to purge a month of history, for each option you try.

### M10: Connection pool sizing

**Concept.** Postgres uses one OS **process per connection**. Each backend has
its own private memory, typically a few MB at idle and more after running
queries. On top of that, `work_mem` can be allocated **once per sort or hash
operation, per query**, so the worst case is roughly
`connections × operations per query × work_mem`. `shared_buffers` is one shared
memory segment used by every backend. The OS page cache sits below it, so hot
pages can be cached twice.

**Change.** Size `pgxpool` from what actually uses connections: workers,
the dispatcher, HTTP handlers and recovery. Something like `workers + 4` is enough,
not 100. Keep the refill and the claim free of sorts (M4 makes the refill an index
scan), so they never need `work_mem`.

**Measure.** Postgres RSS (`ps` on the container, or `docker stats`) with pool
sizes 10 and 100 under the same load. Also `EXPLAIN` the hot queries and confirm
there's no `Sort` node.

## 7. Go process and the OS

### M11: `GOMEMLIMIT` and container limits

**Concept.**
- In a container, the kernel enforces memory through **cgroups**
  (`memory.max` in cgroup v2). Going over it gets the process killed by the
  **OOM killer**, with no Go panic and no stack trace.
- The Go GC normally lets the heap grow to twice the live heap before collecting
  (`GOGC=100`), so a 400 MB live heap can briefly need about 800 MB.
- `GOMEMLIMIT` (Go 1.19+) gives the GC a soft ceiling. Near the limit it collects
  more often instead of letting the heap grow.
- Go 1.25 sets `GOMAXPROCS` from the cgroup CPU limit automatically, but
  **nothing sets `GOMEMLIMIT` for you.**
- Freed memory goes back to the OS gradually (the runtime's **scavenger**), so
  RSS can stay high for a while after a spike even though the heap is small.

**Change.** Set a memory limit in `docker-compose.yaml` and set `GOMEMLIMIT` to
about 90% of it. Leave `GOGC` at its default.

**Measure.** Deliberately exceed the limit: submit a burst of large payloads with
the heap capped. Compare three runs: no `GOMEMLIMIT` (expect an OOM kill: check
`docker inspect` for `OOMKilled`), with `GOMEMLIMIT` (expect more GC cycles and
survival), and `GOMEMLIMIT` set too low (expect high GC CPU time; this is the
"death spiral" the limit can cause).

### M12: Bounded handler output and subprocesses

**Concept.** This belongs with the stretch goal of running external `.go` files
as subprocesses (FR-4).
- A child process writes to a **pipe**, and the kernel pipe buffer is small
  (64 KB by default on Linux). If Orbit doesn't read the pipe, the child blocks
  when the buffer fills. That's **backpressure**, and it's also a classic deadlock
  when the parent waits for the child to exit before reading.
- If Orbit reads everything into memory, a noisy child can use all of Orbit's
  memory.
- A child's own memory can be capped with `setrlimit` (`RLIMIT_AS`) or, better,
  by putting it in its own cgroup.

**Change.**
- Read child output through `io.LimitReader` into a fixed-size buffer, keeping
  only the head (or head plus tail) up to the M7 cap.
- Keep reading and discarding after the cap, so the child never blocks on a full
  pipe.
- Run children with a memory limit: `prlimit`, or a per-task cgroup when running
  on Linux.

**Measure.** A test handler that prints 1 GB to stdout: Orbit's RSS should stay
flat, and the task should finish with its output truncated. A test handler that
allocates without limit should be killed without affecting Orbit.

## 8. Not doing (and why)

| Idea | Why not |
|---|---|
| `unsafe` string/byte tricks, arenas, custom allocators | Hard to get right and hard to read. M1 gets most of the benefit safely. |
| A timing wheel instead of the heap | It helps with millions of short timers. The read-ahead window already keeps the heap small, and `O(log n)` on 10k entries is trivial. Worth studying, not building. |
| Tuning `GOGC` | `GOMEMLIMIT` (M11) is the better control. Change `GOGC` only if a profile shows a reason. |
| Postgres `ENUM` for `status` | Revisits a decision made for schema evolution (§6.1) to save a few bytes a row. |
| Caching task rows in Go | The database is the source of truth. A cache adds invalidation bugs to a system whose whole point is correctness. |

## 9. Open questions

1. Should `max_loaded` adapt to observed memory instead of being a fixed number?
2. With the hot/cold split (M9), should `GET /tasks/{id}` check `tasks` first,
   or use the ULID's timestamp to guess which table holds the row?
3. Is `uuid` storage (M6) worth losing readable IDs in `psql`, given the
   project's size?
