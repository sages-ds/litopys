package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// `litopys update`: the order of an update and what a failure at each step leaves behind.
// The steps are behind three interfaces so the order and the rollbacks are tested without a platform; the live
// implementations are the truenas adapter, the database of the installation and the HTTP endpoint of the core.

// UpdatePlatform is what an update does to the services and the data of the installation.
type UpdatePlatform interface {
	StopApp(ctx context.Context, name string) error
	StartApp(ctx context.Context, name string) error
	// AppCompose returns the compose the app runs now — what a rollback puts back.
	AppCompose(ctx context.Context, name string) (any, error)
	// SetAppCompose replaces the compose of the app (a string to render, or what AppCompose returned).
	SetAppCompose(ctx context.Context, name string, compose any) error
	Snapshot(ctx context.Context, dataset, name string) (string, error)
	Rollback(ctx context.Context, id string) error
	Snapshots(ctx context.Context, dataset string) ([]string, error)
	DeleteSnapshot(ctx context.Context, id string) error
}

// UpdateDatabase is what an update does in the database.
type UpdateDatabase interface {
	// ReadOnly reads the mode and its reason (false on a database from before 0042).
	ReadOnly(ctx context.Context) (bool, string, error)
	Maintenance(ctx context.Context, readOnly bool, reason string) error
	Drain(ctx context.Context, timeout time.Duration) error
	// Rehearse runs the migrations, a rebuild if the projector version changed and verify on a throw-away copy of
	// the database, then drops it; the live database is not touched. Nobody may be connected to it (core stopped).
	Rehearse(ctx context.Context) error
	// Upgrade does the same on the live database.
	Upgrade(ctx context.Context) error
	// WaitReady waits until the database accepts connections (after a start it may still recover its WAL).
	WaitReady(ctx context.Context, timeout time.Duration) error
}

// UpdateHealth checks the running core.
type UpdateHealth interface {
	// Commit is the commit the core reports (/version); "" when it does not answer.
	Commit(ctx context.Context) string
	// Check: /healthz, the commit, no projector lag, the projector version of the code.
	Check(ctx context.Context, commit string) error
}

// Updater runs one update to a release.
type Updater struct {
	P UpdatePlatform
	D UpdateDatabase
	H UpdateHealth

	Core, DB string // app names (services.core, services.db)
	Dataset  string // the dataset of the database (db_dataset)
	Commit   string // of the release
	Compose  string // the core's compose for the release, rendered
	Keep     int    // update snapshots kept after a successful update (at least 1)
	Drain    time.Duration
	Health   time.Duration // how long the new core may take to become healthy
	Log      func(format string, a ...any)
	Now      func() time.Time
}

