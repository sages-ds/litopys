-- Litopys · 0003 · TRUNCATE guard for the journal (decision 2026-09-29)
-- journal_guard is a row trigger and never sees TRUNCATE, so the table owner could empty the
-- journal. Refuse TRUNCATE on journal and journal_head for everyone, owner included.
-- TRUNCATE ... CASCADE through tenant fires these triggers too.
-- Consequence: a stand is reset only by dropping and recreating the database.

BEGIN;

CREATE OR REPLACE FUNCTION journal_truncate_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'journal is append-only (truncate of % refused)', TG_TABLE_NAME
    USING ERRCODE = 'integrity_constraint_violation';
END $$;

CREATE TRIGGER journal_truncate_guard_trg
  BEFORE TRUNCATE ON journal
  FOR EACH STATEMENT EXECUTE FUNCTION journal_truncate_guard();

CREATE TRIGGER journal_head_truncate_guard_trg
  BEFORE TRUNCATE ON journal_head
  FOR EACH STATEMENT EXECUTE FUNCTION journal_truncate_guard();

COMMIT;
