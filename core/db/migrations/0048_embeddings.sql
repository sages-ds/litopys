-- Litopys · 0048 · embeddings of records (#246 П6): meaning search beside the lexical one. Derived data,
-- not a projection of the journal: a projection rebuild keeps it (embedding again costs model time); a new revision or
-- another model re-embeds the record. Vectors are L2-normalised real[] — the dot product is the cosine; no pgvector
-- needed at the size of a personal memory (thousands of records), pgvector stays an option for later.

BEGIN;

CREATE SCHEMA derived;

CREATE TABLE derived.statement_embedding (
  statement_id uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenant(tenant_id),
  revision     int NOT NULL,
  model        text NOT NULL CHECK (length(model) BETWEEN 1 AND 120),
  vec          real[] NOT NULL,
  embedded_at  timestamptz NOT NULL DEFAULT now()
);

GRANT USAGE ON SCHEMA derived TO litopys_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON derived.statement_embedding TO litopys_app;

COMMIT;
