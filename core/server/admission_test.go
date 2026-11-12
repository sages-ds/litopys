package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// TestAdmission: a call over the instance cap waits for a free slot and is shed with 503
// and Retry-After when none frees in time; a call over the tenant budget gets 429 with Retry-After; 0 disables
// both gates. No database: the gates sit in front of the handlers.
func TestAdmission(t *testing.T) {
	oldMax, oldWait, oldRate := MaxInflight, AdmitWaitMS, RateTenantCalls
	t.Cleanup(func() { MaxInflight, AdmitWaitMS, RateTenantCalls = oldMax, oldWait, oldRate })
	call := func(h http.Handler) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", nil))
		return w
	}
	newFacade := func() *Server {
		return &Server{Tenant: uuid.New(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	}

	// the cap: one slot held, the next call waits, then is shed; after the release it passes
	MaxInflight, AdmitWaitMS, RateTenantCalls = 1, 50, 0
	f := newFacade()
	hold, entered := make(chan struct{}), make(chan struct{})
	blocking := f.admitted(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-hold
	}))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); call(blocking) }()
	<-entered
	if w := call(f.admitted(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over the cap: %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if f.admit.inflight.Load() != 1 || f.admit.shed.Load() != 1 {
		t.Errorf("inflight %d, shed %d", f.admit.inflight.Load(), f.admit.shed.Load())
	}
	close(hold)
	wg.Wait()
	ok := f.admitted(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if w := call(ok); w.Code != http.StatusOK {
		t.Fatalf("after the release: %d", w.Code)
	}
	if f.admit.inflight.Load() != 0 || f.admit.admitted.Load() != 2 {
		t.Errorf("inflight %d, admitted %d", f.admit.inflight.Load(), f.admit.admitted.Load())
	}

	// a waiting call gets the slot freed while it waits
	MaxInflight, AdmitWaitMS = 1, 2000
	f = newFacade()
	hold, entered = make(chan struct{}), make(chan struct{})
	blocking = f.admitted(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-hold
	}))
	wg.Add(1)
	go func() { defer wg.Done(); call(blocking) }()
	<-entered
	go close(hold)
	if w := call(f.admitted(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))); w.Code != http.StatusOK {
		t.Fatalf("a waiting call: %d", w.Code)
	}
	wg.Wait()

	// the tenant budget
	MaxInflight, RateTenantCalls = 0, 3
	f = newFacade()
	ok = f.admitted(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	codes := map[int]int{}
	for i := 0; i < 5; i++ {
		w := call(ok)
		codes[w.Code]++
		if w.Code == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
	}
	if codes[http.StatusOK] != 3 || codes[http.StatusTooManyRequests] != 2 || f.admit.limited.Load() != 2 {
		t.Errorf("tenant budget 3 of 5 calls: %v, limited %d", codes, f.admit.limited.Load())
	}

	// both gates off
	MaxInflight, RateTenantCalls = 0, 0
	f = newFacade()
	ok = f.admitted(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for i := 0; i < 10; i++ {
		if w := call(ok); w.Code != http.StatusOK {
			t.Fatalf("gates off: %d", w.Code)
		}
	}
}
