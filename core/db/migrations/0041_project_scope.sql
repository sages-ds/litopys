-- Litopys · 0041 · the scope of a project (owner's decision 2026-10-31 on #233 a): what belongs to the
-- project in one line of up to 200 characters, so a model routes a record without reading every summary. Set by
-- project.declare / project.amend; the migration from the other provider fills it from the summary (a line
-- "МЕЖІ: …", else its first sentence). Filled by the projector from the events; older events have none.

BEGIN;

ALTER TABLE projection.project ADD COLUMN scope text;

COMMIT;
