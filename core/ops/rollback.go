package ops

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// `litopys rollback`: back to the data of an update snapshot and to the release that ran before
// it. Only a person runs it. It first says what is lost — the journal's events recorded after the snapshot and the
// dataset's snapshots taken after it (a recursive zfs rollback destroys them) — and refuses to lose events without
// --accept-loss.

// RollbackPlatform adds to the update's platform what a rollback has to know about a snapshot.
type RollbackPlatform interface {
	UpdatePlatform
	// Later lists the snapshots of the dataset taken after id (what the rollback destroys).
	Later(ctx context.Context, id string) ([]string, error)
	// SnapshotTime is when the snapshot was taken.
	SnapshotTime(ctx context.Context, id string) (time.Time, error)
}

// RollbackDatabase adds the count of what would be lost and the schema check.
type RollbackDatabase interface {
	UpdateDatabase
	// EventsSince counts the journal's events recorded after t (writes the rollback loses).
	EventsSince(ctx context.Context, t time.Time) (int64, error)
	// AuthSince counts the token changes after t — issued and revoked: a revocation the rollback loses makes a
	// revoked token valid again.
	AuthSince(ctx context.Context, t time.Time) (int64, error)
	// MigrationHead is the number of the newest applied migration.
	MigrationHead(ctx context.Context) (int, error)
}

// Rollbacker runs one rollback.
type Rollbacker struct {
	P RollbackPlatform
	D RollbackDatabase
	H UpdateHealth

	Core, DB string
	Dataset  string
	Snapshot string // the update snapshot to return to ("" — the newest)
	Commit   string // of the release to run again
	Head     int    // its migration head (manifest schema.migration_head)
	Compose  string // its compose, rendered
	Accept   bool   // --accept-loss
	Drain    time.Duration
	Health   time.Duration
	Log      func(format string, a ...any)
	Now      func() time.Time
}

// RollbackPlan is what a rollback would do and lose.
type RollbackPlan struct {
	Snapshot      string    `json:"snapshot"`
	TakenAt       time.Time `json:"taken_at"`
	LostEvents    int64     `json:"lost_events"`
	LostAuth      int64     `json:"lost_auth"` // token changes after the snapshot: a lost revocation revives a token
	LostSnapshots []string  `json:"lost_snapshots,omitempty"`
	SnapshotFrom  string    `json:"snapshot_from,omitempty"` // the release the snapshot's data belongs to
	ReadOnly      string    `json:"read_only,omitempty"`     // a read-only left on, with its reason
	ReleaseCommit string    `json:"release_commit"`
	RunningCommit string    `json:"running_commit"`
}

// ErrLoss: the rollback would lose events and --accept-loss was not given.
var ErrLoss = errors.New("the rollback would lose events")

// Plan says what the rollback would do; nothing is changed.
func (r *Rollbacker) Plan(ctx context.Context) (RollbackPlan, error) {
	r.defaults()
	snap := r.Snapshot
	if snap == "" {
		ids, err := r.P.Snapshots(ctx, r.Dataset)
		if err != nil {
			return RollbackPlan{}, err
		}
		if len(ids) == 0 {
			return RollbackPlan{}, fmt.Errorf("no update snapshot of %s to roll back to", r.Dataset)
		}
		snap = ids[len(ids)-1]
	}
	if !isUpdateSnapshot(snap) {
		return RollbackPlan{}, fmt.Errorf("%s is not a snapshot of litopys update", snap)
	}
	at, err := r.P.SnapshotTime(ctx, snap)
	if err != nil {
		return RollbackPlan{}, err
	}
	lost, err := r.D.EventsSince(ctx, at)
	if err != nil {
		return RollbackPlan{}, err
	}
	auth, err := r.D.AuthSince(ctx, at)
	if err != nil {
		return RollbackPlan{}, err
	}
	later, err := r.P.Later(ctx, snap)
	if err != nil {
		return RollbackPlan{}, err
	}
	p := RollbackPlan{Snapshot: snap, TakenAt: at, LostEvents: lost, LostAuth: auth, LostSnapshots: later,
		SnapshotFrom: SnapshotFrom(snap), ReleaseCommit: r.Commit, RunningCommit: r.H.Commit(ctx)}
	if ro, why, err := r.D.ReadOnly(ctx); err == nil && ro {
		p.ReadOnly = why
	}
	return p, nil
}