// UpdateResult says what happened.
type UpdateResult struct {
	Outcome  string `json:"outcome"` // unchanged | updated | rolled_back | stopped_before_change
	Snapshot string `json:"snapshot,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ErrUpdate is wrapped by every error of a failed update.
var ErrUpdate = errors.New("update failed")

// updateReason and rollbackReason start the read-only reason of an update and of a rollback: a run that finds one
// knows it did not finish.
const (
	updateReason   = "update to "
	rollbackReason = "rollback to "
)

// SnapshotName is the name of an update snapshot: the time it is taken and the release (12 characters of its commit)
// whose data it holds — "unknown" when the core did not answer.
func SnapshotName(at time.Time, from string) string {
	if from == "" {
		from = "unknown"
	}
	return UpdateSnapshotPrefix + at.UTC().Format("20060102T150405") + "-" + short(from)
}

// SnapshotFrom is the release a snapshot's data belongs to ("" for a name without it).
func SnapshotFrom(id string) string {
	_, name, _ := strings.Cut(id, "@")
	parts := strings.Split(strings.TrimPrefix(name, UpdateSnapshotPrefix), "-")
	if len(parts) < 2 || parts[1] == "unknown" {
		return ""
	}
	return parts[1]
}

// Run updates to the release. A failure before the snapshot leaves the live data untouched and starts the old core
// again; a failure after it rolls the data back to the snapshot and puts the old compose back.
func (u *Updater) Run(ctx context.Context) (UpdateResult, error) {
	u.defaults()
	reason := updateReason + short(u.Commit)
	// a previous update that did not finish leaves its read-only behind: what runs now may be neither the old nor
	// the new release, so nothing is taken for "old" until a person has looked
	if ro, why, err := u.D.ReadOnly(ctx); err != nil {
		return UpdateResult{Outcome: "stopped_before_change", Error: err.Error()}, fmt.Errorf("%w: read-only state: %w", ErrUpdate, err)
	} else if ro && (strings.HasPrefix(why, updateReason) || strings.HasPrefix(why, rollbackReason)) {
		err := fmt.Errorf("%w: a previous update or rollback did not finish (read-only: %s) — check the core and the "+
			"data, then `litopys maintenance off`", ErrUpdate, why)
		return UpdateResult{Outcome: "stopped_before_change", Error: err.Error()}, err
	}
	running := u.H.Commit(ctx)
	if c := running; c != "" && c == u.Commit {
		if err := u.H.Check(ctx, u.Commit); err == nil {
			u.Log("unchanged: the core already runs %s and is healthy", short(u.Commit))
			return UpdateResult{Outcome: "unchanged"}, nil
		}
	}
	// 1. read-only, then wait for the commands already running
	if err := u.D.Maintenance(ctx, true, reason); err != nil {
		return u.stopped(ctx, false, fmt.Errorf("read-only: %w", err))
	}
	u.Log("read-only: %s", reason)
	if err := u.D.Drain(ctx, u.Drain); err != nil {
		return u.stopped(ctx, false, err)
	}
	u.Log("drained")
	old, err := u.P.AppCompose(ctx, u.Core)
	if err != nil {
		return u.stopped(ctx, false, fmt.Errorf("current compose of %s: %w", u.Core, err))
	}
	// 2. stop the core: the copy for the rehearsal is a file copy, nobody may be connected
	if err := u.P.StopApp(ctx, u.Core); err != nil {
		return u.stopped(ctx, true, fmt.Errorf("stop %s: %w", u.Core, err))
	}
	u.Log("core stopped")
	// 3. rehearsal on a copy
	if err := u.D.Rehearse(ctx); err != nil {
		return u.stopped(ctx, true, fmt.Errorf("rehearsal on a copy: %w", err))
	}
	u.Log("rehearsal on a copy: ok")
	// 4. snapshot of the data — the point a rollback returns to
	// the name says which release the data belongs to: a rollback to it runs that release again
	snap, err := u.P.Snapshot(ctx, u.Dataset, SnapshotName(u.Now(), running))
	if err != nil {
		return u.stopped(ctx, true, fmt.Errorf("snapshot of %s: %w", u.Dataset, err))
	}
	u.Log("snapshot %s", snap)
	// 5. the live database, 6. the new core
	if err := u.D.Upgrade(ctx); err != nil {
		return u.rollback(ctx, snap, old, fmt.Errorf("live database: %w", err))
	}
	u.Log("live database migrated and verified")
	if err := u.P.SetAppCompose(ctx, u.Core, u.Compose); err != nil {
		return u.rollback(ctx, snap, old, fmt.Errorf("compose of %s: %w", u.Core, err))
	}
	if err := u.P.StartApp(ctx, u.Core); err != nil {
		return u.rollback(ctx, snap, old, fmt.Errorf("start %s: %w", u.Core, err))
	}
	// 7. writes again, then the health of the new core; writes from here to a failed check are lost by the rollback
	if err := u.D.Maintenance(ctx, false, ""); err != nil {
		return u.rollback(ctx, snap, old, fmt.Errorf("leave read-only: %w", err))
	}
	if err := u.healthy(ctx); err != nil {
		return u.rollback(ctx, snap, old, err)
	}
	u.Log("healthy on %s", short(u.Commit))
	u.prune(ctx)
	return UpdateResult{Outcome: "updated", Snapshot: snap}, nil
}

func (u *Updater) defaults() {
	if u.Log == nil {
		u.Log = func(string, ...any) {}
	}
	if u.Now == nil {
		u.Now = time.Now
	}
	if u.Keep < 1 {
		u.Keep = 2
	}
	if u.Drain <= 0 {
		u.Drain = 60 * time.Second
	}
	if u.Health <= 0 {
		u.Health = 2 * time.Minute
	}
}

// healthy waits for the new core to pass the check.
func (u *Updater) healthy(ctx context.Context) error {
	deadline := u.Now().Add(u.Health)
	for {
		err := u.H.Check(ctx, u.Commit)
		if err == nil {
			return nil
		}
		if !u.Now().Before(deadline) {
			return fmt.Errorf("health of the new core: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// cleanup is the context of the steps that restore a working installation: they must run even when the update's
// context is cancelled (Ctrl+C, its timeout) — that is when they are needed most.
func cleanup(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
}

// stopped: a failure before any change of the live data — the old core runs again (when it was stopped), writes too.
func (u *Updater) stopped(ctx context.Context, coreStopped bool, cause error) (UpdateResult, error) {
	ctx, cancel := cleanup(ctx)
	defer cancel()
	var errs []error
	if coreStopped {
		if err := u.P.StartApp(ctx, u.Core); err != nil {
			errs = append(errs, fmt.Errorf("start the old core: %w", err))
		}
	}
	if err := u.D.Maintenance(ctx, false, ""); err != nil {
		errs = append(errs, fmt.Errorf("leave read-only: %w", err))
	}
	u.Log("stopped before any change: %v", cause)
	err := fmt.Errorf("%w: %w; the live data is untouched", ErrUpdate, cause)
	if len(errs) > 0 {
		err = fmt.Errorf("%w; and: %w", err, errors.Join(errs...))
	}
	return UpdateResult{Outcome: "stopped_before_change", Error: cause.Error()}, err
}

// rollback: the data back to the snapshot, the old compose back, writes on again.
func (u *Updater) rollback(ctx context.Context, snap string, old any, cause error) (UpdateResult, error) {
	ctx, cancel := cleanup(ctx)
	defer cancel()
	u.Log("rolling back to %s: %v", snap, cause)
	steps := []struct {
		what string
		do   func() error
	}{
		{"stop the core", func() error { return u.P.StopApp(ctx, u.Core) }},
		{"stop the database", func() error { return u.P.StopApp(ctx, u.DB) }},
		{"roll the data back", func() error { return u.P.Rollback(ctx, snap) }},
		{"start the database", func() error { return u.P.StartApp(ctx, u.DB) }},
		{"wait for the database", func() error { return u.D.WaitReady(ctx, 3*time.Minute) }},
		{"put the old compose back", func() error { return u.P.SetAppCompose(ctx, u.Core, old) }},
		{"start the old core", func() error { return u.P.StartApp(ctx, u.Core) }},
		// the snapshot was taken read-only: the rolled-back flag says so
		{"leave read-only", func() error { return u.D.Maintenance(ctx, false, "") }},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			u.Log("ROLLBACK STOPPED at %q: %v — finish by hand", s.what, err)
			return UpdateResult{Outcome: "rolled_back", Snapshot: snap, Error: cause.Error()},
				fmt.Errorf("%w: %w; rollback stopped at %q: %w", ErrUpdate, cause, s.what, err)
		}
	}
	u.Log("rolled back to %s", snap)
	// the data is the snapshot's again and the old release runs: the snapshot would only mislead a later
	// `litopys rollback` into taking the release that runs now for the earlier one
	if err := u.P.DeleteSnapshot(ctx, snap); err != nil {
		u.Log("delete %s after the rollback: %v — delete it by hand", snap, err)
	}
	return UpdateResult{Outcome: "rolled_back", Snapshot: snap, Error: cause.Error()}, fmt.Errorf("%w: %w; rolled back to %s", ErrUpdate, cause, snap)
}

// prune keeps the newest Keep update snapshots of the dataset; a failure is only logged.
func (u *Updater) prune(ctx context.Context) {
	ids, err := u.P.Snapshots(ctx, u.Dataset)
	if err != nil {
		u.Log("prune: %v", err)
		return
	}
	for i := 0; i < len(ids)-u.Keep; i++ {
		if err := u.P.DeleteSnapshot(ctx, ids[i]); err != nil {
			u.Log("prune %s: %v", ids[i], err)
		}
	}
}
