-- Litopys · 0049 · attachments (#246 П7): a file a person or a model adds to a project — a contract scan, a
-- photo, an estimate. The bytes live in the app's data directory (LITOPYS_FILES_DIR) under their SHA-256, never in
-- the journal; the journal and this projection hold what describes them. A record points to one by files: att:<sha>.

BEGIN;

CREATE TABLE projection.attachment (
  attachment_id uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL,
  project_id    uuid NOT NULL REFERENCES projection.project(project_id),
  owner_id      uuid NOT NULL,
  sha256        text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
  mime          text NOT NULL CHECK (length(mime) BETWEEN 1 AND 100),
  size          bigint NOT NULL CHECK (size >= 0),
  added_by      uuid NOT NULL,
  added_at      timestamptz NOT NULL,
  last_seq      bigint NOT NULL,
  last_event_id uuid NOT NULL,
  UNIQUE (project_id, sha256)
);
CREATE INDEX attachment_sha_idx ON projection.attachment (tenant_id, sha256);

GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON projection.attachment TO litopys_app;

ALTER TABLE projection.attachment ENABLE ROW LEVEL SECURITY;
CREATE POLICY owner_read ON projection.attachment FOR SELECT TO litopys_app
  USING (owner_id = litopys_owner() OR litopys_scope() IN ('projector','system'));
CREATE POLICY projector_insert ON projection.attachment FOR INSERT TO litopys_app
  WITH CHECK (litopys_scope() = 'projector');
CREATE POLICY projector_delete ON projection.attachment FOR DELETE TO litopys_app
  USING (litopys_scope() = 'projector');

COMMIT;
