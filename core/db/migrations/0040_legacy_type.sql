-- Litopys · 0040 · the old type of an imported record (#134 step 6b). The migration from the other
-- provider maps the old 7 types into the vocabularies; where the map changed a type (bugfix → event in a project that
-- is not technical, bugfix → dev.bugfix in one that is), the original stays in imported_from.type of the asserted
-- event and in legacy_type of a later amendment. legacy_type carries it to reads: the memory_* facade shows and
-- filters the old name, recall.read returns it. An amendment of the type without one clears it (the record is no
-- longer what the old service called it). Filled by the projector (version 25 rebuilds the projections).

BEGIN;

ALTER TABLE projection.statement ADD COLUMN legacy_type text;
ALTER TABLE projection.statement_version ADD COLUMN legacy_type text;

COMMIT;
