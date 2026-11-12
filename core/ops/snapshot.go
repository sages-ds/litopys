package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The writes of the truenas adapter that `litopys update` and `rollback` need:
// a snapshot of the database dataset taken after the drain, rolled back on a failed update, and the stop / start
// of an app around it. Snapshots need the TrueNAS role SNAPSHOT_WRITE (create, rollback) and SNAPSHOT_DELETE.

// JobCaller runs a middleware method that is a job and waits for its result (midclt call -j).
type JobCaller interface {
	CallJob(ctx context.Context, method string, args ...any) (json.RawMessage, error)
}

// UpdateSnapshotPrefix names the snapshots `litopys update` takes, so pruning and rollback find only its own.
const UpdateSnapshotPrefix = "litopys-update-"

// Snapshot takes a snapshot of one dataset (not recursive) and returns its id, dataset@name. It is atomic and, taken
// of a running PostgreSQL, crash-consistent: the WAL (pg_wal) lives inside PGDATA on the same dataset, so a start from
// it recovers like after a power loss. The name carries the time (litopys-update-YYYYMMDDThhmmss): it sorts.
func (t TrueNAS) Snapshot(ctx context.Context, dataset, name string) (string, error) {
	if !strings.HasPrefix(name, UpdateSnapshotPrefix) {
		return "", fmt.Errorf("snapshot %q: the name must start with %s", name, UpdateSnapshotPrefix)
	}
	raw, err := t.C.Call(ctx, "pool.snapshot.create", map[string]any{"dataset": dataset, "name": name, "recursive": false})
	if err != nil {
		return "", err
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.ID == "" {
		return "", fmt.Errorf("pool.snapshot.create: no id in %s", raw)
	}
	return got.ID, nil
}

// Snapshots lists the update snapshots of a dataset, oldest first (by name: it carries the time).
func (t TrueNAS) Snapshots(ctx context.Context, dataset string) ([]string, error) {
	raw, err := t.C.Call(ctx, "pool.snapshot.query",
		[]any{[]any{"dataset", "=", dataset}, []any{"snapshot_name", "^", UpdateSnapshotPrefix}},
		map[string]any{"select": []string{"id"}})
	if err != nil {
		return nil, err
	}
	var got []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, fmt.Errorf("pool.snapshot.query: %w", err)
	}
	ids := make([]string, len(got))
	for i, s := range got {
		ids[i] = s.ID
	}
	sort.Strings(ids)
	return ids, nil
}

// Rollback returns a dataset to an update snapshot: recursive destroys the snapshots taken after it (zfs rollback -r)
// — the periodic ones of the pool too, which may break an incremental replication until its next full run; force
// unmounts (-f). The app that uses the dataset must be stopped first: PostgreSQL must not hold the files.
// Later returns the snapshots a rollback to id would destroy, so the caller can show them first.
func (t TrueNAS) Rollback(ctx context.Context, id string) error {
	if !isUpdateSnapshot(id) {
		return fmt.Errorf("rollback %q: only snapshots of litopys update", id)
	}
	_, err := t.C.Call(ctx, "pool.snapshot.rollback", id, map[string]any{"recursive": true, "force": true})
	return err
}

// Later lists every snapshot of the dataset of id taken after it — what Rollback(id) destroys.
func (t TrueNAS) Later(ctx context.Context, id string) ([]string, error) {
	dataset, _, ok := strings.Cut(id, "@")
	if !ok {
		return nil, fmt.Errorf("snapshot id %q: no @", id)
	}
	raw, err := t.C.Call(ctx, "pool.snapshot.query", []any{[]any{"dataset", "=", dataset}},
		map[string]any{"select": []string{"id", "properties"}})
	if err != nil {
		return nil, err
	}
	var got []struct {
		ID         string `json:"id"`
		Properties struct {
			CreateTXG struct {
				Rawvalue string `json:"rawvalue"` // a decimal string (parsed is a string too)
			} `json:"createtxg"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, fmt.Errorf("pool.snapshot.query: %w", err)
	}
	txg := make(map[string]int64, len(got))
	for _, s := range got {
		n, err := strconv.ParseInt(s.Properties.CreateTXG.Rawvalue, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: createtxg %q: %w", s.ID, s.Properties.CreateTXG.Rawvalue, err)
		}
		txg[s.ID] = n
	}
	at, ok := txg[id]
	if !ok {
		return nil, fmt.Errorf("snapshot %s not found", id)
	}
	var later []string
	for _, s := range got {
		if txg[s.ID] > at {
			later = append(later, s.ID)
		}
	}
	sort.Strings(later)
	return later, nil
}

// DeleteSnapshot removes an update snapshot (pruning after a successful update).
func (t TrueNAS) DeleteSnapshot(ctx context.Context, id string) error {
	if !isUpdateSnapshot(id) {
		return fmt.Errorf("delete %q: only snapshots of litopys update", id)
	}
	_, err := t.C.Call(ctx, "pool.snapshot.delete", id)
	return err
}

func isUpdateSnapshot(id string) bool {
	_, name, ok := strings.Cut(id, "@")
	return ok && strings.HasPrefix(name, UpdateSnapshotPrefix)
}

// StopApp and StartApp wait for the job; a stopped app stays stopped, a running one running (idempotent).
func (t TrueNAS) StopApp(ctx context.Context, name string) error {
	return t.appJob(ctx, "app.stop", name, "STOPPED")
}
func (t TrueNAS) StartApp(ctx context.Context, name string) error {
	return t.appJob(ctx, "app.start", name, "RUNNING")
}

func (t TrueNAS) appJob(ctx context.Context, method, name, want string) error {
	apps, err := t.Apps(ctx, []string{name})
	if err != nil {
		return err
	}
	a, ok := apps[name]
	if !ok {
		return fmt.Errorf("app %q not found", name)
	}
	if a.State == want {
		return nil
	}
	j, ok := t.C.(JobCaller)
	if !ok {
		return fmt.Errorf("%s: the transport cannot wait for jobs", method)
	}
	if _, err := j.CallJob(ctx, method, name); err != nil {
		return err
	}
	apps, err = t.Apps(ctx, []string{name})
	if err != nil {
		return err
	}
	if got := apps[name].State; got != want {
		return fmt.Errorf("%s %s: state %s after the job, want %s", method, name, got, want)
	}
	return nil
}

// CallJob runs a job method and waits for it (midclt call -j).
func (s SSHMidclt) CallJob(ctx context.Context, method string, args ...any) (json.RawMessage, error) {
	return s.call(ctx, true, method, args...)
}

// SnapshotTime is when a snapshot was taken (its creation property).
func (t TrueNAS) SnapshotTime(ctx context.Context, id string) (time.Time, error) {
	raw, err := t.C.Call(ctx, "pool.snapshot.query", []any{[]any{"id", "=", id}}, map[string]any{"select": []string{"id", "properties"}})
	if err != nil {
		return time.Time{}, err
	}
	var got []struct {
		Properties struct {
			Creation struct {
				Rawvalue string `json:"rawvalue"` // seconds since the epoch
			} `json:"creation"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return time.Time{}, fmt.Errorf("pool.snapshot.query: %w", err)
	}
	if len(got) == 0 {
		return time.Time{}, fmt.Errorf("snapshot %s not found", id)
	}
	sec, err := strconv.ParseInt(got[0].Properties.Creation.Rawvalue, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("snapshot %s: creation %q: %w", id, got[0].Properties.Creation.Rawvalue, err)
	}
	return time.Unix(sec, 0), nil
}
