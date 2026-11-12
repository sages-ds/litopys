-- Litopys · 0005 · tasks follow the todo v2 model of the source service
-- projection.task is derived: it is recreated here and refilled by a projector rebuild
-- (projector version 4; `litopys projector rebuild`). The journal is untouched.
-- Retracted tasks keep their row (retracted = true, hidden from lists) so num is never reused.

BEGIN;

DROP TABLE projection.task;

CREATE TABLE projection.task (
  task_id       uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL,
  project_id    uuid NOT NULL REFERENCES projection.project(project_id),
  num           int  NOT NULL CHECK (num > 0),
  kind          text NOT NULL DEFAULT 'task'
                CHECK (kind IN ('task','bug','requirement','question','risk','test')),
  title         text NOT NULL,
  body          text,
  status        text NOT NULL DEFAULT 'open'
                CHECK (status IN ('backlog','open','analysis','in_progress','review','testing','waiting','blocked','done','dropped')),
  open          boolean GENERATED ALWAYS AS (status NOT IN ('done','dropped')) STORED,
  priority      text CHECK (priority IN ('high','medium','low')),
  rank          int,
  role          text CHECK (role IN ('dev','dba','ops','pm','ba','qa')),
  owner         text,
  due           date,
  parent        int,
  acceptance    text,
  severity      text CHECK (severity IN ('critical','major','minor','trivial')),
  resolution    text,
  labels        text[] NOT NULL DEFAULT '{}',
  refs          text[] NOT NULL DEFAULT '{}',
  closed_at     timestamptz,
  retracted     boolean NOT NULL DEFAULT false,
  revision      int  NOT NULL DEFAULT 1,
  declared_seq  bigint NOT NULL,
  last_seq      bigint NOT NULL,
  last_event_id uuid NOT NULL,
  UNIQUE (project_id, num)
);
CREATE INDEX task_open_idx  ON projection.task (project_id, status) WHERE open AND NOT retracted;
CREATE INDEX task_label_idx ON projection.task USING gin (labels);

GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON projection.task TO litopys_app;

COMMIT;
