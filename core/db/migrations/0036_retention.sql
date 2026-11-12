-- Litopys · 0036 · retention labels (#136 step 5): a project fact of kind retention says what the worker
-- retracts or erases and when; without one nothing is removed automatically. Only the CHECK changes: a label is an
-- ordinary fact (fact.recorded / fact.retired), as the layer kinds of 0015 — no projection rebuild.

BEGIN;

ALTER TABLE projection.fact DROP CONSTRAINT fact_kind_check;
ALTER TABLE projection.fact ADD CONSTRAINT fact_kind_check CHECK (kind IN
  ('architecture','credential','access','note','tech_stack','binding','instruction','style','agent','skill','retention'));

COMMIT;
