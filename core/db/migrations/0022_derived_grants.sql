-- Litopys · 0022 · derived grants of executors (decisions of the owner 2026-10-16).
-- An orchestrator delegates one step of one run (step.delegate): Litopys issues a token narrower than the
-- orchestrator's own — the executor actor (agent / service on behalf of the person), the run and the step, the
-- step's verbs, the write mode and the data class — and it lives only while every ancestor token lives and the
-- run is open. Same table and same Verify as every token; revoking a token revokes its descendants.

BEGIN;

ALTER TABLE auth.token
  ADD COLUMN kind              text NOT NULL DEFAULT 'human' CHECK (kind IN ('human', 'derived')),
  ADD COLUMN parent_token_id   uuid REFERENCES auth.token(token_id),
  ADD COLUMN depth             int  NOT NULL DEFAULT 0 CHECK (depth BETWEEN 0 AND 3),
  ADD COLUMN executor_actor_id uuid,          -- the actor the derived token speaks as (on_behalf_of actor_id)
  ADD COLUMN execution_id      uuid,          -- projection.execution is derived: no FK
  ADD COLUMN step_key          text,
  ADD COLUMN verbs             text[],
  ADD COLUMN write_mode        text CHECK (write_mode IN ('direct', 'candidate')),
  ADD COLUMN max_data_class    text CHECK (max_data_class IN ('public', 'internal', 'local')),
  ADD CONSTRAINT token_derived_shape CHECK (
    kind = 'human' AND parent_token_id IS NULL AND depth = 0 AND executor_actor_id IS NULL AND execution_id IS NULL
    OR kind = 'derived' AND parent_token_id IS NOT NULL AND depth >= 1 AND executor_actor_id IS NOT NULL
       AND execution_id IS NOT NULL AND step_key IS NOT NULL AND verbs IS NOT NULL AND write_mode IS NOT NULL
       AND expires_at IS NOT NULL);
CREATE INDEX token_parent_idx ON auth.token (parent_token_id) WHERE parent_token_id IS NOT NULL;
CREATE INDEX token_execution_idx ON auth.token (execution_id) WHERE execution_id IS NOT NULL;

COMMIT;
