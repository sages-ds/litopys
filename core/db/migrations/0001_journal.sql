-- Litopys · 0001 · journal (append-only event log)
-- Applies to PostgreSQL 16+. Envelope: domain/envelope.yaml (envelope_version 1).
-- NOT YET EXECUTED against a live server (no local PG on 2026-09-21) — first run on the stand.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid() for tenants/actors created by SQL

-- Roles: the application never gets UPDATE/DELETE on the journal.
-- Created idempotently; passwords are set out of band (OpenBao / env), never here.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'litopys_app')   THEN CREATE ROLE litopys_app   NOLOGIN; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'litopys_admin') THEN CREATE ROLE litopys_admin NOLOGIN; END IF;
END $$;

-- ---------------------------------------------------------------------------
-- tenants · one row "default" on start (envelope.tenant_id)
-- ---------------------------------------------------------------------------
CREATE TABLE tenant (
  tenant_id   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name        text NOT NULL UNIQUE,
  created_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO tenant (name) VALUES ('default');

-- ---------------------------------------------------------------------------
-- journal_head · gapless, commit-ordered seq per tenant
-- Writers do: UPDATE journal_head SET last_seq = last_seq + 1 WHERE tenant_id = $1 RETURNING last_seq
-- The row lock serialises writers; a rolled-back transaction gives its number back.
-- Consequence (E6): a consumer that has seen seq N has seen every event with seq <= N.
-- ---------------------------------------------------------------------------
CREATE TABLE journal_head (
  tenant_id   uuid PRIMARY KEY REFERENCES tenant(tenant_id),
  last_seq    bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0)
);
INSERT INTO journal_head (tenant_id) SELECT tenant_id FROM tenant;

