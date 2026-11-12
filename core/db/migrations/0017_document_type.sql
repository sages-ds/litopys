-- Litopys · 0017 · the statement type document (todo #91): a curated note of the vault
-- becomes a document on the server — versions, provenance (external_ref = the note's uid, files = its path) and
-- derived_from links to the records it sums up. Only the CHECK changes.

BEGIN;

ALTER TABLE projection.statement DROP CONSTRAINT statement_type_check;
ALTER TABLE projection.statement ADD CONSTRAINT statement_type_check CHECK (type IN
  ('bugfix','feature','refactor','decision','discovery','config','note','question','proposal','document'));

COMMIT;
