CREATE TABLE schedules (
  id              TEXT PRIMARY KEY,
  handler         TEXT NOT NULL,
  payload         JSONB,
  cron            TEXT NOT NULL,
  timezone        TEXT NOT NULL DEFAULT 'UTC',
  status          TEXT NOT NULL
                  CHECK (status IN ('ACTIVE', 'PAUSED', 'CANCELLED')),
  timeout_ms      BIGINT NOT NULL, 
  misfire_policy  TEXT NOT NULL DEFAULT 'fire_once'
                  CHECK (misfire_policy IN ('fire_once', 'fire_all', 'skip')),
  next_run_at     TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL,
  updated_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE tasks (
  id           TEXT PRIMARY KEY,
  schedule_id  TEXT REFERENCES schedules(id),
  run_type     TEXT NOT NULL CHECK (run_type IN ('ONCE', 'SCHEDULE')),
  handler      TEXT NOT NULL,
  payload      JSONB,
  run_at       TIMESTAMPTZ NOT NULL, 
  status       TEXT NOT NULL CHECK (status IN ('SCHEDULED', 'QUEUED', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
  timeout_ms   BIGINT NOT NULL,
  attempts     INTEGER NOT NULL DEFAULT 0,
  output       TEXT,
  error        TEXT,
  created_at   TIMESTAMPTZ NOT NULL,
  started_at   TIMESTAMPTZ,
  finished_at  TIMESTAMPTZ,
  CHECK ((run_type = 'ONCE') = (schedule_id IS NULL))
);

CREATE UNIQUE INDEX uq_tasks_schedule_fire ON tasks(schedule_id, run_at) WHERE schedule_id IS NOT NULL;
CREATE UNIQUE INDEX uq_tasks_schedule_active ON tasks(schedule_id) WHERE schedule_id IS NOT NULL AND status IN ('SCHEDULED', 'QUEUED', 'RUNNING');
CREATE INDEX ix_tasks_pending ON tasks(status, run_at);
