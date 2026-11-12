-- Litopys · 0004 · purge keeps structural keys (found while writing gate E7, 2026-09-29)
-- 0001 replaced the WHOLE payload of a purged entity's events with a marker. Replay
-- then cannot rebuild the statement row: project_id (FK), type and superseded_by are gone.
-- Content is what must be erased (title, body, concepts, files, external_ref, reasons, notes);
-- structure is not content. The marker now carries the structural keys that were present.
-- Events purged before this migration keep the old marker (none exist yet on any stand).

BEGIN;

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
                        'part',          payload->'part'))
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

-- CREATE OR REPLACE keeps owner and grants; restated so the file is self-describing.
REVOKE EXECUTE ON FUNCTION journal_purge(uuid, text, uuid, text, text, uuid, uuid, uuid, uuid) FROM PUBLIC;
GRANT  EXECUTE ON FUNCTION journal_purge(uuid, text, uuid, text, text, uuid, uuid, uuid, uuid) TO litopys_admin;

COMMIT;
