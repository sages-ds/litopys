-- Litopys · 0045 · provider keys and runner status (#245)
-- A key a person enters on the settings page is sealed with the app key (LITOPYS_SECRET_KEY, AES-256-GCM bound to the
-- executor) and kept here, outside the journal and the projections: a secret in the append-only journal could not be
-- erased, and a projection rebuild must not lose it (as auth.token). The journal gets only the key's fingerprint
-- (actor.amended, executor.key). runner_status is what each runner reported last — operational, not history.

BEGIN;

CREATE TABLE auth.executor_key (
  actor_id     uuid PRIMARY KEY,                     -- the executor (agent / service actor, projection.actor is derived)
  tenant_id    uuid NOT NULL REFERENCES tenant(tenant_id),
  owner_id     uuid NOT NULL,                        -- the person whose executor it is
  nonce        bytea NOT NULL,
  sealed       bytea NOT NULL,
  fingerprint  text NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{8}$'),
  set_at       timestamptz NOT NULL DEFAULT now(),
  set_by       uuid NOT NULL,
  checked_at   timestamptz,                          -- last "Check" of the key against the vendor
  check_ok     boolean,
  check_note   text CHECK (length(check_note) <= 500)
);
CREATE INDEX executor_key_owner_idx ON auth.executor_key (tenant_id, owner_id);

CREATE TABLE auth.runner_status (
  tenant_id    uuid NOT NULL REFERENCES tenant(tenant_id),
  owner_id     uuid NOT NULL,                        -- the owner of the runner's token
  runner       text NOT NULL CHECK (length(runner) BETWEEN 1 AND 100),
  version      text CHECK (length(version) <= 60),
  seen_at      timestamptz NOT NULL DEFAULT now(),
  executors    jsonb NOT NULL DEFAULT '[]',          -- [{name, actor_id, polled_at, error, backoff_s}]
  PRIMARY KEY (tenant_id, owner_id, runner)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON auth.executor_key TO litopys_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON auth.runner_status TO litopys_app;

COMMIT;
