-- Litopys · 0038 · families of record types (#134 step 2): a record type belongs to a usage vocabulary
-- (core/vocab) and to one of eight families; cross-cutting reads ("every decision", "open questions") go by the
-- family. The CHECK on type goes — a new type is catalog data, not a migration; the verb checks the type against the
-- project's vocabularies instead. Events from now on carry the family; older ones get it from the fixed legacy map,
--the same in the projector. The fact kind vocabulary turns a vocabulary on or off for a layer.

BEGIN;

ALTER TABLE projection.statement DROP CONSTRAINT statement_type_check;

ALTER TABLE projection.statement ADD COLUMN family text;
UPDATE projection.statement SET family = CASE type
  WHEN 'discovery' THEN 'finding'
  WHEN 'bugfix' THEN 'event' WHEN 'feature' THEN 'event' WHEN 'refactor' THEN 'event' WHEN 'config' THEN 'event'
  ELSE type END;
ALTER TABLE projection.statement ALTER COLUMN family SET NOT NULL;
ALTER TABLE projection.statement ADD CONSTRAINT statement_family_check CHECK (family IN
  ('decision','proposal','finding','question','event','document','note','outcome'));
CREATE INDEX statement_family_idx ON projection.statement (owner_id, family, effective_at);

ALTER TABLE projection.fact DROP CONSTRAINT fact_kind_check;
ALTER TABLE projection.fact ADD CONSTRAINT fact_kind_check CHECK (kind IN
  ('architecture','credential','access','note','tech_stack','binding','instruction','style','agent','skill','retention',
   'vocabulary'));

COMMIT;
