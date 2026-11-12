package ops

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/verbs"
)

// Maintenance is the read-only mode of 0042: while it is on, every command refuses with code maintenance.
type Maintenance struct {
	ReadOnly bool       `json:"read_only"`
	Reason   *string    `json:"reason,omitempty"`
	Since    *time.Time `json:"since,omitempty"`
}

// GetMaintenance reads the mode.
func GetMaintenance(ctx context.Context, db interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) (Maintenance, error) {
	var m Maintenance
	err := db.QueryRow(ctx, `SELECT read_only, reason, since FROM ops.maintenance`).Scan(&m.ReadOnly, &m.Reason, &m.Since)
	return m, err
}

// SetMaintenance switches the mode; the reason is shown to every refused write. Idempotent.
func SetMaintenance(ctx context.Context, db *pgx.Conn, readOnly bool, reason string) error {
	var r *string
	if reason != "" {
		r = &reason
	}
	_, err := db.Exec(ctx, `UPDATE ops.maintenance SET read_only = $1, reason = $2,
	        since = CASE WHEN $1 THEN COALESCE(CASE WHEN read_only THEN since END, now()) END`, readOnly, r)
	return err
}

// Drain waits until every command that started before the mode was switched on has finished: it takes the
// maintenance lock exclusive (each command holds it shared for its transaction, verbs.MaintenanceLock) and lets it go.
// Commands that start later see the mode and refuse. Run it after SetMaintenance on (committed).
func Drain(ctx context.Context, db *pgx.Conn, timeout time.Duration) error {
	if _, err := db.Exec(ctx, fmt.Sprintf(`SET lock_timeout = '%dms'`, timeout.Milliseconds())); err != nil {
		return err
	}
	defer db.Exec(ctx, `RESET lock_timeout`) //nolint:errcheck
	if _, err := db.Exec(ctx, `SELECT pg_advisory_lock($1)`, verbs.MaintenanceLock); err != nil {
		return fmt.Errorf("drain: commands still running after %s: %w", timeout, err)
	}
	_, err := db.Exec(ctx, `SELECT pg_advisory_unlock($1)`, verbs.MaintenanceLock)
	return err
}
