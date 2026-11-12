-- Litopys · 0042 · read-only mode for maintenance (#163): `litopys update` drains writes before
-- it snapshots the data and migrates — every command refuses with code maintenance while ops.maintenance.read_only is
-- set, reads go on. One row; set by the operator's role (the owner of the schema), read by the app and the worker.

BEGIN;

CREATE TABLE ops.maintenance (
  id        boolean PRIMARY KEY DEFAULT true CHECK (id),
  read_only boolean NOT NULL DEFAULT false,
  reason    text,
  since     timestamptz
);
INSERT INTO ops.maintenance DEFAULT VALUES;

GRANT SELECT ON ops.maintenance TO litopys_app;
GRANT USAGE ON SCHEMA ops TO litopys_admin;
GRANT SELECT ON ops.maintenance TO litopys_admin;

COMMIT;
