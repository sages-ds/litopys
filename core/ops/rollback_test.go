package ops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeRollback extends the update's fake installation with what a rollback reads.
type fakeRollback struct {
	*fakeInstall
	lost, auth int64
	head       int
}

func (f *fakeRollback) Later(context.Context, string) ([]string, error) {
	return []string{"d@auto-daily"}, nil
}
func (f *fakeRollback) SnapshotTime(context.Context, string) (time.Time, error) {
	return time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC), nil
}
func (f *fakeRollback) EventsSince(context.Context, time.Time) (int64, error) { return f.lost, nil }
func (f *fakeRollback) AuthSince(context.Context, time.Time) (int64, error)   { return f.auth, nil }
func (f *fakeRollback) MigrationHead(context.Context) (int, error)            { return f.head, nil }

func rollbacker(f *fakeRollback, accept bool) *Rollbacker {
	return &Rollbacker{P: f, D: f, H: f, Core: "core", DB: "db", Dataset: "d", Commit: "old", Head: 40, Compose: "old-compose",
		Accept: accept, Health: time.Second}
}

// TestRollbackPlanAndLoss: the plan names the newest update snapshot, the events and snapshots lost;
// events lost without --accept-loss refuse before any change.
func TestRollbackPlanAndLoss(t *testing.T) {
	ctx := context.Background()
	f := &fakeRollback{fakeInstall: &fakeInstall{commit: "new", healthy: true}, lost: 7}
	r := rollbacker(f, false)
	p, err := r.Plan(ctx)
	if err != nil || p.Snapshot != "d@litopys-update-3" || p.LostEvents != 7 || !slices.Equal(p.LostSnapshots, []string{"d@auto-daily"}) ||
		p.RunningCommit != "new" {
		t.Fatalf("%+v %v", p, err)
	}
	before := len(f.steps)
	if err := r.Run(ctx, p); !errors.Is(err, ErrLoss) || len(f.steps) != before {
		t.Fatalf("a loss without --accept-loss: %v, steps %v", err, f.steps[before:])
	}
}

// TestRollbackRun: with the loss accepted, the steps in order and the old release healthy at the end.
func TestRollbackRun(t *testing.T) {
	ctx := context.Background()
	f := &fakeRollback{fakeInstall: &fakeInstall{commit: "new", healthy: true}, lost: 7, head: 40}
	r := rollbacker(f, true)
	p, err := r.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.steps = nil
	if err := r.Run(ctx, p); err != nil {
		t.Fatal(err)
	}
	want := []string{"read-only on", "drain", "stop core", "stop db", "rollback", "start db", "db ready", "compose core old-compose",
		"start core", "read-only off", "check"}
	if !slices.Equal(f.steps, want) {
		t.Fatalf("steps\n got %v\nwant %v", f.steps, want)
	}
}

// TestRollbackRefusesForeignSnapshot: only snapshots of litopys update.
func TestRollbackRefusesForeignSnapshot(t *testing.T) {
	f := &fakeRollback{fakeInstall: &fakeInstall{healthy: true}}
	r := rollbacker(f, true)
	r.Snapshot = "d@auto-daily"
	if _, err := r.Plan(context.Background()); err == nil {
		t.Fatal("a periodic snapshot must be refused")
	}
}

// TestRollbackGuards: the snapshot's release must be the one given; a schema that does not fit the
// release stops before the core starts; a token change after the snapshot is a loss too.
func TestRollbackGuards(t *testing.T) {
	ctx := context.Background()
	f := &fakeRollback{fakeInstall: &fakeInstall{commit: "new", healthy: true}, head: 40}
	r := rollbacker(f, true)
	r.Snapshot = "d@litopys-update-20261031T010000-0123456789ab"
	p, err := r.Plan(ctx)
	if err != nil || p.SnapshotFrom != "0123456789ab" {
		t.Fatalf("%+v %v", p, err)
	}
	if err := r.Run(ctx, p); err == nil || !strings.Contains(err.Error(), "belongs to release 0123456789ab") {
		t.Fatalf("a snapshot of another release: %v", err)
	}
	r.Snapshot, f.head = "", 41 // the newest, no release in the name; the data's schema is ahead of the release
	f.steps = nil
	p, _ = r.Plan(ctx)
	if err := r.Run(ctx, p); err == nil || !strings.Contains(err.Error(), "migration 0041") || slices.Contains(f.steps, "start core") {
		t.Fatalf("a schema that does not fit: %v %v", err, f.steps)
	}
	f.auth, f.head = 2, 40
	r.Accept = false
	p, _ = r.Plan(ctx)
	if err := r.Run(ctx, p); !errors.Is(err, ErrLoss) {
		t.Fatalf("token changes after the snapshot: %v", err)
	}
}

// TestSnapshotName: the release in the name and back.
func TestSnapshotName(t *testing.T) {
	at := time.Date(2026, 10, 31, 1, 2, 3, 0, time.UTC)
	n := SnapshotName(at, "e2f442293c1f5d74536bd7075227af9b53bfe178")
	if n != "litopys-update-20261031T010203-e2f442293c1f" || SnapshotFrom("d@"+n) != "e2f442293c1f" {
		t.Fatalf("%s %s", n, SnapshotFrom("d@"+n))
	}
	if SnapshotFrom("d@"+SnapshotName(at, "")) != "" || SnapshotFrom("d@litopys-update-3") != "" {
		t.Error("no release in the name must give none")
	}
}
