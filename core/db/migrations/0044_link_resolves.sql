-- Litopys · 0044 · link type resolves (roast r1, #244 С8): an answer resolves a question or a removed risk; an answered
-- question leaves the open ones (work_open_items, topic_resume) and keeps its history.

ALTER TABLE projection.link DROP CONSTRAINT link_relation_check;
ALTER TABLE projection.link ADD CONSTRAINT link_relation_check CHECK (relation IN
  ('related','contradicts','duplicates','confirms','about','derived_from','depends_on','predecessor_of','parallel_line_of','member_of',
   'handoff','mentions','resolves'));

-- is this question answered: by the target, live links only
CREATE INDEX link_resolves_to_idx ON projection.link (to_id) WHERE relation = 'resolves' AND NOT retired;
