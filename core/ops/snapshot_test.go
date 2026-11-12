package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fakeNAS answers middleware calls from a table and records them (method and the JSON of the arguments).
type fakeNAS struct {
	answers map[string]string
	calls   []string
	state   string // of the app: app.query reports it, the jobs app.stop / app.start change it
}

func (f *fakeNAS) Call(_ context.Context, method string, args ...any) (json.RawMessage, error) {
	return f.answer(method, args)
}

func (f *fakeNAS) CallJob(_ context.Context, method string, args ...any) (json.RawMessage, error) {
	switch method {
	case "app.stop":
		f.state = "STOPPED"
	case "app.start":
		f.state = "RUNNING"
	}
	return f.answer("job:"+method, args)
}

func (f *fakeNAS) answer(method string, args []any) (json.RawMessage, error) {
	b, _ := json.Marshal(args)
	f.calls = append(f.calls, method+" "+string(b))
	if method == "app.query" {
		return json.RawMessage(`[{"name": "litopys-pg", "state": "` + f.state + `"}]`), nil
	}
	a, ok := f.answers[method]
	if !ok {
		return nil, fmt.Errorf("%s: not allowed", method)
	}
	return json.RawMessage(a), nil
}

// TestTrueNASSnapshots: snapshots carry the update prefix, rollback and delete refuse any other snapshot, and
// stop / start wait for the job only when the app is not already there.
func TestTrueNASSnapshots(t *testing.T) {
	ctx := context.Background()
	f := &fakeNAS{answers: map[string]string{
		"pool.snapshot.create": `{"id": "fastpool/apps/litopys-pg/data@litopys-update-20261031T0100"}`,
		// the shape TrueNAS 25 answers: createtxg values are strings
		"pool.snapshot.query": `[{"id": "d@litopys-update-2", "properties": {"createtxg": {"rawvalue": "20", "parsed": "20"}}},
			{"id": "d@auto-daily", "properties": {"createtxg": {"rawvalue": "30", "parsed": "30"}}},
			{"id": "d@litopys-update-1", "properties": {"createtxg": {"rawvalue": "10", "parsed": "10"}}}]`,
		"pool.snapshot.rollback": `null`,
		"pool.snapshot.delete":   `true`,
		"job:app.stop":           `null`,
		"job:app.start":          `null`,
	}, state: "RUNNING"}
	nas := TrueNAS{C: f}
	id, err := nas.Snapshot(ctx, "fastpool/apps/litopys-pg/data", "litopys-update-20261031T0100")
	if err != nil || id != "fastpool/apps/litopys-pg/data@litopys-update-20261031T0100" {
		t.Fatalf("snapshot: %q %v", id, err)
	}
	if _, err := nas.Snapshot(ctx, "d", "manual"); err == nil {
		t.Error("a snapshot without the update prefix must be refused")
	}
	ids, err := nas.Snapshots(ctx, "d")
	if err != nil || len(ids) != 3 || ids[1] != "d@litopys-update-1" || ids[2] != "d@litopys-update-2" {
		t.Fatalf("snapshots (sorted by name): %v %v", ids, err)
	}
	// what a rollback to update-2 destroys: the later daily snapshot, by creation (createtxg), not by name
	if later, err := nas.Later(ctx, "d@litopys-update-2"); err != nil || len(later) != 1 || later[0] != "d@auto-daily" {
		t.Fatalf("later: %v %v", later, err)
	}
	if err := nas.Rollback(ctx, "d@auto-2026-10-31"); err == nil {
		t.Error("rollback to a snapshot that is not an update's must be refused")
	}
	if err := nas.DeleteSnapshot(ctx, "d@auto-2026-10-31"); err == nil {
		t.Error("deleting a snapshot that is not an update's must be refused")
	}
	if err := nas.Rollback(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := nas.StopApp(ctx, "litopys-pg"); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second start finds it RUNNING: no job
		if err := nas.StartApp(ctx, "litopys-pg"); err != nil {
			t.Fatal(err)
		}
	}
	all := strings.Join(f.calls, "\n")
	for _, want := range []string{`pool.snapshot.rollback ["fastpool/apps/litopys-pg/data@litopys-update-20261031T0100",{"force":true,"recursive":true}]`,
		`job:app.stop ["litopys-pg"]`} {
		if !strings.Contains(all, want) {
			t.Errorf("missing call %s in\n%s", want, all)
		}
	}
	if n := strings.Count(all, "job:app.start"); n != 1 {
		t.Errorf("app.start jobs: %d, want 1 (a running app is not started again)", n)
	}
}
