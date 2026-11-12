package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/ops"
)

// runMaintenance: `litopys maintenance status | on [--reason R] [--drain D] | off` — the read-only mode of 0042.
// on refuses every command with code maintenance (reads go on); --drain then waits until no write transaction is
// open (the step before a snapshot in `litopys update`). Connects with the PG* environment of the operator's role
// (the owner of the schema ops), never the app's.
func runMaintenance(ctx context.Context, args []string) error {
	usage := errors.New("maintenance: usage: litopys maintenance status | on [--reason <text>] [--drain <duration>] | off")
	if len(args) == 0 {
		return usage
	}
	sub, reason, drain := args[0], "", time.Duration(0)
	for i := 1; i < len(args); i++ {
		if i+1 >= len(args) {
			return usage
		}
		switch args[i] {
		case "--reason":
			reason = args[i+1]
		case "--drain":
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return fmt.Errorf("maintenance: --drain: %w", err)
			}
			drain = d
		default:
			return usage
		}
		i++
	}
	db, err := pgx.Connect(ctx, "") // PG* environment
	if err != nil {
		return err
	}
	defer db.Close(ctx)
	switch sub {
	case "status":
	case "on":
		if err := ops.SetMaintenance(ctx, db, true, reason); err != nil {
			return err
		}
		if drain > 0 {
			if err := ops.Drain(ctx, db, drain); err != nil {
				return err
			}
			fmt.Println("drained: no write transaction open")
		}
	case "off":
		if err := ops.SetMaintenance(ctx, db, false, ""); err != nil {
			return err
		}
	default:
		return usage
	}
	m, err := ops.GetMaintenance(ctx, db)
	if err != nil {
		return err
	}
	if !m.ReadOnly {
		fmt.Println("maintenance: off (writes accepted)")
		return nil
	}
	why := ""
	if m.Reason != nil {
		why = " — " + *m.Reason
	}
	fmt.Printf("maintenance: read-only since %s%s\n", m.Since.UTC().Format(time.RFC3339), why)
	return nil
}
