-- Litopys · 0012 · typed links between projects (todo #43: project_link / project_links of contract 1.19)
-- A project link is an ordinary link (link.asserted / link.retired) whose ends are two projects of the same
-- owner; the relations depends_on, predecessor_of, parallel_line_of are reserved for project ends (core/verbs).

BEGIN;

ALTER TABLE projection.link DROP CONSTRAINT link_relation_check;
ALTER TABLE projection.link ADD CONSTRAINT link_relation_check CHECK (relation IN
  ('related','contradicts','duplicates','confirms','about','derived_from','depends_on','predecessor_of','parallel_line_of'));
CREATE INDEX link_project_idx ON projection.link (to_id) WHERE to_type = 'project' AND NOT retired;

COMMIT;
