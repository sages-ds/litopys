package server

// Edge of the facade: WWW-Authenticate on 401, the 401 limiter that never
// blocks a valid token and actionable database errors.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"litopys.smart-gw.co.ua/core/auth"
)

// Limits of failed authentications per minute: per (ip, token fingerprint) and, for requests
// without a token (OAuth discovery, misconfigured clients), a softer bucket per ip.
var (
	UnauthorizedPerKey   = 10
	UnauthorizedNoToken  = 60
	unauthorizedInterval = time.Minute
)

type unauthorizedLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newUnauthorizedLimiter() *unauthorizedLimiter {
	return &unauthorizedLimiter{hits: map[string][]time.Time{}}
}

func (l *unauthorizedLimiter) recent(key string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.hits[key] {
		if now.Sub(t) < unauthorizedInterval {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = keep
	}
	return keep
}

func (l *unauthorizedLimiter) blocked(key string, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, time.Now())) >= limit
}

func (l *unauthorizedLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.hits[key] = append(l.recent(key, now), now)
}

// authKey: (ip, sha256(token)[:12]) — a valid token fails never, so its key never fills up; a shared
// vendor egress ip with someone else's expired tokens does not block it either.
func authKey(r *http.Request) (key string, limit int, hasToken bool) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) == 2 && strings.EqualFold(fields[0], "bearer") {
		h := sha256.Sum256([]byte(fields[1]))
		return ip + "|" + hex.EncodeToString(h[:])[:12], UnauthorizedPerKey, true
	}
	return ip + "|-", UnauthorizedNoToken, false
}

type statusWriter struct {
	http.ResponseWriter
	status   int
	hasToken bool
	expired  func() bool // asked only on a 401 with a token
	extra    string      // resource_metadata / scope when OAuth is on
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	if code == http.StatusUnauthorized {
		v := `Bearer realm="litopys"` + w.extra // resource_metadata only when OAuth is on: static-header connectors would switch to an OAuth flow
		if w.hasToken {
			v += `, error="invalid_token"`
			if w.expired != nil && w.expired() {
				v += `, error_description="expired"`
			}
		}
		w.Header().Set("WWW-Authenticate", v)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() { // SSE through the edge
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// edge wraps a bearer-protected handler: an over-limit key gets 429 before verification; a 401 is
// counted only after it happened.
func (f *Server) edge(l *unauthorizedLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, limit, hasToken := authKey(r)
		if l.blocked(key, limit) {
			f.Security(r.Context(), "unauthorized_burst", "key", key[:strings.IndexByte(key, '|')])
			http.Error(w, "too many failed authentications", http.StatusTooManyRequests)
			return
		}
		sw := &statusWriter{ResponseWriter: w, hasToken: hasToken, extra: f.oauthHeader(r.URL.Path), expired: func() bool {
			fields := strings.Fields(r.Header.Get("Authorization"))
			return len(fields) == 2 && auth.Expired(r.Context(), f.DB, fields[1])
		}}
		next.ServeHTTP(sw, r)
		if sw.status == http.StatusUnauthorized {
			l.fail(key)
		}
	})
}

// pgMessage: database conditions the caller can act on; "" for everything else.
func PgMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "запит задовгий — звузьте фільтри"
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "22P02", "22007", "22008", "22003":
			return "некоректне значення параметра: " + pe.Message
		case "23505":
			return "такий запис уже існує"
		case "40P01", "40001", "55P03":
			return "тимчасовий конфлікт — повторіть"
		case "57014":
			return "запит задовгий — звузьте фільтри"
		case "53300", "57P01", "57P03":
			return "сервіс тимчасово недоступний — повторіть пізніше"
		}
		return ""
	}
	var ne *net.OpError
	if errors.As(err, &ne) || strings.Contains(err.Error(), "connection refused") {
		return "сервіс тимчасово недоступний — повторіть пізніше"
	}
	return ""
}

func NewRef() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
