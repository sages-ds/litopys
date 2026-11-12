-- Litopys · 0043 · the retention of one record (#136): retain_until — the day (Europe/Kyiv)
-- from which on_expiry applies; on_expiry retract — the worker retracts it; purge — the worker opens a deletion
-- proposal for it (an erasure always waits for a human's GO). retain_by is who set it (the worker acts on
-- their behalf, and an agent may not lift what a human set); retain_seq is the event that set it, so a proposal is
-- opened once per setting. Set by statement.assert or statement.retain (event statement.retention_set); older events
-- have none — every column stays NULL.

BEGIN;

ALTER TABLE projection.statement
  ADD COLUMN retain_until date,
  ADD COLUMN on_expiry    text CHECK (on_expiry IN ('retract','purge')),
  ADD COLUMN retain_by    uuid,
  ADD COLUMN retain_seq   bigint,
  ADD CONSTRAINT statement_retention_whole
    CHECK ((retain_until IS NULL) = (on_expiry IS NULL) AND (retain_until IS NULL) = (retain_by IS NULL)
           AND (retain_until IS NULL) = (retain_seq IS NULL));

-- the worker's daily pass looks only at records that carry a date
CREATE INDEX statement_retain_until ON projection.statement (tenant_id, retain_until) WHERE retain_until IS NOT NULL;

COMMIT;
