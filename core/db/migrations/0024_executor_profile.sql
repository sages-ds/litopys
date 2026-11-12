-- Litopys · 0024 · registry of executors (decision of the owner 2026-10-16)
-- An executor is an actor (agent / service) with a profile: provider, model, key kind, a reference to its key in the
-- secret store (never the key), limits, the provider's data policy, the highest data class it may receive, when the
-- terms were checked. actor.registered may carry it, actor.amended replaces it (null removes). step.delegate narrows
-- the derived grant to the executor's class and refuses a run whose project is above it (data_class_exceeded).
-- A new nullable column: existing events carry no profile, so no rebuild is needed.

BEGIN;

ALTER TABLE projection.actor ADD COLUMN executor jsonb;
ALTER TABLE projection.actor ADD CONSTRAINT actor_executor_class_check
  CHECK (executor IS NULL OR executor->>'max_data_class' IN ('public', 'internal', 'local'));

COMMIT;
