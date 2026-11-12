-- Litopys · 0008 · people inside a tenant (todo #31)
-- Tenant = organisation; every projection row gets owner_id = the root human of its author
-- (COALESCE(on_behalf_of, actor_id)); a project is unique per (tenant, owner, name).
-- Row-level security for litopys_app is the safety net under the explicit owner filters of the code:
--   read  — own rows (litopys.owner), or scope projector/system; actors also for scope admin;
--   write — only inside projector.Apply (litopys.scope = 'projector'); a command may still lock its own rows;
--   journal — events of my actors (or scope projector/system; registration events for scope admin).
-- The owner of the tables (litopys_migrator) is not subject to RLS: migrations, purge (SECURITY DEFINER).
-- TRUNCATE is not covered by RLS — only Rebuild truncates.
--
-- Projections are derived: they are emptied here and refilled by `litopys projector rebuild`
-- (projector version 6), so owners come from the same code as on the live path (gate E7).
-- The journal is untouched.

BEGIN;

-- ---------------------------------------------------------------------------
-- who is asking (set per connection by internal/pg.PrepareConn; per tx by projector.Apply)
-- ---------------------------------------------------------------------------
CREATE FUNCTION litopys_owner() RETURNS uuid LANGUAGE sql STABLE AS
  $$ SELECT NULLIF(current_setting('litopys.owner', true), '')::uuid $$;

CREATE FUNCTION litopys_scope() RETURNS text LANGUAGE sql STABLE AS
  $$ SELECT COALESCE(current_setting('litopys.scope', true), '') $$;

-- Root human of an actor; NULL when the actor is not projected (yet). SECURITY DEFINER because
-- projection.actor itself is under RLS and the edge needs the root before it knows the owner.
CREATE FUNCTION litopys_root(p_actor uuid) RETURNS uuid
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS
  $$ SELECT COALESCE(on_behalf_of, actor_id) FROM projection.actor WHERE actor_id = p_actor $$;

REVOKE EXECUTE ON FUNCTION litopys_root(uuid) FROM PUBLIC;
GRANT  EXECUTE ON FUNCTION litopys_owner(), litopys_scope(), litopys_root(uuid) TO litopys_app;

-- ---------------------------------------------------------------------------
-- owners
-- ---------------------------------------------------------------------------
TRUNCATE projection.search_vocab, projection.statement_lexical, projection.statement_version,
         projection.link, projection.statement, projection.task, projection.fact, projection.context,
         projection.run, projection.actor, projection.consumer, projection.project, projection.projector_state;

ALTER TABLE projection.actor             ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.run               ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.context           ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.project           ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.task              ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.fact              ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.statement         ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.statement_version ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.statement_lexical ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.link              ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.search_vocab      ADD COLUMN owner_id uuid NOT NULL;
ALTER TABLE projection.consumer          ADD COLUMN owner_id uuid NOT NULL;

-- a project name is unique per owner: two people may both have "homelab"
ALTER TABLE projection.project DROP CONSTRAINT project_tenant_id_name_key;
ALTER TABLE projection.project ADD CONSTRAINT project_owner_name_key UNIQUE (tenant_id, owner_id, name);

-- the dictionary of word forms and consumer cursors are per person
ALTER TABLE projection.search_vocab DROP CONSTRAINT search_vocab_pkey;
ALTER TABLE projection.search_vocab ADD PRIMARY KEY (tenant_id, owner_id, word);
ALTER TABLE projection.consumer DROP CONSTRAINT consumer_pkey;
ALTER TABLE projection.consumer ADD PRIMARY KEY (tenant_id, owner_id, consumer);

CREATE INDEX statement_owner_idx ON projection.statement (tenant_id, owner_id, effective_at DESC);
CREATE INDEX actor_owner_idx     ON projection.actor (owner_id);

-- ---------------------------------------------------------------------------
-- row-level security
-- ---------------------------------------------------------------------------
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['run','context','project','task','fact','statement','statement_version',
                           'statement_lexical','link','search_vocab','consumer'] LOOP
    EXECUTE format('ALTER TABLE projection.%I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format($p$CREATE POLICY owner_read ON projection.%I FOR SELECT TO litopys_app
                      USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'))$p$, t);
    EXECUTE format($p$CREATE POLICY projector_insert ON projection.%I FOR INSERT TO litopys_app
                      WITH CHECK (litopys_scope() = 'projector')$p$, t);
    -- USING lets a command lock its own rows (SELECT ... FOR UPDATE checks UPDATE policies too);
    -- WITH CHECK keeps the change itself to the projector
    EXECUTE format($p$CREATE POLICY projector_update ON projection.%I FOR UPDATE TO litopys_app
                      USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'))
                      WITH CHECK (litopys_scope() = 'projector')$p$, t);
    EXECUTE format($p$CREATE POLICY projector_delete ON projection.%I FOR DELETE TO litopys_app
                      USING (litopys_scope() = 'projector')$p$, t);
  END LOOP;
END $$;

ALTER TABLE projection.actor ENABLE ROW LEVEL SECURITY;
CREATE POLICY owner_read ON projection.actor FOR SELECT TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system','admin'));
CREATE POLICY projector_insert ON projection.actor FOR INSERT TO litopys_app
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_update ON projection.actor FOR UPDATE TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'))
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_delete ON projection.actor FOR DELETE TO litopys_app
  USING (litopys_scope() = 'projector');

-- journal: an event is mine when its actor's root is me. A self-registering human is not projected
-- yet while its actor.registered is appended, hence COALESCE(root, actor_id).
ALTER TABLE journal ENABLE ROW LEVEL SECURITY;
CREATE POLICY owner_read ON journal FOR SELECT TO litopys_app
  USING (COALESCE(litopys_root(actor_id), actor_id) = litopys_owner()
         OR litopys_scope() IN ('projector','system')
         OR (litopys_scope() = 'admin' AND type = 'actor.registered'));
CREATE POLICY append ON journal FOR INSERT TO litopys_app WITH CHECK (true);

COMMIT;
