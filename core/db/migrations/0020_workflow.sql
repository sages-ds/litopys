-- Litopys · 0020 · workflow definitions with immutable versions (todo #123).
-- A workflow is a stable identity (project, key) with metadata; every version is a frozen definition: steps, params,
-- triggers. workflow.declared writes version 1, workflow.superseded the next one; nothing edits a version. Executions,
-- step attempts and approvals come with the next slice (their own tables).

BEGIN;

CREATE TABLE projection.workflow (
  workflow_id   uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL,
  owner_id      uuid NOT NULL,
  project_id    uuid NOT NULL REFERENCES projection.project(project_id),
  key           text NOT NULL,
  title         text NOT NULL,
  purpose       text,
  version       int  NOT NULL DEFAULT 1 CHECK (version >= 1),   -- the current (latest) version
  retired       boolean NOT NULL DEFAULT false,
  revision      int  NOT NULL DEFAULT 1,
  declared_seq  bigint NOT NULL,
  last_seq      bigint NOT NULL,
  last_event_id uuid NOT NULL,
  UNIQUE (project_id, key)
);

CREATE TABLE projection.workflow_version (
  workflow_id  uuid NOT NULL REFERENCES projection.workflow(workflow_id),
  version      int  NOT NULL CHECK (version >= 1),
  tenant_id    uuid NOT NULL,
  owner_id     uuid NOT NULL,
  definition   jsonb NOT NULL,
  steps        int  NOT NULL CHECK (steps >= 1),
  reason       text,
  created_at   timestamptz NOT NULL,
  created_seq  bigint NOT NULL,
  event_id     uuid NOT NULL,
  PRIMARY KEY (workflow_id, version)
);

GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON projection.workflow, projection.workflow_version TO litopys_app;

DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['workflow','workflow_version'] LOOP
    EXECUTE format('ALTER TABLE projection.%I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format($p$CREATE POLICY owner_read ON projection.%I FOR SELECT TO litopys_app
                      USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'))$p$, t);
    EXECUTE format($p$CREATE POLICY projector_insert ON projection.%I FOR INSERT TO litopys_app
                      WITH CHECK (litopys_scope() = 'projector')$p$, t);
    EXECUTE format($p$CREATE POLICY projector_update ON projection.%I FOR UPDATE TO litopys_app
                      USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'))
                      WITH CHECK (litopys_scope() = 'projector')$p$, t);
    EXECUTE format($p$CREATE POLICY projector_delete ON projection.%I FOR DELETE TO litopys_app
                      USING (litopys_scope() = 'projector')$p$, t);
  END LOOP;
END $$;

COMMIT;
