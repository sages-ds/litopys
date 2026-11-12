-- Litopys · 0033 · purge of a task (#136 step 2)
-- Until now journal.purge of a task erased the journal but not projection.task (the projector purged statements
-- only), and replay of the purged markers could not rebuild the row: title (NOT NULL) and the outcome of
-- task.closed were gone. The marker of a task now also keeps its structure — status, outcome, kind, priority,
-- rank, role, due, parent, severity, owner_project, data_class; content (title, body, acceptance, resolution,
-- labels, refs, owner, assignee, reasons) is erased. The keys are chosen by entity type: an outcome record's
-- 'outcome' is content and stays erased.
--
-- projection.task gets purged and hidden = retracted OR purged: every read and verb that skipped retracted tasks
-- skips hidden ones (the row stays, so the number is never reused). Projector v19 fills purged on replay.

BEGIN;

ALTER TABLE projection.task ADD COLUMN purged boolean NOT NULL DEFAULT false;
ALTER TABLE projection.task ADD COLUMN hidden boolean GENERATED ALWAYS AS (retracted OR purged) STORED;
DROP INDEX projection.task_open_idx;
CREATE INDEX task_open_idx ON projection.task (project_id, status) WHERE open AND NOT hidden;

CREATE OR REPLACE FUNCTION journal_purge(
  p_tenant_id      uuid,
  p_entity_type    text,
  p_entity_id      uuid,
  p_reason         text,
  p_legal_basis    text,
  p_actor_id       uuid,
  p_run_id         uuid,
  p_causation_id   uuid,
  p_correlation_id uuid
) RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
  v_purge_event uuid := gen_random_uuid();
  v_ids uuid[];
  v_seq bigint;
BEGIN
  SELECT array_agg(event_id ORDER BY seq) INTO v_ids
    FROM journal
   WHERE tenant_id = p_tenant_id AND entity_type = p_entity_type AND entity_id = p_entity_id
     AND type <> 'journal.purged';

  PERFORM set_config('litopys.purge', 'on', true);
  UPDATE journal
     SET payload = jsonb_build_object('purged', true, 'purge_event_id', v_purge_event)
                   || jsonb_strip_nulls(jsonb_build_object(
                        'project_id',    payload->'project_id',
                        'type',          payload->'type',
                        'superseded_by', payload->'superseded_by',
                        'part',          payload->'part',
                        'num',           payload->'num'))
                   || CASE WHEN p_entity_type = 'task' THEN jsonb_strip_nulls(jsonb_build_object(
                        'status',        payload->'status',
                        'outcome',       payload->'outcome',
                        'kind',          payload->'kind',
                        'priority',      payload->'priority',
                        'rank',          payload->'rank',
                        'role',          payload->'role',
                        'due',           payload->'due',
                        'parent',        payload->'parent',
                        'severity',      payload->'severity',
                        'owner_project', payload->'owner_project',
                        'data_class',    payload->'data_class'))
                      ELSE '{}'::jsonb END
   WHERE tenant_id = p_tenant_id AND entity_type = p_entity_type AND entity_id = p_entity_id
     AND type <> 'journal.purged';
  PERFORM set_config('litopys.purge', 'off', true);

  SELECT j.seq INTO v_seq FROM journal_append(
    p_tenant_id, v_purge_event, 1::smallint, 'journal.purged', NULL,
    p_actor_id, p_run_id, p_causation_id, p_correlation_id,
    p_entity_type, p_entity_id, NULL,
    jsonb_build_object(
      'target', jsonb_build_object('entity_type', p_entity_type, 'entity_id', p_entity_id),
      'reason', p_reason, 'legal_basis', p_legal_basis,
      'purged_event_ids', to_jsonb(COALESCE(v_ids, ARRAY[]::uuid[]))
    )
  ) j;
  RETURN v_seq;
END $$;

REVOKE EXECUTE ON FUNCTION journal_purge(uuid, text, uuid, text, text, uuid, uuid, uuid, uuid) FROM PUBLIC;
GRANT  EXECUTE ON FUNCTION journal_purge(uuid, text, uuid, text, text, uuid, uuid, uuid, uuid) TO litopys_admin;

COMMIT;
