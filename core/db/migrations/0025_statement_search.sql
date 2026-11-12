-- Litopys · 0025 · search projection v2 (#133)
-- What core/textnorm.Analyze gives for a statement (title + concepts + body): canonical identifiers (idents) and
-- uk/en stems without function words (stems). Derived like every projection: the projector fills it on
-- asserted / amended, removes it on purge, and a new textnorm.Version rebuilds it from the journal (norm_version
-- is part of the projector version). Texts are Ukrainian and English only — NO RUSSIAN SUPPORT.
-- The old lexical index (statement_lexical, search_vocab) stays until the new search is switched on.

BEGIN;

CREATE TABLE projection.statement_search (
  statement_id uuid PRIMARY KEY REFERENCES projection.statement(statement_id),
  tenant_id    uuid NOT NULL,
  owner_id     uuid NOT NULL,
  idents       text[] NOT NULL DEFAULT '{}',
  stems        tsvector NOT NULL,
  norm_version text NOT NULL,
  last_seq     bigint NOT NULL
);
CREATE INDEX statement_search_idents_idx ON projection.statement_search USING gin (idents);
CREATE INDEX statement_search_stems_idx  ON projection.statement_search USING gin (stems);

GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON projection.statement_search TO litopys_app;

-- the owner policies of 0008
ALTER TABLE projection.statement_search ENABLE ROW LEVEL SECURITY;
CREATE POLICY owner_read ON projection.statement_search FOR SELECT TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'));
CREATE POLICY projector_insert ON projection.statement_search FOR INSERT TO litopys_app
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_update ON projection.statement_search FOR UPDATE TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'))
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_delete ON projection.statement_search FOR DELETE TO litopys_app
  USING (litopys_scope() = 'projector');

COMMIT;
