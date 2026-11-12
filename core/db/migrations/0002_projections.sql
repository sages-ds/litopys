-- Litopys · 0002 · read models (slice 1) fed by the synchronous projector
-- Every table here is DERIVED. Rebuild: TRUNCATE projection.* , reset projector_state, replay journal.
-- Only the projector role writes here (litopys_app via the apply() code path); nothing else ever UPDATEs a projection.
-- NOT YET EXECUTED against a live server — first run on the stand.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

CREATE SCHEMA projection;
GRANT USAGE ON SCHEMA projection TO litopys_app;

-- ---------------------------------------------------------------------------
-- projector_state · exactly-once: updated in the same transaction as the events it applied.
-- lag = journal_head.last_seq - projector_state.last_seq (system.health).
-- ---------------------------------------------------------------------------
CREATE TABLE projection.projector_state (
  tenant_id   uuid PRIMARY KEY REFERENCES tenant(tenant_id),
  last_seq    bigint NOT NULL DEFAULT 0,
  rebuilt_at  timestamptz,
  version     text NOT NULL          -- projector code version that produced this state; mismatch → rebuild
);

-- ---------------------------------------------------------------------------
-- actors · runs · contexts  (R1, R3, R11)
-- ---------------------------------------------------------------------------
CREATE TABLE projection.actor (
  actor_id          uuid PRIMARY KEY,
  tenant_id         uuid NOT NULL,
  kind              text NOT NULL CHECK (kind IN ('human','agent','service','automation')),
  display_name      text NOT NULL,
  on_behalf_of      uuid,
  external_identity text,
  client            jsonb,
  revoked_at        timestamptz,
  registered_seq    bigint NOT NULL,
  UNIQUE (tenant_id, kind, external_identity, on_behalf_of)
);

CREATE TABLE projection.run (
  run_id         uuid PRIMARY KEY,
  tenant_id      uuid NOT NULL,
  actor_id       uuid NOT NULL,
  parent_run_id  uuid,
  correlation_id uuid NOT NULL,
  client         jsonb,
  purpose        text,
  started_at     timestamptz NOT NULL,
  finished_at    timestamptz,
  outcome        text,
  started_seq    bigint NOT NULL
);
CREATE INDEX run_parent_idx ON projection.run (parent_run_id) WHERE parent_run_id IS NOT NULL;
CREATE INDEX run_corr_idx   ON projection.run (correlation_id);

CREATE TABLE projection.context (
  context_id      uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  project_id      uuid NOT NULL,
  actor_id        uuid NOT NULL,
  run_id          uuid NOT NULL,
  attached_to     uuid,
  client          jsonb,
  opened_at       timestamptz NOT NULL,
  closed_at       timestamptz,
  closed_auto     boolean,
  summary         text,                 -- from context.closed or the last context.checkpointed
  current         text,                 -- "where we are" from the last checkpoint
  statement_count int  NOT NULL DEFAULT 0,
  checkpoint_count int NOT NULL DEFAULT 0,
  has_content     boolean GENERATED ALWAYS AS (statement_count > 0 OR checkpoint_count > 0) STORED,
  last_event_at   timestamptz NOT NULL,
  opened_seq      bigint NOT NULL
);
CREATE INDEX context_project_idx ON projection.context (tenant_id, project_id, closed_at DESC NULLS FIRST);
CREATE INDEX context_open_idx    ON projection.context (tenant_id, actor_id) WHERE closed_at IS NULL;

-- ---------------------------------------------------------------------------
-- projects · tasks · facts  (R1, R8)
-- ---------------------------------------------------------------------------
CREATE TABLE projection.project (
  project_id      uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  name            text NOT NULL,
  summary         text,
  repo_url        text,
  status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused','archived')),
  auto_created    boolean NOT NULL DEFAULT false,
  current         text,                  -- last checkpoint.current across contexts
  updated_at      timestamptz NOT NULL,  -- last event touching the project or its statements
  statement_count int NOT NULL DEFAULT 0,
  declared_seq    bigint NOT NULL,
  last_seq        bigint NOT NULL,
  UNIQUE (tenant_id, name)
);

CREATE TABLE projection.task (
  task_id      uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL,
  project_id   uuid NOT NULL REFERENCES projection.project(project_id),
  title        text NOT NULL,
  note         text,
  priority     text NOT NULL CHECK (priority IN ('high','medium','low')),
  blocked_by   text,
  open         boolean NOT NULL DEFAULT true,
  outcome      text CHECK (outcome IN ('done','dropped')),
  revision     int NOT NULL DEFAULT 1,
  declared_seq bigint NOT NULL,
  last_seq     bigint NOT NULL,
  last_event_id uuid NOT NULL
);
CREATE INDEX task_open_idx ON projection.task (project_id, priority) WHERE open;

CREATE TABLE projection.fact (
  tenant_id    uuid NOT NULL,
  project_id   uuid NOT NULL REFERENCES projection.project(project_id),
  kind         text NOT NULL CHECK (kind IN ('architecture','credential','access','note','tech_stack')),
  key          text NOT NULL,
  value        text,
  secret_ref   text,
  retired      boolean NOT NULL DEFAULT false,
  last_seq     bigint NOT NULL,
  last_event_id uuid NOT NULL,
  PRIMARY KEY (project_id, kind, key),
  CHECK (kind <> 'credential' OR (value IS NULL AND secret_ref IS NOT NULL))
);