-- ---------------------------------------------------------------------------
-- journal · the only source of truth
-- ---------------------------------------------------------------------------
CREATE TABLE journal (
  seq               bigint       NOT NULL,
  tenant_id         uuid         NOT NULL REFERENCES tenant(tenant_id),
  event_id          uuid         NOT NULL,
  envelope_version  smallint     NOT NULL DEFAULT 1 CHECK (envelope_version = 1),
  schema_version    smallint     NOT NULL CHECK (schema_version >= 1),
  type              text         NOT NULL CHECK (type ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$'),
  recorded_at       timestamptz  NOT NULL DEFAULT clock_timestamp(),  -- transaction time; never updated
  effective_at      timestamptz  NOT NULL,                             -- valid time; defaults to recorded_at in the app
  actor_id          uuid         NOT NULL,
  run_id            uuid         NOT NULL,
  causation_id      uuid         NOT NULL,
  correlation_id    uuid         NOT NULL,
  entity_type       text         NOT NULL,
  entity_id         uuid         NOT NULL,
  context_id        uuid,
  payload           jsonb        NOT NULL,
  PRIMARY KEY (tenant_id, seq),
  UNIQUE (event_id)
);

-- Read paths: R5 changes by seq (PK); R3/R7 by entity; R11 by run/correlation; R1 by context.
CREATE INDEX journal_entity_idx      ON journal (tenant_id, entity_type, entity_id, seq);
CREATE INDEX journal_type_idx        ON journal (tenant_id, type, seq);
CREATE INDEX journal_run_idx         ON journal (run_id, seq);
CREATE INDEX journal_correlation_idx ON journal (correlation_id, seq);
CREATE INDEX journal_context_idx     ON journal (context_id, seq) WHERE context_id IS NOT NULL;
CREATE INDEX journal_recorded_idx    ON journal (tenant_id, recorded_at);   -- R6 recorded_at <= T → seq boundary

COMMENT ON TABLE  journal              IS 'Append-only event log. Invariant 1: nothing is deleted; invariant 7: order is seq.';
COMMENT ON COLUMN journal.recorded_at  IS 'Server clock at insert while holding the head lock. Informational; seq is the order.';
COMMENT ON COLUMN journal.effective_at IS 'When this became true about the world. May precede recorded_at (import, retrospective facts).';

-- ---------------------------------------------------------------------------
-- Immutability guard. UPDATE is allowed ONLY to the purge function (it sets litopys.purge = on),
-- and only on the payload column. DELETE is never allowed.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION journal_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'journal is append-only (delete of seq % refused)', OLD.seq USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF current_setting('litopys.purge', true) IS DISTINCT FROM 'on' THEN
    RAISE EXCEPTION 'journal is append-only (update of seq % refused)', OLD.seq USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF ROW(NEW.seq, NEW.tenant_id, NEW.event_id, NEW.envelope_version, NEW.schema_version, NEW.type, NEW.recorded_at,
         NEW.effective_at, NEW.actor_id, NEW.run_id, NEW.causation_id, NEW.correlation_id, NEW.entity_type, NEW.entity_id, NEW.context_id)
     IS DISTINCT FROM
     ROW(OLD.seq, OLD.tenant_id, OLD.event_id, OLD.envelope_version, OLD.schema_version, OLD.type, OLD.recorded_at,
         OLD.effective_at, OLD.actor_id, OLD.run_id, OLD.causation_id, OLD.correlation_id, OLD.entity_type, OLD.entity_id, OLD.context_id) THEN
    RAISE EXCEPTION 'purge may replace payload only (seq %)', OLD.seq USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER journal_guard_trg
  BEFORE UPDATE OR DELETE ON journal
  FOR EACH ROW EXECUTE FUNCTION journal_guard();

-- ---------------------------------------------------------------------------
-- journal_append · the single write path. Assigns seq under the head lock, stamps recorded_at,
-- returns (seq, recorded_at). The application calls this inside its transaction together with
-- the synchronous projector.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION journal_append(
  p_tenant_id      uuid,
  p_event_id       uuid,
  p_schema_version smallint,
  p_type           text,
  p_effective_at   timestamptz,
  p_actor_id       uuid,
  p_run_id         uuid,
  p_causation_id   uuid,
  p_correlation_id uuid,
  p_entity_type    text,
  p_entity_id      uuid,
  p_context_id     uuid,
  p_payload        jsonb
) RETURNS TABLE (seq bigint, recorded_at timestamptz) LANGUAGE plpgsql AS $$
DECLARE
  v_seq bigint;
  v_now timestamptz;
BEGIN
  UPDATE journal_head SET last_seq = journal_head.last_seq + 1
   WHERE journal_head.tenant_id = p_tenant_id
   RETURNING journal_head.last_seq INTO v_seq;
  IF v_seq IS NULL THEN
    RAISE EXCEPTION 'unknown tenant %', p_tenant_id USING ERRCODE = 'foreign_key_violation';
  END IF;
  v_now := clock_timestamp();
  INSERT INTO journal (seq, tenant_id, event_id, schema_version, type, recorded_at, effective_at,
                       actor_id, run_id, causation_id, correlation_id, entity_type, entity_id, context_id, payload)
  VALUES (v_seq, p_tenant_id, p_event_id, p_schema_version, p_type, v_now, COALESCE(p_effective_at, v_now),
          p_actor_id, p_run_id, p_causation_id, p_correlation_id, p_entity_type, p_entity_id, p_context_id, p_payload);
  RETURN QUERY SELECT v_seq, v_now;
END $$;

-- ---------------------------------------------------------------------------
-- journal_purge · the only UPDATE. Replaces payloads of the targeted entity's events with a marker
-- and appends a journal.purged event in the same transaction (the purge itself is history).
-- SECURITY DEFINER so litopys_app cannot do it directly; granted to litopys_admin only.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION journal_purge(
  p_tenant_id      uuid,
  p_entity_type    text,
  p_entity_id      uuid,
  p_reason         text,
  p_legal_basis    text,
  p_actor_id       uuid,
  p_run_id         uuid,
  p_causation_id   uuid,
  p_correlation_id uuid
) RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
  v_purge_event uuid := gen_random_uuid();
  v_ids uuid[];
  v_seq bigint;
BEGIN
  SELECT array_agg(event_id ORDER BY seq) INTO v_ids
    FROM journal
   WHERE tenant_id = p_tenant_id AND entity_type = p_entity_type AND entity_id = p_entity_id
     AND type <> 'journal.purged';

  PERFORM set_config('litopys.purge', 'on', true);
  UPDATE journal
     SET payload = jsonb_build_object('purged', true, 'purge_event_id', v_purge_event)
   WHERE tenant_id = p_tenant_id AND entity_type = p_entity_type AND entity_id = p_entity_id
     AND type <> 'journal.purged';
  PERFORM set_config('litopys.purge', 'off', true);

  SELECT j.seq INTO v_seq FROM journal_append(
    p_tenant_id, v_purge_event, 1::smallint, 'journal.purged', NULL,
    p_actor_id, p_run_id, p_causation_id, p_correlation_id,
    p_entity_type, p_entity_id, NULL,
    jsonb_build_object(
      'target', jsonb_build_object('entity_type', p_entity_type, 'entity_id', p_entity_id),
      'reason', p_reason, 'legal_basis', p_legal_basis,
      'purged_event_ids', to_jsonb(COALESCE(v_ids, ARRAY[]::uuid[]))
    )
  ) j;
  RETURN v_seq;
END $$;

-- ---------------------------------------------------------------------------
-- grants
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT ON journal TO litopys_app;
GRANT SELECT, UPDATE ON journal_head TO litopys_app;
GRANT SELECT ON tenant TO litopys_app;
GRANT EXECUTE ON FUNCTION journal_append(uuid, uuid, smallint, text, timestamptz, uuid, uuid, uuid, uuid, text, uuid, uuid, jsonb) TO litopys_app;
REVOKE EXECUTE ON FUNCTION journal_purge(uuid, text, uuid, text, text, uuid, uuid, uuid, uuid) FROM PUBLIC;
GRANT  EXECUTE ON FUNCTION journal_purge(uuid, text, uuid, text, text, uuid, uuid, uuid, uuid) TO litopys_admin;
GRANT litopys_app TO litopys_admin;

COMMIT;
