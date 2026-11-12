package server

// Per-person limits: tool calls per minute, written bytes per minute and live
// SSE streams. Keyed by the root human, not the token (an OAuth access token changes every hour).
// In-memory sliding windows: one facade instance (appliance).

import (
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v >= 0 {
		return v
	}
	return def
}

// Limits; 0 disables a limit (test instances, evals).
var (
	RateCalls   = envInt("LITOPYS_RATE_CALLS", 120)    // MCP requests per minute per person
	RateWriteKB = envInt("LITOPYS_RATE_WRITE_KB", 300) // argument bytes of writing tools per minute
	rateWindow  = time.Minute
)

type window struct {
	mu   sync.Mutex
	hits map[uuid.UUID][]hit
}

type hit struct {
	at     time.Time
	weight int
}

// take adds weight for key unless the window would exceed limit; returns false and the wait otherwise.
func (w *window) take(key uuid.UUID, weight, limit int) (bool, time.Duration) {
	if limit <= 0 {
		return true, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hits == nil {
		w.hits = map[uuid.UUID][]hit{}
	}
	now := time.Now()
	var keep []hit
	sum := 0
	for _, h := range w.hits[key] {
		if now.Sub(h.at) < rateWindow {
			keep = append(keep, h)
			sum += h.weight
		}
	}
	if sum+weight > limit && len(keep) > 0 {
		w.hits[key] = keep
		return false, rateWindow - now.Sub(keep[0].at)
	}
	w.hits[key] = append(keep, hit{now, weight})
	return true, 0
}

// owners caches actor -> root human for the rate limiter (roots never change).
type owners struct{ m sync.Map }

func (o *owners) of(r *http.Request, f *Server, actor uuid.UUID) uuid.UUID {
	if v, ok := o.m.Load(actor); ok {
		return v.(uuid.UUID)
	}
	var root *uuid.UUID
	if err := f.DB.QueryRow(r.Context(), `SELECT litopys_root($1)`, actor).Scan(&root); err != nil || root == nil {
		return actor
	}
	o.m.Store(actor, *root)
	return *root
}

func TooMany(w http.ResponseWriter, wait time.Duration, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	RestError(w, http.StatusTooManyRequests, msg)
}
