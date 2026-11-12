package ops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeInstall records the steps of an update and fails the one named in fail.
type fakeInstall struct {
	fail     string
	steps    []string
	commit   string // what the core reports
	healthy  bool
	readOnly string // the reason of a read-only left on, "" for off
}

func (f *fakeInstall) do(step string) error {
	f.steps = append(f.steps, step)
	if step == f.fail {
		return errors.New(step + " broke")
	}
	return nil
}

func (f *fakeInstall) StopApp(_ context.Context, n string) error  { return f.do("stop " + n) }
func (f *fakeInstall) StartApp(_ context.Context, n string) error { return f.do("start " + n) }
func (f *fakeInstall) AppCompose(_ context.Context, n string) (any, error) {
	return "old-compose", f.do("read compose " + n)
}
func (f *fakeInstall) SetAppCompose(_ context.Context, n string, c any) error {
	if c == "new-compose" {
		f.commit = "new"
	} else {
		f.commit = "old"
	}
	return f.do("compose " + n + " " + c.(string))
}
func (f *fakeInstall) Snapshot(_ context.Context, ds, name string) (string, error) {
	return ds + "@" + name, f.do("snapshot")
}
func (f *fakeInstall) Rollback(_ context.Context, id string) error { return f.do("rollback") }
func (f *fakeInstall) Snapshots(context.Context, string) ([]string, error) {
	return []string{"d@litopys-update-1", "d@litopys-update-2", "d@litopys-update-3"}, f.do("list snapshots")
}
func (f *fakeInstall) DeleteSnapshot(_ context.Context, id string) error { return f.do("delete " + id) }
func (f *fakeInstall) Maintenance(_ context.Context, on bool, _ string) error {
	if on {
		return f.do("read-only on")
	}
	return f.do("read-only off")
}
func (f *fakeInstall) Drain(context.Context, time.Duration) error { return f.do("drain") }
func (f *fakeInstall) ReadOnly(context.Context) (bool, string, error) {
	return f.readOnly != "", f.readOnly, nil
}
func (f *fakeInstall) WaitReady(context.Context, time.Duration) error { return f.do("db ready") }
func (f *fakeInstall) Rehearse(context.Context) error                 { return f.do("rehearse") }
func (f *fakeInstall) Upgrade(context.Context) error                  { return f.do("upgrade") }
func (f *fakeInstall) Commit(context.Context) string                  { return f.commit }
func (f *fakeInstall) Check(_ context.Context, commit string) error {
	if err := f.do("check"); err != nil {
		return err
	}
	if !f.healthy || f.commit != commit {
		return errors.New("not healthy")
	}
	return nil
}

func run(t *testing.T, f *fakeInstall) (UpdateResult, error) {
	t.Helper()
	at := time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC)
	u := &Updater{P: f, D: f, H: f, Core: "core", DB: "db", Dataset: "d", Commit: "new", Compose: "new-compose", Keep: 2,
		Health: time.Nanosecond, Now: func() time.Time { at = at.Add(time.Second); return at }}
	return u.Run(context.Background())
}

// TestUpdateOrder: the steps of a good update in order; the oldest update snapshot is pruned.
func TestUpdateOrder(t *testing.T) {
	f := &fakeInstall{commit: "old", healthy: true}
	res, err := run(t, f)
	if err != nil || res.Outcome != "updated" || !strings.HasPrefix(res.Snapshot, "d@litopys-update-20261031T") {
		t.Fatalf("%+v %v", res, err)
	}
	want := []string{"read-only on", "drain", "read compose core", "stop core", "rehearse", "snapshot", "upgrade",
		"compose core new-compose", "start core", "read-only off", "check", "list snapshots", "delete d@litopys-update-1"}
	if !slices.Equal(f.steps, want) {
		t.Fatalf("steps\n got %v\nwant %v", f.steps, want)
	}
}

// TestUpdateUnchanged: the same release again, healthy — nothing is done.
func TestUpdateUnchanged(t *testing.T) {
	f := &fakeInstall{commit: "new", healthy: true}
	res, err := run(t, f)
	if err != nil || res.Outcome != "unchanged" || !slices.Equal(f.steps, []string{"check"}) {
		t.Fatalf("%+v %v %v", res, err, f.steps)
	}
}

// TestUpdateRehearsalFails: a failure at the rehearsal changes nothing live — no snapshot, no upgrade, the old
// core runs again and writes are back.
func TestUpdateRehearsalFails(t *testing.T) {
	f := &fakeInstall{commit: "old", healthy: true, fail: "rehearse"}
	res, err := run(t, f)
	if !errors.Is(err, ErrUpdate) || res.Outcome != "stopped_before_change" {
		t.Fatalf("%+v %v", res, err)
	}
	for _, never := range []string{"snapshot", "upgrade", "rollback", "compose core new-compose"} {
		if slices.Contains(f.steps, never) {
			t.Errorf("%q after a failed rehearsal: %v", never, f.steps)
		}
	}
	if n := len(f.steps); f.steps[n-2] != "start core" || f.steps[n-1] != "read-only off" {
		t.Errorf("the old core and writes must be back: %v", f.steps)
	}
}

// TestUpdateHealthFails: a failure after the snapshot rolls the data back and puts the old compose back.
func TestUpdateHealthFails(t *testing.T) {
	f := &fakeInstall{commit: "old", healthy: false}
	res, err := run(t, f)
	if !errors.Is(err, ErrUpdate) || res.Outcome != "rolled_back" {
		t.Fatalf("%+v %v", res, err)
	}
	i := slices.Index(f.steps, "rollback")
	if i < 0 {
		t.Fatalf("no rollback: %v", f.steps)
	}
	want := []string{"stop core", "stop db", "rollback", "start db", "db ready", "compose core old-compose", "start core", "read-only off"}
	if got := f.steps[i-2 : i-2+len(want)]; !slices.Equal(got, want) {
		t.Fatalf("rollback steps\n got %v\nwant %v", got, want)
	}
	if f.commit != "old" {
		t.Error("the old compose must run after the rollback")
	}
}

