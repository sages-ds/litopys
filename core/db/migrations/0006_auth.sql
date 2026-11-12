-- Litopys · 0006 · own bearer tokens (not the tokens of the source service)
-- Tokens are an edge concern, not knowledge: they live outside the journal and the projections,
-- so purge and replay never touch them. Only the SHA-256 of a token is stored; the raw value
-- (LTP_ + 43 base64url chars) is shown once at issue time.

BEGIN;

CREATE SCHEMA auth;

CREATE TABLE auth.token (
  token_id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenant(tenant_id),
  actor_id     uuid NOT NULL,                       -- a human actor (projection.actor is derived, no FK)
  role         text NOT NULL CHECK (role IN ('owner', 'admin')),
  label        text CHECK (length(label) <= 100),
  hash         bytea NOT NULL UNIQUE,              -- sha256(raw token)
  fingerprint  text NOT NULL,                      -- first 12 hex chars of the hash, for listings
  created_at   timestamptz NOT NULL DEFAULT now(),
  created_by   uuid,                               -- actor that issued it (NULL = bootstrap)
  last_used_at timestamptz,
  revoked_at   timestamptz
);
CREATE INDEX token_actor_idx ON auth.token (tenant_id, actor_id);

GRANT USAGE ON SCHEMA auth TO litopys_app;
GRANT SELECT, INSERT, UPDATE ON auth.token TO litopys_app;

COMMIT;
