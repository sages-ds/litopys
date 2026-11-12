-- Litopys · 0046 · structured fields of a record (#246 П2, П3): amount, currency, payee, paid_at, phone…
-- by the schema of the record's type (vocabularies.yaml fields). Current values on the statement, history on its
-- versions; ledger.summary adds up amount over the current records.

BEGIN;

ALTER TABLE projection.statement ADD COLUMN fields jsonb;
ALTER TABLE projection.statement_version ADD COLUMN fields jsonb;

-- ledger.summary: the project's current records that carry an amount
CREATE INDEX statement_amount_idx ON projection.statement (project_id) WHERE fields ? 'amount' AND status = 'current';

COMMIT;
