-- Litopys · 0007 · numeric statement ids for the memory_* facade
-- The current contract exposes observations by integer id; the vault mirror (by record id), curated
-- derived_from links and legacy record references depend on them. Edge table, like auth.token: outside
-- the journal and projections (replay and purge never touch it). Imported statements keep their
-- source number (backfilled from imported_from.observation_id); new ones continue after max.

BEGIN;

CREATE SCHEMA facade;

CREATE TABLE facade.legacy_id (
  statement_id uuid   PRIMARY KEY,
  tenant_id    uuid   NOT NULL REFERENCES tenant(tenant_id),
  legacy_id    bigint NOT NULL CHECK (legacy_id > 0),
  UNIQUE (tenant_id, legacy_id)
);

GRANT USAGE ON SCHEMA facade TO litopys_app;
GRANT SELECT, INSERT ON facade.legacy_id TO litopys_app;

COMMIT;
