-- Litopys · 0023 · data classes (decision of the owner 2026-10-16)
-- public < internal < local: public may go anywhere (free cloud tiers that train on data included), internal only to
-- cloud executors without training or retention, local only to local executors.
--   project.data_class   — the class of everything in the project; NOT NULL, default internal (existing ones too);
--   statement / task     — NULL = the project's class; a value can only raise it (the effective class is the higher).
-- A grant with max_data_class (a derived grant of an executor, 0022) sees nothing above its class: the policy layer
-- hides projects above it (litopys.projects, 0010) and the RLS below hides raised records through litopys.max_class
-- (internal/pg). Without the setting nothing is hidden, so every other consumer reads as before.

BEGIN;

CREATE FUNCTION litopys_class_rank(c text) RETURNS int LANGUAGE sql IMMUTABLE AS
  $$ SELECT CASE c WHEN 'local' THEN 2 WHEN 'internal' THEN 1 ELSE 0 END $$;

CREATE FUNCTION litopys_class_ok(c text) RETURNS boolean LANGUAGE sql STABLE AS
  $$ SELECT COALESCE(current_setting('litopys.max_class', true), '') = ''
            OR litopys_class_rank(c) <= litopys_class_rank(current_setting('litopys.max_class', true)) $$;

GRANT EXECUTE ON FUNCTION litopys_class_rank(text), litopys_class_ok(text) TO litopys_app;

ALTER TABLE projection.project ADD COLUMN data_class text NOT NULL DEFAULT 'internal'
  CHECK (data_class IN ('public', 'internal', 'local'));
ALTER TABLE projection.statement ADD COLUMN data_class text CHECK (data_class IN ('public', 'internal', 'local'));
ALTER TABLE projection.task ADD COLUMN data_class text CHECK (data_class IN ('public', 'internal', 'local'));

-- the read policies of 0010 plus the class of the row (the project's class is decided by the policy layer)
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['project', 'task', 'statement'] LOOP
    EXECUTE format('DROP POLICY owner_read ON projection.%I', t);
    EXECUTE format($p$CREATE POLICY owner_read ON projection.%I FOR SELECT TO litopys_app
                      USING ((owner_id = litopys_owner() AND litopys_project_visible(project_id) AND litopys_class_ok(data_class))
                             OR litopys_scope() IN ('projector', 'system'))$p$, t);
  END LOOP;
END $$;

-- new columns are filled by events that carry data_class; existing events carry none, so no rebuild is needed

COMMIT;
