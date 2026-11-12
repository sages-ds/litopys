-- Litopys · 0047 · the source of a fact (#246 П4): the record a fact was taken from (a budget cap from the
-- budget decision). When that record is superseded or retracted, fact_list and the start bundle mark the fact stale
-- with the record that replaced it — computed on read, no new event.

BEGIN;

ALTER TABLE projection.fact ADD COLUMN source_id uuid;

COMMIT;
