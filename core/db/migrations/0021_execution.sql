-- Litopys · 0021 · workflow executions, step attempts and approvals (todo #123).
-- An execution is a run of one frozen version. Step states are not stored: core/flow derives them from the
-- definition, the attempts and the approvals, so the projection only holds facts — who took which step until
-- when (an attempt with a lease), how it ended, and which GO of a person exists.

BEGIN;

CREATE TABLE projection.execution (
  execution_id        uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  owner_id            uuid NOT NULL,
  project_id          uuid NOT NULL REFERENCES projection.project(project_id),
  workflow_id         uuid NOT NULL REFERENCES projection.workflow(workflow_id),
  version             int  NOT NULL,
  params              jsonb,
  task_id             uuid,
  parent_execution_id uuid,
  parent_step_key     text,
  state               text NOT NULL DEFAULT 'working' CHECK (state IN ('working','completed','failed','canceled')),
  summary             text,
  started_at          timestamptz NOT NULL,
  closed_at           timestamptz,
  revision            int NOT NULL DEFAULT 1,
  started_seq         bigint NOT NULL,
  last_seq            bigint NOT NULL,
  last_event_id       uuid NOT NULL,
  FOREIGN KEY (workflow_id, version) REFERENCES projection.workflow_version(workflow_id, version)
);
CREATE INDEX execution_open_idx ON projection.execution (owner_id, project_id) WHERE state = 'working';

CREATE TABLE projection.step_attempt (
  attempt_id   uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL,
  owner_id     uuid NOT NULL,
  execution_id uuid NOT NULL REFERENCES projection.execution(execution_id),
  step_key     text NOT NULL,
  n            int  NOT NULL CHECK (n >= 1),
  actor_id     uuid NOT NULL,
  run_id       uuid,
  state        text NOT NULL DEFAULT 'working' CHECK (state IN ('working','completed','failed','canceled','rejected')),
  lease_until  timestamptz NOT NULL,
  output       jsonb,
  error        text,
  approval_id  uuid,
  started_at   timestamptz NOT NULL,
  finished_at  timestamptz,
  last_seq     bigint NOT NULL,
  UNIQUE (execution_id, step_key, n)
);

CREATE TABLE projection.approval (
  approval_id  uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL,
  owner_id     uuid NOT NULL,
  execution_id uuid NOT NULL REFERENCES projection.execution(execution_id),
  step_key     text NOT NULL,
  actor_id     uuid NOT NULL,
  items        jsonb,
  note         text,
  revoked      boolean NOT NULL DEFAULT false,
  used_by      uuid,
  granted_at   timestamptz NOT NULL,
  last_seq     bigint NOT NULL
);
CREATE INDEX approval_exec_idx ON projection.approval (execution_id, step_key) WHERE NOT revoked;

GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON projection.execution, projection.step_attempt, projection.approval TO litopys_app;

DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['execution','step_attempt','approval'] LOOP
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
