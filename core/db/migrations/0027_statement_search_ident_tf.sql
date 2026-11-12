-- Litopys · 0027 · how often each identifier occurs, in the search projection v2 (#133 x8)
-- ident_tf[i] is how many times idents[i] occurs in the statement (core/textnorm Doc.IdentTF). Search v2 uses it only
-- to break ties in the identifier tier, after the title (x7) and before recency: of two records naming the code
-- equally, the one about it — naming it again and again — ranks above a mention in passing.
-- Filled by projector v14, which rebuilds the projection from the journal.

BEGIN;

ALTER TABLE projection.statement_search ADD COLUMN ident_tf int[] NOT NULL DEFAULT '{}';

COMMIT;