-- ---------------------------------------------------------------------------
-- statements · versions  (R1, R2, R3, R6, R7)
-- statement = current view; statement_version = every revision with its seq validity range (SCD-2 by seq),
-- which answers recall.as_of on the transaction axis without replaying the journal.
-- ---------------------------------------------------------------------------
CREATE TABLE projection.statement (
  statement_id   uuid PRIMARY KEY,
  tenant_id      uuid NOT NULL,
  project_id     uuid NOT NULL REFERENCES projection.project(project_id),
  type           text NOT NULL CHECK (type IN ('bugfix','feature','refactor','decision','discovery','config','note','question')),
  title          text NOT NULL,
  body           text,
  concepts       text[] NOT NULL DEFAULT '{}',
  files          text[] NOT NULL DEFAULT '{}',
  external_ref   text,
  part_index     int,
  part_total     int,
  context_id     uuid,
  actor_id       uuid NOT NULL,          -- author of the asserted event
  status         text NOT NULL DEFAULT 'current' CHECK (status IN ('current','amended','superseded','retracted','folded')),
  revision       int NOT NULL DEFAULT 1,
  effective_at   timestamptz NOT NULL,
  recorded_at    timestamptz NOT NULL,   -- of the asserted event
  supersedes     uuid,
  superseded_by  uuid,
  folded_under   uuid,
  confirmations  int NOT NULL DEFAULT 0,
  purged         boolean NOT NULL DEFAULT false,
  asserted_event_id uuid NOT NULL,
  asserted_seq   bigint NOT NULL,
  last_event_id  uuid NOT NULL,
  last_seq       bigint NOT NULL
);
CREATE INDEX statement_project_idx  ON projection.statement (tenant_id, project_id, effective_at DESC);
CREATE INDEX statement_status_idx   ON projection.statement (project_id, status, effective_at DESC);
CREATE INDEX statement_extref_idx   ON projection.statement (project_id, external_ref) WHERE external_ref IS NOT NULL;
CREATE INDEX statement_context_idx  ON projection.statement (context_id) WHERE context_id IS NOT NULL;
CREATE INDEX statement_title_trgm   ON projection.statement USING gin (title gin_trgm_ops);
CREATE INDEX statement_concepts_idx ON projection.statement USING gin (concepts);
CREATE INDEX statement_files_idx    ON projection.statement USING gin (files);

CREATE TABLE projection.statement_version (
  statement_id  uuid NOT NULL REFERENCES projection.statement(statement_id),
  revision      int  NOT NULL,
  title         text NOT NULL,
  body          text,
  type          text NOT NULL,
  concepts      text[] NOT NULL DEFAULT '{}',
  files         text[] NOT NULL DEFAULT '{}',
  status        text NOT NULL,           -- status as of this version's range
  reason        text,
  effective_at  timestamptz NOT NULL,
  from_seq      bigint NOT NULL,         -- version valid for seq in [from_seq, to_seq)
  to_seq        bigint,                  -- NULL = current
  event_id      uuid NOT NULL,
  actor_id      uuid NOT NULL,
  PRIMARY KEY (statement_id, revision)
);
CREATE INDEX statement_version_range_idx ON projection.statement_version (statement_id, from_seq, to_seq);

-- lexical index (R2) — separate table so long bodies do not double the statement row (decision 2026-09-21).
-- Fed by the projector on asserted/amended/purged; title/concepts weight A, body weight B; 'simple' config,
-- word forms are expanded at query time from search_vocab.
CREATE TABLE projection.statement_lexical (
  statement_id uuid PRIMARY KEY REFERENCES projection.statement(statement_id),
  tsv          tsvector NOT NULL,
  last_seq     bigint NOT NULL
);
CREATE INDEX statement_lexical_tsv_idx ON projection.statement_lexical USING gin (tsv);

-- word forms / user dictionary carried over from the current server (011_search_trgm.search_vocab)
CREATE TABLE projection.search_vocab (
  tenant_id  uuid NOT NULL,
  word       text NOT NULL,
  freq       int  NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, word)
);
CREATE INDEX search_vocab_trgm ON projection.search_vocab USING gin (word gin_trgm_ops);

-- ---------------------------------------------------------------------------
-- links  (R7, R9, R10)
-- ---------------------------------------------------------------------------
CREATE TABLE projection.link (
  link_id      uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL,
  from_type    text NOT NULL,
  from_id      uuid NOT NULL,
  to_type      text NOT NULL,
  to_id        uuid NOT NULL,
  relation     text NOT NULL CHECK (relation IN ('related','contradicts','duplicates','confirms','about','derived_from')),
  note         text,
  actor_id     uuid NOT NULL,
  retired      boolean NOT NULL DEFAULT false,
  asserted_seq bigint NOT NULL,
  UNIQUE (from_type, from_id, to_type, to_id, relation)
);
CREATE INDEX link_from_idx ON projection.link (from_id) WHERE NOT retired;
CREATE INDEX link_to_idx   ON projection.link (to_id)   WHERE NOT retired;

-- ---------------------------------------------------------------------------
-- consumers  (R1, R5)
-- ---------------------------------------------------------------------------
CREATE TABLE projection.consumer (
  tenant_id   uuid NOT NULL,
  consumer    text NOT NULL CHECK (consumer ~ '^[a-z][a-z0-9-]*$'),
  seq         bigint NOT NULL,
  updated_at  timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, consumer)
);

-- ---------------------------------------------------------------------------
-- grants · the app role is the projector; nobody else writes here
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA projection TO litopys_app;

COMMIT;
