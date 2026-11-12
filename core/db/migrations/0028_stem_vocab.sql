-- Litopys · 0028 · the dictionary of stems for the typo channel of search v2 (#139)
-- Every stem the search projection v2 ever held for an owner, with a trigram index: the typo channel (x9) looks
-- up the nearest stem of an unknown word here instead of collecting DISTINCT unnest(stems) over all the person's
-- records on every query (recall_search up to 1.2 s at a 500 ms budget). A superset, like search_vocab: stems of
-- earlier revisions and retracted records stay, and the query keeps only stems a live record of the person holds;
-- purge recomputes it, so no word of purged content survives. Filled by projector v15 from the journal.

BEGIN;

CREATE TABLE projection.stem_vocab (
  tenant_id uuid NOT NULL,
  owner_id  uuid NOT NULL,
  stem      text NOT NULL,
  PRIMARY KEY (tenant_id, owner_id, stem)
);
CREATE INDEX stem_vocab_trgm ON projection.stem_vocab USING gin (stem gin_trgm_ops);

GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON projection.stem_vocab TO litopys_app;

-- the owner policies of 0008
ALTER TABLE projection.stem_vocab ENABLE ROW LEVEL SECURITY;
CREATE POLICY owner_read ON projection.stem_vocab FOR SELECT TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'));
CREATE POLICY projector_insert ON projection.stem_vocab FOR INSERT TO litopys_app
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_update ON projection.stem_vocab FOR UPDATE TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'))
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_delete ON projection.stem_vocab FOR DELETE TO litopys_app
  USING (litopys_scope() = 'projector');

COMMIT;
