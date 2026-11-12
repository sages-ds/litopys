-- Litopys · 0029 · outcomes of a session per subject (#141)
-- session_close writes one statement of type outcome per subject the session moved: what came of it (title), its
-- state and the next step. statement.outcome keeps the structure the reads need — the subject (a statement or a
-- task by id, or a topic label), state, next, evidence — so "the latest outcome of T-12" is one indexed lookup.
-- It is content: purge clears it with the body. context.next_steps are the session's next steps beyond subjects.

BEGIN;

ALTER TABLE projection.statement DROP CONSTRAINT statement_type_check;
ALTER TABLE projection.statement ADD CONSTRAINT statement_type_check CHECK (type IN
  ('bugfix','feature','refactor','decision','discovery','config','note','question','proposal','document','outcome'));

ALTER TABLE projection.statement ADD COLUMN outcome jsonb;
CREATE INDEX statement_outcome_subject_idx ON projection.statement ((outcome->>'subject_id'), effective_at DESC)
  WHERE outcome IS NOT NULL;
CREATE INDEX statement_outcome_topic_idx ON projection.statement ((outcome->>'topic'), effective_at DESC)
  WHERE outcome IS NOT NULL;

ALTER TABLE projection.context ADD COLUMN next_steps text[];

COMMIT;
