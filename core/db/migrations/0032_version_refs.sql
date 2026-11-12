-- Litopys · 0032 · refs in every revision of a record (#142)
-- statement_version keeps the refs list each revision had, so recall_read history shows what an amend changed in
-- them. Filled by the projector (v18, replay); purge clears them with the rest of the content.

BEGIN;

ALTER TABLE projection.statement_version ADD COLUMN refs text[] NOT NULL DEFAULT '{}';

COMMIT;