// TestUpdateUpgradeFails: the live migration fails after the snapshot — rolled back, the new compose never applied.
func TestUpdateUpgradeFails(t *testing.T) {
	f := &fakeInstall{commit: "old", healthy: true, fail: "upgrade"}
	res, err := run(t, f)
	if !errors.Is(err, ErrUpdate) || res.Outcome != "rolled_back" || slices.Contains(f.steps, "compose core new-compose") {
		t.Fatalf("%+v %v %v", res, err, f.steps)
	}
}

// TestUpdateRollbackStops: a rollback that cannot finish says where it stopped.
func TestUpdateRollbackStops(t *testing.T) {
	f := &fakeInstall{commit: "old", healthy: true, fail: "upgrade"}
	f2 := &failTwice{fakeInstall: f, second: "rollback"}
	at := time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC)
	u := &Updater{P: f2, D: f2, H: f2, Core: "core", DB: "db", Dataset: "d", Commit: "new", Compose: "new-compose",
		Health: time.Nanosecond, Now: func() time.Time { at = at.Add(time.Second); return at }}
	_, err := u.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `rollback stopped at "roll the data back"`) {
		t.Fatalf("%v", err)
	}
}

// failTwice fails a second step too (the rollback itself).
type failTwice struct {
	*fakeInstall
	second string
}

func (f *failTwice) Rollback(context.Context, string) error {
	f.steps = append(f.steps, "rollback")
	return errors.New("rollback broke")
}

// TestUpdateUnfinished: a read-only left by an update that did not finish stops the next one before
// anything — the compose that runs may be the new one, it must not be taken for the old.
func TestUpdateUnfinished(t *testing.T) {
	f := &fakeInstall{commit: "new", healthy: true, readOnly: "update to new"}
	res, err := run(t, f)
	if !errors.Is(err, ErrUpdate) || res.Outcome != "stopped_before_change" || len(f.steps) != 0 {
		t.Fatalf("%+v %v %v", res, err, f.steps)
	}
	f = &fakeInstall{commit: "old", healthy: true, readOnly: "the owner's maintenance"} // someone else's: go on
	if res, err := run(t, f); err != nil || res.Outcome != "updated" {
		t.Fatalf("%+v %v", res, err)
	}
}

// TestUpdateCancelledRollsBack: a cancelled context (Ctrl+C, timeout) during the live upgrade still rolls back, and no
// step of the rollback sees the cancelled context.
func TestUpdateCancelledRollsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &cancelAt{fakeInstall: &fakeInstall{commit: "old", healthy: true}, at: "upgrade", cancel: cancel}
	at := time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC)
	u := &Updater{P: f, D: f, H: f, Core: "core", DB: "db", Dataset: "d", Commit: "new", Compose: "new-compose",
		Health: time.Nanosecond, Now: func() time.Time { at = at.Add(time.Second); return at }}
	res, err := u.Run(ctx)
	if res.Outcome != "rolled_back" || err == nil || f.after != 0 || !slices.Contains(f.steps, "rollback") {
		t.Fatalf("%+v %v; %d rollback steps saw a cancelled context; %v", res, err, f.after, f.steps)
	}
}

// cancelAt cancels the update's context at one step and counts later steps that see it cancelled.
type cancelAt struct {
	*fakeInstall
	at     string
	cancel context.CancelFunc
	fired  bool
	after  int
}

func (c *cancelAt) step(ctx context.Context, name string, do func() error) error {
	if c.fired && ctx.Err() != nil {
		c.after++
	}
	if name == c.at {
		c.cancel()
		c.fired = true
		c.steps = append(c.steps, name)
		return ctx.Err()
	}
	return do()
}

func (c *cancelAt) Upgrade(ctx context.Context) error {
	return c.step(ctx, "upgrade", func() error { return c.fakeInstall.Upgrade(ctx) })
}
func (c *cancelAt) StopApp(ctx context.Context, n string) error {
	return c.step(ctx, "stop "+n, func() error { return c.fakeInstall.StopApp(ctx, n) })
}
func (c *cancelAt) Rollback(ctx context.Context, id string) error {
	return c.step(ctx, "rollback", func() error { return c.fakeInstall.Rollback(ctx, id) })
}
func (c *cancelAt) StartApp(ctx context.Context, n string) error {
	return c.step(ctx, "start "+n, func() error { return c.fakeInstall.StartApp(ctx, n) })
}
func (c *cancelAt) Maintenance(ctx context.Context, on bool, r string) error {
	return c.step(ctx, "maintenance", func() error { return c.fakeInstall.Maintenance(ctx, on, r) })
}

// TestUpdateRollbackDeletesSnapshot: after an automatic rollback the update's snapshot goes, so a
// later `litopys rollback` does not take the running release's data for the earlier one's; the name says whose data.
func TestUpdateRollbackDeletesSnapshot(t *testing.T) {
	f := &fakeInstall{commit: "0123456789abcdef", healthy: false}
	res, _ := run(t, f)
	if res.Outcome != "rolled_back" || !strings.HasSuffix(res.Snapshot, "-0123456789ab") {
		t.Fatalf("%+v", res)
	}
	if last := f.steps[len(f.steps)-1]; last != "delete "+res.Snapshot {
		t.Fatalf("the snapshot must be deleted after the rollback: %v", f.steps)
	}
}
