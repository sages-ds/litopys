-- Litopys · 0026 · what the title alone holds, in the search projection v2 (#133 x7)
-- The canonical identifiers and the uk/en stems of the title, beside those of the whole statement. Search v2 uses
-- them only to break ties: of two records matching the query equally, the one whose title carries the match ranks
-- first, before recency (x4–x6 broke such ties by recency alone and lost to v1 on identifiers and decisions).
-- Never matched on, never a channel of their own (x5 tried a channel and lost) — no index.
-- Filled by projector v13, which rebuilds the projection from the journal.

BEGIN;

ALTER TABLE projection.statement_search
  ADD COLUMN title_idents text[] NOT NULL DEFAULT '{}',
  ADD COLUMN title_stems  text[] NOT NULL DEFAULT '{}';

COMMIT;
