package verbs

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// MaintenanceLock is the advisory lock of the read-only mode (0042): every command holds it shared for its
// transaction; the drain takes it exclusive, so it returns only when every command that started before the switch
// has committed or rolled back — with no privilege to read other sessions.
const MaintenanceLock int64 = 0x4c49544f50590001 // "LITOPY" + 1

// writable refuses a command while Litopys is read-only for maintenance (0042): `litopys update` drains writes
// before it snapshots the data. The shared lock comes first, then the flag in the same transaction: a command that
// locked before the switch is waited for by the drain, one that locks after it sees the flag and writes nothing.
func writable(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, MaintenanceLock); err != nil {
		return err
	}
	var readOnly bool
	var reason *string
	if err := tx.QueryRow(ctx, `SELECT read_only, reason FROM ops.maintenance`).Scan(&readOnly, &reason); err != nil {
		return err
	}
	if !readOnly {
		return nil
	}
	why := "maintenance"
	if reason != nil && *reason != "" {
		why = *reason
	}
	return fail("maintenance", "Litopys is read-only for a moment (%s): reading works, retry the write in a minute", why)
}

// Writable is the same guard for writes outside the commands (token revocation, raw events): call it first in their
// transaction.
func Writable(ctx context.Context, tx pgx.Tx) error { return writable(ctx, tx) }
