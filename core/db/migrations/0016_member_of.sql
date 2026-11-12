-- Litopys · 0016 · the relation member_of: a project belongs to a space (the layer "projects").
-- A space is an ordinary project labelled "space"; the owner's global layer — a project labelled
-- "global". Only the CHECK changes: member_of links are ordinary links (link.asserted / link.retired).

BEGIN;

ALTER TABLE projection.link DROP CONSTRAINT link_relation_check;
ALTER TABLE projection.link ADD CONSTRAINT link_relation_check CHECK (relation IN
  ('related','contradicts','duplicates','confirms','about','derived_from','depends_on','predecessor_of','parallel_line_of','member_of'));

COMMIT;
