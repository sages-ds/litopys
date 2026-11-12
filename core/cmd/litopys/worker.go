package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"
	_ "time/tzdata" // Europe/Kyiv of the retention pass also in an image without zoneinfo

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/embed"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
)

// retentionRetry: the back-off of a failed retention pass (the purges of proposals keep their own interval).
const retentionRetry = time.Hour

// sweepEvery: how often idle sessions are looked for (a day of idleness needs no finer clock).
const sweepEvery = 10 * time.Minute

// embedBatch: records embedded per worker pass: small, so a search query is not held behind it.
const embedBatch = 4

// runWorker: `litopys worker [--interval 5s] [--once] [--retention-at 03:30] [--retention-now] [--session-idle 24h]`
// executes approved purges of deletion proposals (owner's decision P1, 2026-10-24): the admin role stays
// with this process, never with the public facade. The login must be allowed to SET ROLE litopys_admin, as for
// `litopys purge`. Once a day, at --retention-at Europe/Kyiv (or at start when that time has passed today), it applies
// retention labels; --retention-now forces that pass at start. Every sweepEvery it closes the sessions
// idle longer than --session-idle (0 turns the sweep off).
func runWorker(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	interval := fs.Duration("interval", 5*time.Second, "how often approved purges are looked for")
	once := fs.Bool("once", false, "one pass, then exit")
	at := fs.String("retention-at", "03:30", "daily time of the retention pass, HH:MM "+verbs.RetentionZone)
	now := fs.Bool("retention-now", false, "apply retention labels in the first pass, whatever the time")
	idle := fs.Duration("session-idle", verbs.SessionIdle, "close sessions without events for this long (0: never)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	daily, err := time.Parse("15:04", *at)
	if err != nil {
		return fmt.Errorf("worker: --retention-at %q: HH:MM", *at)
	}
	zone, err := time.LoadLocation(verbs.RetentionZone)
	if err != nil {
		return err
	}
	db, err := pg.Pool(ctx) // LITOPYS_SET_ROLE=litopys_app, as everywhere
	if err != nil {
		return err
	}
	defer db.Close()
	admin, err := rolePool(ctx, "litopys_admin")
	if err != nil {
		return err
	}
	defer admin.Close()
	var tenant uuid.UUID
	if err := db.QueryRow(pg.WithScope(ctx, pg.ScopeSystem), `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		return err
	}
	s := &verbs.Service{DB: db, Admin: admin}
	lastDay := ""         // the day of the last retention pass; a restart repeats it, which is harmless (nothing left to do)
	var retryAt time.Time // after a failed retention pass: not before this, so a lasting error is logged once an hour
	var sweptAt time.Time // the last idle-session sweep
	paused := false       // read-only for maintenance (0042): logged once, not every pass
	ec, err := embed.FromEnv()
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	embedErr := ""            // the last embeddings error logged
	var scheduledAt time.Time // the last minute the schedule triggers were looked at
	for {
		var errs []error
		n, err := s.ExecutePurges(ctx, tenant)
		if n > 0 {
			log.Printf("worker: %d approved purge(s) executed", n)
		}
		errs = append(errs, err)
		if *idle > 0 && time.Since(sweptAt) >= sweepEvery {
			sweptAt = time.Now()
			n, err := s.SweepIdleSessions(ctx, tenant, sweptAt, *idle)
			if n > 0 {
				log.Printf("worker: %d idle session(s) auto-closed", n)
			}
			errs = append(errs, err)
		}
		t := time.Now().In(zone)
		today := t.Format(time.DateOnly)
		due := time.Date(t.Year(), t.Month(), t.Day(), daily.Hour(), daily.Minute(), 0, 0, zone)
		if *now || (lastDay != today && !t.Before(due) && !t.Before(retryAt)) {
			*now = false
			n, err := s.ExecuteRetention(ctx, tenant, time.Now())
			if n > 0 {
				log.Printf("worker: retention retracted or erased %d entit(ies)", n)
			}
			if err == nil {
				lastDay = today
			} else {
				retryAt = t.Add(retentionRetry)
			}
			errs = append(errs, err)
		}
		// vectors of new and changed records for meaning search; a model that is down is logged when that
		// starts or changes, not every pass, and never blocks the purges and sweeps above. Small batches: a search query
		// shares the model and must not wait behind a long batch (on the pilot 16 texts kept it past its 5 s, 2026-11-01)
		if ec != nil {
			n, err := ec.Pass(pg.WithScope(ctx, pg.ScopeSystem), db, tenant, embedBatch)
			if n > 0 {
				log.Printf("worker: %d record(s) embedded (%s)", n, ec.Model)
			}
			if msg := fmt.Sprint(err); err != nil && msg != embedErr {
				log.Printf("worker: embeddings: %v", err)
				embedErr = msg
			} else if err == nil {
				embedErr = ""
			}
		}
		// workflows with a schedule trigger due this minute: a digest, a weekly review
		if m := time.Now().Truncate(time.Minute); m.After(scheduledAt) {
			scheduledAt = m
			started, err := s.StartScheduled(ctx, tenant, m)
			for _, ref := range started {
				log.Printf("worker: scheduled run %s started", ref)
			}
			errs = append(errs, err)
		}
		held := false
		for _, err := range errs {
			if err == nil {
				continue
			}
			if *once {
				return err
			}
			if verbs.Code(err) == "maintenance" {
				held = true
				continue
			}
			log.Printf("worker: %v", err) // the next pass retries; the proposal stays approved, the label pending
		}
		if held != paused {
			paused = held
			log.Printf("worker: maintenance read-only = %v (paused while it lasts)", paused)
		}
		if *once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}