// Run rolls back by the plan: read-only and drain, stop the core and the database, the snapshot back, the database
// up, the release's compose, the core up, writes on, health. Refuses without Accept when events would be lost.
func (r *Rollbacker) Run(ctx context.Context, p RollbackPlan) error {
	r.defaults()
	if p.SnapshotFrom != "" && p.SnapshotFrom != short(r.Commit) {
		return fmt.Errorf("the data of %s belongs to release %s, not to %s: pass the manifest of %s (bin/releases)",
			p.Snapshot, p.SnapshotFrom, short(r.Commit), p.SnapshotFrom)
	}
	if (p.LostEvents > 0 || p.LostAuth > 0) && !r.Accept {
		return fmt.Errorf("%w: %d event(s) and %d token change(s) after %s (taken %s); pass --accept-loss to lose them",
			ErrLoss, p.LostEvents, p.LostAuth, p.Snapshot, p.TakenAt.UTC().Format(time.RFC3339))
	}
	if err := r.D.Maintenance(ctx, true, rollbackReason+p.Snapshot); err != nil {
		return err
	}
	if err := r.D.Drain(ctx, r.Drain); err != nil {
		c, cancel := cleanup(ctx)
		defer cancel()
		_ = r.D.Maintenance(c, false, "")
		return err
	}
	// what is lost now: writes between the plan and the read-only count too
	if n, err := r.D.EventsSince(ctx, p.TakenAt); err == nil && n != p.LostEvents {
		r.Log("events after the snapshot now %d (planned %d)", n, p.LostEvents)
	}
	// past this point a half-done rollback is worse than a finished one: no cancellation
	ctx, cancel := cleanup(ctx)
	defer cancel()
	steps := []struct {
		what string
		do   func() error
	}{
		{"stop the core", func() error { return r.P.StopApp(ctx, r.Core) }},
		{"stop the database", func() error { return r.P.StopApp(ctx, r.DB) }},
		{"roll the data back", func() error { return r.P.Rollback(ctx, p.Snapshot) }},
		{"start the database", func() error { return r.P.StartApp(ctx, r.DB) }},
		{"wait for the database", func() error { return r.D.WaitReady(ctx, 3*time.Minute) }},
		// the release must fit the data: its migration head is the snapshot's (before the core starts on it)
		{"check the schema", func() error {
			got, err := r.D.MigrationHead(ctx)
			if err == nil && r.Head > 0 && got != r.Head {
				err = fmt.Errorf("the data has migration %04d, release %s expects %04d — the core stays stopped", got, short(r.Commit), r.Head)
			}
			return err
		}},
		{"compose of the release", func() error { return r.P.SetAppCompose(ctx, r.Core, r.Compose) }},
		{"start the core", func() error { return r.P.StartApp(ctx, r.Core) }},
		{"leave read-only", func() error { return r.D.Maintenance(ctx, false, "") }},
	}
	for _, s := range steps {
		r.Log("%s", s.what)
		if err := s.do(); err != nil {
			return fmt.Errorf("rollback stopped at %q: %w — finish by hand", s.what, err)
		}
	}
	deadline := r.Now().Add(r.Health)
	for {
		err := r.H.Check(ctx, r.Commit)
		if err == nil {
			r.Log("rolled back to %s, running %s", p.Snapshot, short(r.Commit))
			return nil
		}
		if !r.Now().Before(deadline) {
			return fmt.Errorf("rolled back to %s, but the core is not healthy: %w", p.Snapshot, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (r *Rollbacker) defaults() {
	if r.Log == nil {
		r.Log = func(string, ...any) {}
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Drain <= 0 {
		r.Drain = 60 * time.Second
	}
	if r.Health <= 0 {
		r.Health = 2 * time.Minute
	}
}
