-- Litopys · 0015 · fact kinds of the Claude Code plugin: binding (a folder of a machine bound to the
-- project, P2) and the layer facts instruction / style / agent / skill. Only the CHECK
-- changes: facts of the new kinds are ordinary facts (fact.recorded / fact.retired), no projection rebuild.

BEGIN;

ALTER TABLE projection.fact DROP CONSTRAINT fact_kind_check;
ALTER TABLE projection.fact ADD CONSTRAINT fact_kind_check CHECK (kind IN
  ('architecture','credential','access','note','tech_stack','binding','instruction','style','agent','skill'));

COMMIT;
