-- Litopys · 0031 · explicit refs and the mentions link (#135)
-- A record or a task names entities in its refs: an internal ref (R-N, <project>/T-, W-, X-N, an id) becomes a
-- link "mentions" from the author to the target in the same command; any other ref (jira:ABC-1, pr:<url>) stays a
-- string. statement.refs keeps the list as given (tasks have refs since 0005). Refs are content: purge clears them,
-- the links stay (structure).

BEGIN;

ALTER TABLE projection.statement ADD COLUMN refs text[] NOT NULL DEFAULT '{}';

ALTER TABLE projection.link DROP CONSTRAINT link_relation_check;
ALTER TABLE projection.link ADD CONSTRAINT link_relation_check CHECK (relation IN
  ('related','contradicts','duplicates','confirms','about','derived_from','depends_on','predecessor_of','parallel_line_of','member_of',
   'handoff','mentions'));

-- who mentions an entity, newest first: task_resume.related, mentioned_by, search by number
CREATE INDEX link_mentions_to_idx ON projection.link (to_id, asserted_seq DESC) WHERE relation = 'mentions' AND NOT retired;

COMMIT;
