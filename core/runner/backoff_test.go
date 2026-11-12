package runner

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPollDelay: a failing executor backs off from the poll interval, doubling up to MaxPollBackoff.
func TestPollDelay(t *testing.T) {
	base := 15 * time.Second
	for _, c := range []struct {
		fails int
		want  time.Duration
	}{{0, base}, {1, 30 * time.Second}, {3, 2 * time.Minute}, {6, 15 * time.Minute}, {100, MaxPollBackoff}} {
		if got := pollDelay(base, c.fails); got != c.want {
			t.Errorf("pollDelay(%v, %d) = %v, want %v", base, c.fails, got, c.want)
		}
	}
}

// TestReconcile: the file's executor keeps running whatever the server lists; a server executor starts, restarts when
// its key changes and stops when gone.
func TestReconcile(t *testing.T) {
	file, srv := uuid.New(), uuid.New()
	fromFile := map[uuid.UUID]bool{file: true}
	running := map[uuid.UUID]running{file: runningOf(Executor{ActorID: file, BaseURL: "g", Model: "m"}, nil)}
	stop, add := reconcile(running, fromFile, nil)
	if len(stop) != 0 || len(add) != 0 {
		t.Fatalf("an empty server list touched the file executor: stop %v add %v", stop, add)
	}
	e := Executor{ActorID: srv, BaseURL: "u", Model: "m", Key: "k1"}
	if stop, add = reconcile(running, fromFile, []Executor{e}); len(stop) != 0 || len(add) != 1 {
		t.Fatalf("a new server executor: stop %v add %v", stop, add)
	}
	running[srv] = runningOf(e, nil)
	e.Key = "k2"
	if stop, add = reconcile(running, fromFile, []Executor{e}); len(stop) != 1 || len(add) != 1 {
		t.Fatalf("a changed key: stop %v add %v", stop, add)
	}
	if stop, _ = reconcile(running, fromFile, nil); len(stop) != 1 || stop[0] != srv {
		t.Fatalf("a gone server executor: stop %v", stop)
	}
}
