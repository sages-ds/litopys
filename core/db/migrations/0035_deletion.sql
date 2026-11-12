-- Litopys · 0035 · deletion proposals D-N (#136 step 3)
-- Anyone proposes to retract or erase a record or a task, with the impact it would have; only a human approves or
-- rejects. A retract is done in the transaction of the GO; an erase is done by `litopys worker`, which holds the admin
-- pool (owner's decision P1, 2026-10-24), and recorded by deletion.executed. Numbers D-N are per project.

BEGIN;

CREATE TABLE projection.deletion (
  deletion_id   uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL,
  owner_id      uuid NOT NULL,
  project_id    uuid NOT NULL REFERENCES projection.project(project_id),
  num           int  NOT NULL CHECK (num > 0),
  target_type   text NOT NULL CHECK (target_type IN ('statement','task')),
  target_id     uuid NOT NULL,
  action        text NOT NULL CHECK (action IN ('retract','purge')),
  reason        text NOT NULL,
  secret        boolean NOT NULL DEFAULT false,
  impact        jsonb NOT NULL,                       -- the counts the GO is given on: {relation: n, outcomes: n}
  state         text NOT NULL DEFAULT 'open' CHECK (state IN ('open','approved','done','rejected')),
  proposed_by   uuid NOT NULL,
  proposed_at   timestamptz NOT NULL,
  decided_by    uuid,
  decided_at    timestamptz,
  note          text,                                 -- of the GO, or the reason of a rejection
  proposed_seq  bigint NOT NULL,
  last_seq      bigint NOT NULL,
  last_event_id uuid NOT NULL,
  UNIQUE (project_id, num)
);
CREATE INDEX deletion_open_idx ON projection.deletion (project_id) WHERE state IN ('open','approved');
-- one open proposal per target and action: a repeat answers with the open one
CREATE UNIQUE INDEX deletion_target_open_idx ON projection.deletion (target_id, action) WHERE state = 'open';

GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON projection.deletion TO litopys_app;

ALTER TABLE projection.deletion ENABLE ROW LEVEL SECURITY;
CREATE POLICY owner_read ON projection.deletion FOR SELECT TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'));
CREATE POLICY projector_insert ON projection.deletion FOR INSERT TO litopys_app
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_update ON projection.deletion FOR UPDATE TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'))
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_delete ON projection.deletion FOR DELETE TO litopys_app
  USING (litopys_scope() = 'projector');

COMMIT;
