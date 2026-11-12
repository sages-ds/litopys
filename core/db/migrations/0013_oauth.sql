-- Litopys · 0013 · OAuth resource server (todo #33b, IdP Keycloak)
-- A person signs in through the broker-IdP; the access token is a JWT the facade verifies by JWKS.
-- auth.identity: the key of a person is (iss, sub); e-mail links only on the FIRST sign-in and only when the
--   IdP says email_verified. auth.oauth_grant: one consent of a person for one client — the profile chosen on
--   the consent screen (D5) and a revocation checked on EVERY request (a valid JWT of a revoked grant or a
--   revoked person is refused). Edge data like auth.token: outside the journal.

BEGIN;

CREATE TABLE auth.identity (
  tenant_id  uuid NOT NULL REFERENCES tenant(tenant_id),
  actor_id   uuid NOT NULL,                      -- the human (projection.actor is derived, no FK)
  iss        text NOT NULL,
  sub        text NOT NULL,
  email      text,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (iss, sub)
);
CREATE INDEX identity_actor_idx ON auth.identity (tenant_id, actor_id);

CREATE TABLE auth.oauth_grant (
  grant_id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenant(tenant_id),
  actor_id     uuid NOT NULL,
  client_id    text NOT NULL,
  profile      text NOT NULL CHECK (profile IN ('chat', 'core')),  -- admin / token:issue never via OAuth
  vendor       text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  revoked_at   timestamptz,
  UNIQUE (tenant_id, actor_id, client_id)
);

GRANT USAGE ON SCHEMA auth TO litopys_app;
GRANT SELECT, INSERT, UPDATE ON auth.identity, auth.oauth_grant TO litopys_app;

COMMIT;
