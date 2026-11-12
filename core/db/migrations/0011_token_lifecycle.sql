-- Litopys · 0011 · token lifetime (todo #33)
-- New tokens always carry an expiry (default 90 days, at most 365; admin-role tokens at most 7 —
-- LITOPYS_TOKEN_TTL_DAYS / LITOPYS_ADMIN_TOKEN_TTL_DAYS). NULL is the legacy "no expiry" of tokens issued
-- before this migration; an expired token is refused like a revoked one, but the edge tells it apart
-- (WWW-Authenticate error_description="expired").

BEGIN;

ALTER TABLE auth.token ADD COLUMN expires_at timestamptz;
CREATE INDEX token_expiry_idx ON auth.token (expires_at) WHERE revoked_at IS NULL AND expires_at IS NOT NULL;

COMMIT;
