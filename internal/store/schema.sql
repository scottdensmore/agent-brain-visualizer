-- Schema for the shared Agent Brain Visualizer store.
--
-- Every statement is idempotent (IF NOT EXISTS), so SchemaBootstrap can run this
-- on every boot, from several machines, against one shared database.

-- One row per ingested agent trajectory. `steps` holds the normalized timeline
-- the frontend renders; the tool-native transcript is normalized at ingest time.
CREATE TABLE IF NOT EXISTS sessions (
    source       text        NOT NULL,
    id           text        NOT NULL,
    title        text,
    updated_at   timestamptz NOT NULL,
    steps        jsonb       NOT NULL,
    content_hash text        NOT NULL,
    source_mtime bigint      NOT NULL,
    raw_source   text,
    ingested_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, id)
);

-- `id` breaks ties so LIMIT-ed listings stay stable across identical mtimes.
CREATE INDEX IF NOT EXISTS sessions_source_updated
    ON sessions (source, updated_at DESC, id);

-- Cached AI analyses, keyed to the session they describe.
CREATE TABLE IF NOT EXISTS summaries (
    source       text        NOT NULL,
    session_id   text        NOT NULL,
    summary      jsonb       NOT NULL,
    short_title  text,
    content_hash text,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, session_id)
);

-- Added after the summaries table shipped: the hash lets an ingest client tell
-- whether a locally-stored summary is already in the store, so a summary that
-- appears after its transcript was ingested can still sync. Additive and
-- idempotent, so the startup bootstrap applies it on every boot.
ALTER TABLE summaries ADD COLUMN IF NOT EXISTS content_hash text;

-- Files a transcript explicitly attached, carried with the session by the ingest
-- client so the inline preview works from any machine. Before this, the preview
-- read the server's own disk, so it only ever worked when the machine serving the
-- UI was the machine that ran the agent.
--
-- `path` is the absolute path as the transcript spelled it, which is what the
-- rendered link carries — it is a lookup key here, never a path the server opens.
CREATE TABLE IF NOT EXISTS session_files (
    source     text        NOT NULL,
    session_id text        NOT NULL,
    path       text        NOT NULL,
    content    text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, session_id, path)
);

-- Saved eval runs. `saved_at` is an ISO-8601 instant, so it doubles as the
-- delete key and a lexicographic stand-in for chronological order. It is NOT
-- unique: two runs saved in the same instant are two rows, and deleting by
-- `saved_at` removes both — the behaviour of the file-backed store it replaces.
-- Hence the surrogate key, which also breaks ordering ties.
CREATE TABLE IF NOT EXISTS eval_runs (
    run_id             bigserial        PRIMARY KEY,
    saved_at           text             NOT NULL,
    flavor             text             NOT NULL,
    model_label        text             NOT NULL,
    session_count      int              NOT NULL,
    evaluated_sessions int              NOT NULL,
    avg_score          double precision NOT NULL,
    check_pass_rates   jsonb            NOT NULL,
    judged             boolean          NOT NULL,
    judged_sessions    int              NOT NULL,
    avg_faithfulness   double precision NOT NULL,
    avg_actionability  double precision NOT NULL,
    avg_clarity        double precision NOT NULL
);

CREATE INDEX IF NOT EXISTS eval_runs_flavor_saved
    ON eval_runs (flavor, saved_at DESC, run_id DESC);

CREATE INDEX IF NOT EXISTS eval_runs_saved_at
    ON eval_runs (saved_at);
