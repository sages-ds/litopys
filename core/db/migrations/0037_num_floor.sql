-- Litopys · 0037 · number floors (#193; owner's decision #183 a): a migration reserves the numbers
-- the source has already given out, so an entity erased at the top of the source sequence does not lend its number to a
-- new one — old texts with legacy record ids / "todo #N" keep pointing where they did. The event numbering.reserved carries the
-- floor: R per owner (records), T per project (tasks); a command issues GREATEST(max(num), floor) + 1.

BEGIN;

CREATE TABLE projection.num_floor (
  tenant_id     uuid   NOT NULL,
  owner_id      uuid   NOT NULL,
  scope_id      uuid   NOT NULL,                      -- the owner (R) or the project (T)
  kind          text   NOT NULL CHECK (kind IN ('R','T')),
  floor         bigint NOT NULL CHECK (floor > 0),    -- the highest number already given out by the source
  last_seq      bigint NOT NULL,
  last_event_id uuid   NOT NULL,
  PRIMARY KEY (scope_id, kind)
);

GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON projection.num_floor TO litopys_app;

ALTER TABLE projection.num_floor ENABLE ROW LEVEL SECURITY;
CREATE POLICY owner_read ON projection.num_floor FOR SELECT TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'));
CREATE POLICY projector_insert ON projection.num_floor FOR INSERT TO litopys_app
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_update ON projection.num_floor FOR UPDATE TO litopys_app
  USING (litopys_scope() = 'projector')
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_delete ON projection.num_floor FOR DELETE TO litopys_app
  USING (litopys_scope() = 'projector');

COMMIT;
