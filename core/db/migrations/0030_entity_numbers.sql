-- Litopys · 0030 · human numbers of entities in the domain (#135)
-- A record is R-<num> within its owner, a workflow W-<num> and an execution X-<num> within the project (tasks have
-- num since 0005). The number is issued by the command and travels in the creation event (statement.asserted,
-- workflow.declared, execution.started), so replay gives the same numbers; it is never reused — rows of retracted
-- and purged entities stay. Events written before this migration carry no num: the projector takes a record's
-- number from facade.legacy_id (the numbers people already saw, 0007; replay and purge never touch that table) and
-- numbers workflows and executions in seq order. Purge keeps num: it is structure, not content.

BEGIN;

-- every record gets its facade number first (the logic of facade.ensureIDs): imported ones their source id,
-- the rest after the maximum in assertion order
INSERT INTO facade.legacy_id (statement_id, tenant_id, legacy_id)
SELECT s.statement_id, s.tenant_id, (j.payload->'imported_from'->>'observation_id')::bigint
  FROM projection.statement s
  JOIN journal j ON j.tenant_id = s.tenant_id AND j.seq = s.asserted_seq
 WHERE j.payload ? 'imported_from'
   AND NOT EXISTS (SELECT 1 FROM facade.legacy_id l WHERE l.statement_id = s.statement_id)
ON CONFLICT DO NOTHING;
INSERT INTO facade.legacy_id (statement_id, tenant_id, legacy_id)
SELECT s.statement_id, s.tenant_id,
       (SELECT COALESCE(max(l.legacy_id), 0) FROM facade.legacy_id l WHERE l.tenant_id = s.tenant_id)
         + row_number() OVER (PARTITION BY s.tenant_id ORDER BY s.asserted_seq)
  FROM projection.statement s
 WHERE NOT EXISTS (SELECT 1 FROM facade.legacy_id l WHERE l.statement_id = s.statement_id);

ALTER TABLE projection.statement ADD COLUMN num bigint;
UPDATE projection.statement s SET num = l.legacy_id FROM facade.legacy_id l WHERE l.statement_id = s.statement_id;
ALTER TABLE projection.statement ALTER COLUMN num SET NOT NULL;
CREATE UNIQUE INDEX statement_owner_num_idx ON projection.statement (owner_id, num);

ALTER TABLE projection.workflow ADD COLUMN num int;
UPDATE projection.workflow w SET num = n.num
  FROM (SELECT workflow_id, row_number() OVER (PARTITION BY project_id ORDER BY declared_seq) AS num
          FROM projection.workflow) n
 WHERE n.workflow_id = w.workflow_id;
ALTER TABLE projection.workflow ALTER COLUMN num SET NOT NULL;
CREATE UNIQUE INDEX workflow_project_num_idx ON projection.workflow (project_id, num);

ALTER TABLE projection.execution ADD COLUMN num int;
UPDATE projection.execution x SET num = n.num
  FROM (SELECT execution_id, row_number() OVER (PARTITION BY project_id ORDER BY started_seq) AS num
          FROM projection.execution) n
 WHERE n.execution_id = x.execution_id;
ALTER TABLE projection.execution ALTER COLUMN num SET NOT NULL;
CREATE UNIQUE INDEX execution_project_num_idx ON projection.execution (project_id, num);

-- purge keeps the number (structure): 0004 plus 'num'
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

-- verify (0018): an imported record must keep its source id as its own number (R-<source id>), not only in the facade
CREATE OR REPLACE FUNCTION ops.verify_report(expected_version text DEFAULT NULL)
RETURNS TABLE (tenant text, journal_head bigint, journal_rows bigint, journal_max bigint, projector_seq bigint,
               projector_version text, orphan_statements bigint, renumbered_imports bigint, findings text[])
LANGUAGE sql STABLE SET search_path = public, pg_temp AS $$
  SELECT r.name, r.head, r.cnt, r.mx, r.applied, r.version, r.orphans, r.renumbered,
         array_remove(ARRAY[
           CASE WHEN NOT (r.cnt = r.head AND r.mx = r.head) THEN 'journal is not gapless (count = max(seq) = head)' END,
           CASE WHEN r.applied <> r.head THEN 'projector lags behind the journal — litopys projector rebuild' END,
           CASE WHEN expected_version IS NOT NULL AND r.version <> expected_version
                THEN 'projections were built by another projector version — litopys projector rebuild' END,
           CASE WHEN r.orphans <> 0 THEN 'statements without their asserted event' END,
           -- an imported observation keeps its source id as R-<num>: docs, todos and cursors cite it
           CASE WHEN r.renumbered <> 0 THEN r.renumbered || ' imported statements lost their source id to earlier native writes' END
         ], NULL)
    FROM (
      SELECT t.name, h.last_seq AS head,
             (SELECT count(*) FROM journal j WHERE j.tenant_id = t.tenant_id) AS cnt,
             (SELECT COALESCE(max(seq), 0) FROM journal j WHERE j.tenant_id = t.tenant_id) AS mx,
             COALESCE(ps.last_seq, 0) AS applied, COALESCE(ps.version, '-') AS version,
             (SELECT count(*) FROM projection.statement s WHERE s.tenant_id = t.tenant_id
                AND NOT EXISTS (SELECT 1 FROM journal j WHERE j.event_id = s.asserted_event_id)) AS orphans,
             (SELECT count(*) FROM projection.statement s
                JOIN journal j ON j.tenant_id = s.tenant_id AND j.seq = s.asserted_seq
               WHERE s.tenant_id = t.tenant_id AND j.payload ? 'imported_from'
                 AND s.num <> (j.payload->'imported_from'->>'observation_id')::bigint) AS renumbered
        FROM tenant t JOIN journal_head h ON h.tenant_id = t.tenant_id
        LEFT JOIN projection.projector_state ps ON ps.tenant_id = t.tenant_id
    ) r
   ORDER BY r.name
$$;

COMMIT;
