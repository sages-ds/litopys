-- Litopys · 0039 · human confirmation of retention labels outside the chat (#212, owner decision
-- 2026-10-30: P1 a, P2 a, P3 a). A retention label that erases (purge) or covers every record of a project
-- (applies_to: record) is only proposed by fact.record; the person confirms that very version of it in the viewer
-- (event fact.confirmed, written by the viewer's confirmation handler alone) and only then does the worker apply it.
-- confirmed_at: when the current version of the fact was confirmed; every fact.recorded clears it. Filled by the
-- projector (version 24 rebuilds the projections, so labels recorded before stay unconfirmed — proposed).

BEGIN;

ALTER TABLE projection.fact ADD COLUMN confirmed_at timestamptz;

COMMIT;
