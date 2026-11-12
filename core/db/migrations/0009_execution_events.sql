-- Litopys · 0009 · execution events: telemetry stream outside the journal (todo #41)
-- domain/envelope.yaml: execution.event does NOT use the event envelope and does NOT go to the journal of
-- statements — a separate stream with its own retention. First producer: POST /api/events of the memory
-- facade (PostToolUse hook of the client plugin: METADATA of file edits, never file content).
-- A consumer folds a context's events into one draft statement and marks them processed; a sweep deletes
-- rows older than the retention (LITOPYS_EXECUTION_TTL_DAYS, default 14).
-- Owned like the projections (0008): RLS on owner_id; unlike them, rows are written by the edge.

BEGIN;

CREATE SCHEMA execution;
GRANT USAGE ON SCHEMA execution TO litopys_app;

CREATE TABLE execution.event (
  event_id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenant(tenant_id),
  owner_id     uuid NOT NULL,                        -- root human (docs/03)
  actor_id     uuid NOT NULL,                        -- the principal that sent it
  context_id   uuid NOT NULL,                        -- live context at receipt (projection.context, no FK: derived)
  project_id   uuid NOT NULL,
  kind         text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$'),
  payload      jsonb NOT NULL,
  recorded_at  timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz
);
CREATE INDEX event_context_idx ON execution.event (context_id) WHERE processed_at IS NULL;
CREATE INDEX event_recorded_idx ON execution.event (recorded_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON execution.event TO litopys_app;

ALTER TABLE execution.event ENABLE ROW LEVEL SECURITY;
CREATE POLICY owner_rw ON execution.event FOR ALL TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() = 'system')
  WITH CHECK (owner_id = litopys_owner() OR litopys_scope() = 'system');

-- GET /version reports the latest migration (versions and checksums only)
GRANT SELECT ON schema_migrations TO litopys_app;

COMMIT;
