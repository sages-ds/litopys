-- Litopys · 0034 · quarantine (#136 step 2)
-- journal.quarantined hides an entity's content from every read without erasing it; journal.released (a human
-- only) shows it again. The journal is untouched: reads of journal.changes mask the payload of a quarantined
-- entity's events at read time.
--
-- One condition of visibility per kind, a generated column (visibleSQL as a column, so the facade
-- and SQL-only reads share it): statement.hidden = purged OR quarantined (a retracted record stays readable as
-- history); task.hidden = retracted OR purged OR quarantined. Projector v20 fills quarantined on replay.

BEGIN;

ALTER TABLE projection.statement ADD COLUMN quarantined boolean NOT NULL DEFAULT false;
ALTER TABLE projection.statement ADD COLUMN hidden boolean GENERATED ALWAYS AS (purged OR quarantined) STORED;

ALTER TABLE projection.task ADD COLUMN quarantined boolean NOT NULL DEFAULT false;
DROP INDEX projection.task_open_idx;
ALTER TABLE projection.task DROP COLUMN hidden;
ALTER TABLE projection.task ADD COLUMN hidden boolean GENERATED ALWAYS AS (retracted OR purged OR quarantined) STORED;
CREATE INDEX task_open_idx ON projection.task (project_id, status) WHERE open AND NOT hidden;

COMMIT;
