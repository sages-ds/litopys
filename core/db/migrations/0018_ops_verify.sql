-- Litopys · 0018 · ops.verify_report: the integrity checks of `litopys verify` as one function in the database
-- (todo #35). `litopys verify` and the scheduled backup of homelab-ansible (playbooks/litopys/backup.yml: dump →
-- restore into a scratch database → this function) run the same checks without a litopys binary on the host.
-- One row per tenant; findings is empty when the tenant is sound. expected_version is the projector version of
-- the caller's code (NULL: not checked — psql knows no code version). SECURITY INVOKER: the caller must see every
-- row (the system scope, BYPASSRLS of litopys_backup, or the owner), as for a dump.

BEGIN;

CREATE SCHEMA ops;

CREATE FUNCTION ops.verify_report(expected_version text DEFAULT NULL)
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
           -- an imported observation must keep its source id: docs, todos and cursors cite it
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
                JOIN facade.legacy_id l ON l.statement_id = s.statement_id
               WHERE s.tenant_id = t.tenant_id AND j.payload ? 'imported_from'
                 AND l.legacy_id <> (j.payload->'imported_from'->>'observation_id')::bigint) AS renumbered
        FROM tenant t JOIN journal_head h ON h.tenant_id = t.tenant_id
        LEFT JOIN projection.projector_state ps ON ps.tenant_id = t.tenant_id
    ) r
   ORDER BY r.name
$$;

-- litopys verify runs as litopys_app with the system scope; the backup login reads a restored copy it owns
GRANT USAGE ON SCHEMA ops TO litopys_app;
REVOKE EXECUTE ON FUNCTION ops.verify_report(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ops.verify_report(text) TO litopys_app;

COMMIT;
