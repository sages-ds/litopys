-- Litopys · 0010 · policy as a computed layer (todo #10)
-- The policy layer (core/policy) decides which projects a grant sees; the database applies the decision:
-- the visible set travels in litopys.projects (internal/pg, like litopys.owner in 0008) and the RLS of the
-- project-scoped projections adds litopys_project_visible(project_id). Empty = the policy hides nothing.
-- Rules live in the journal (policy.set) and are projected into projection.policy (refilled by rebuild,
-- projector version 7). Grant attributes are edge data on auth.token (OAuth of #33 fills the same columns).

BEGIN;

CREATE FUNCTION litopys_projects() RETURNS uuid[] LANGUAGE sql STABLE AS
  $$ SELECT CASE WHEN COALESCE(current_setting('litopys.projects', true), '') = '' THEN NULL
                 ELSE current_setting('litopys.projects', true)::uuid[] END $$;

CREATE FUNCTION litopys_project_visible(p uuid) RETURNS boolean LANGUAGE sql STABLE AS
  $$ SELECT litopys_projects() IS NULL OR p = ANY(litopys_projects()) $$;

GRANT EXECUTE ON FUNCTION litopys_projects(), litopys_project_visible(uuid) TO litopys_app;

-- ---------------------------------------------------------------------------
-- rules (policy.set) and project labels
-- ---------------------------------------------------------------------------
CREATE TABLE projection.policy (
  policy_id    uuid PRIMARY KEY,                 -- UUIDv5 of (tenant, scope, owner-or-nil, name): a re-set replaces
  tenant_id    uuid NOT NULL,
  owner_id     uuid NOT NULL,                    -- root human of the setter (0008); for scope tenant: the admin
  scope        text NOT NULL CHECK (scope IN ('tenant', 'owner')),
  name         text NOT NULL,
  rule         jsonb NOT NULL,
  active       boolean NOT NULL,
  reason       text,
  actor_id     uuid NOT NULL,
  set_event_id uuid NOT NULL,
  set_seq      bigint NOT NULL
);
CREATE INDEX policy_tenant_idx ON projection.policy (tenant_id, scope) WHERE active;
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON projection.policy TO litopys_app;

ALTER TABLE projection.policy ENABLE ROW LEVEL SECURITY;
-- tenant rules apply to everyone, so everyone reads them; owner rules only their owner
CREATE POLICY owner_read ON projection.policy FOR SELECT TO litopys_app
  USING (scope = 'tenant' OR owner_id = litopys_owner() OR litopys_scope() IN ('projector', 'system'));
CREATE POLICY projector_insert ON projection.policy FOR INSERT TO litopys_app
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_update ON projection.policy FOR UPDATE TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector', 'system'))
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_delete ON projection.policy FOR DELETE TO litopys_app
  USING (litopys_scope() = 'projector');

ALTER TABLE projection.project ADD COLUMN labels text[] NOT NULL DEFAULT '{}';
CREATE INDEX project_labels_idx ON projection.project USING gin (labels);

-- ---------------------------------------------------------------------------
-- grant attributes (D5-A profiles, D9 project allowlist, vendor of the client)
-- ---------------------------------------------------------------------------
ALTER TABLE auth.token
  ADD COLUMN profile text NOT NULL DEFAULT 'full' CHECK (profile IN ('full', 'core', 'chat', 'readonly')),
  ADD COLUMN project_allowlist text[],         -- NULL = every project of the owner; '{}' = none
  ADD COLUMN vendor text CHECK (vendor IN ('anthropic', 'openai', 'google', 'local', 'unknown'));

-- ---------------------------------------------------------------------------
-- visibility of projects in the RLS of 0008 (read policies of the project-scoped tables)
-- ---------------------------------------------------------------------------
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['project', 'task', 'fact', 'statement', 'context'] LOOP
    EXECUTE format('DROP POLICY owner_read ON projection.%I', t);
    EXECUTE format($p$CREATE POLICY owner_read ON projection.%I FOR SELECT TO litopys_app
                      USING ((owner_id = litopys_owner() AND litopys_project_visible(project_id))
                             OR litopys_scope() IN ('projector', 'system'))$p$, t);
  END LOOP;
END $$;

-- projections are derived: the rebuild of projector version 7 fills policy and labels
TRUNCATE projection.projector_state;

COMMIT;
