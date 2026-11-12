-- Litopys · 0019 · work links and who does a task (todo #120–#122).
-- In the source service one free-text owner carried both a person and the name of the executing project. Here:
--   owner_project — the project that executes the task when it is not the task's own (a handoff); NULL = its own;
--   assignee      — who does it: {"actor_id"} | {"role"} | {"agent"}; owner stays as legacy free text (facade 1.19).
-- Tasks of different projects are joined by ordinary links (link.asserted): depends_on (the waiting task -> the
-- task it waits for), handoff (the consumer's mirror -> the owner's executing task), duplicates, related.
-- New nullable columns: existing events carry neither field, so no rebuild is needed.

BEGIN;

ALTER TABLE projection.task ADD COLUMN owner_project uuid REFERENCES projection.project(project_id);
ALTER TABLE projection.task ADD COLUMN assignee jsonb;
CREATE INDEX task_owner_project_idx ON projection.task (owner_project) WHERE owner_project IS NOT NULL AND open AND NOT retracted;
-- links of tasks are read from both ends (waiting_on / blocking of the task row)
CREATE INDEX link_task_idx ON projection.link (to_id) WHERE to_type = 'task' AND NOT retired;
CREATE INDEX link_task_from_idx ON projection.link (from_id) WHERE from_type = 'task' AND NOT retired;

ALTER TABLE projection.link DROP CONSTRAINT link_relation_check;
ALTER TABLE projection.link ADD CONSTRAINT link_relation_check CHECK (relation IN
  ('related','contradicts','duplicates','confirms','about','derived_from','depends_on','predecessor_of','parallel_line_of','member_of',
   'handoff'));

COMMIT;
