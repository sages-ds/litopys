package server

// Global admission control: the per-person limits of limits.go do not stop many people or
// agents together from exhausting one instance. Two gates in front of every tool call:
//   - a tenant budget of calls per minute — HTTP 429 with Retry-After;
//   - a cap on concurrent calls of the instance with a short wait in line — HTTP 503 with Retry-After, so a burst
//     waits for a free slot instead of piling up on the database pool until statement_timeout.
// Live SSE streams are long and idle, they take no slot (MaxStreams of limits.go bounds them).

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Limits; 0 disables a gate.
var (
	MaxInflight     = envInt("LITOPYS_MAX_INFLIGHT", 32)       // concurrent tool calls of the instance
	AdmitWaitMS     = envInt("LITOPYS_ADMIT_WAIT_MS", 3000)    // how long a call waits for a free slot
	RateTenantCalls = envInt("LITOPYS_RATE_TENANT_CALLS", 600) // tool calls per minute of the whole tenant
)

type admission struct {
	once  sync.Once
	slots chan struct{} // nil: no cap

	inflight                atomic.Int64
	admitted, shed, limited atomic.Int64 // counters for /metrics
	tenant                  window
}

func (a *admission) init() {
	a.once.Do(func() {
		if MaxInflight > 0 {
			a.slots = make(chan struct{}, MaxInflight)
		}
	})
}

// acquire takes a slot, waiting up to AdmitWaitMS; release must be called when ok.
func (a *admission) acquire(r *http.Request) (release func(), ok bool) {
	a.init()
	if a.slots == nil {
		return func() {}, true
	}
	got := func() func() {
		a.inflight.Add(1)
		return func() { <-a.slots; a.inflight.Add(-1) }
	}
	select {
	case a.slots <- struct{}{}:
		return got(), true
	default:
	}
	t := time.NewTimer(time.Duration(AdmitWaitMS) * time.Millisecond)
	defer t.Stop()
	select {
	case a.slots <- struct{}{}:
		return got(), true
	case <-t.C:
		return nil, false
	case <-r.Context().Done():
		return nil, false
	}
}

// admitted wraps a tool-call surface with both gates.
func (f *Server) admitted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := f.admit.tenant.take(f.Tenant, 1, RateTenantCalls); !ok {
			f.admit.limited.Add(1)
			f.Security(r.Context(), "tenant_rate_limited")
			TooMany(w, wait, "tenant rate limited")
			return
		}
		release, ok := f.admit.acquire(r)
		if !ok {
			f.admit.shed.Add(1)
			f.Security(r.Context(), "overloaded", "inflight", f.admit.inflight.Load())
			w.Header().Set("Retry-After", strconv.Itoa(1))
			RestError(w, http.StatusServiceUnavailable, "overloaded, retry shortly")
			return
		}
		defer release()
		f.admit.admitted.Add(1)
		next.ServeHTTP(w, r)
	})
}
