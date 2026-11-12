-- Litopys · 0014 · native API v1: the human title of a project and the statement type proposal
-- title NULL means "same as the slug" (every project declared before 0014). Lookups by title ignore case, so the
-- index is on lower(title); it is not unique — an ambiguous title is an error of the call, not of the data.

BEGIN;

ALTER TABLE projection.project ADD COLUMN title text;
CREATE INDEX project_title_idx ON projection.project (tenant_id, lower(title)) WHERE title IS NOT NULL;

ALTER TABLE projection.statement DROP CONSTRAINT statement_type_check;
ALTER TABLE projection.statement ADD CONSTRAINT statement_type_check CHECK (type IN
  ('bugfix','feature','refactor','decision','discovery','config','note','question','proposal'));

COMMIT;
