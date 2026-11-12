// Package server is the HTTP server of Litopys: the native API v1, the knowledge viewer, OAuth, limits and admission,
// health and metrics.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"

	"litopys.smart-gw.co.ua/core/api"
	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/executors"
	"litopys.smart-gw.co.ua/core/files"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
)

// Wraps are the middlewares of the server; a route combines the ones it needs.
type Wraps struct {
	Bearer, RateLimited, Admitted func(http.Handler) http.Handler
}

// Server holds the services a request needs.
type Server struct {
	DB     *pgxpool.Pool // litopys_app
	Admin  *pgxpool.Pool // litopys_admin (journal.purge); may be nil
	V      *verbs.Service
	Q      *query.Service
	Tenant uuid.UUID
	Log    *slog.Logger
	OAuth  *OAuthConfig // nil: static tokens only (oauth.go)
	// Canary: channel markers of /v1/mcp for listed actors (api/canary.go); nil: off
	Canary *api.Canary
	// Sealer of executors' provider keys (LITOPYS_SECRET_KEY) and the runner sidecar's secret
	Sealer       *executors.Sealer
	RunnerSecret string
	// Files: the data directory of attachments (LITOPYS_FILES_DIR); nil — off
	Files *files.Store

	calls, writes window    // per-person limits (limits.go)
	roots         owners    // actor -> root human of the rate limiter
	admit         admission // global admission control (admission.go)
	seen          clients   // clientInfo of the last initialize per token (clients.go)
}

// New is a server over the pools; the optional services are set on the result.
func New(db, admin *pgxpool.Pool, tenant uuid.UUID) *Server {
	s := &Server{DB: db, Admin: admin, Tenant: tenant, Q: &query.Service{DB: db}, Log: slog.Default()}
	s.V = &verbs.Service{DB: db}
	if admin != nil {
		s.V.Admin = admin
	}
	return s
}

// Caller is the authenticated principal of one request.
type Caller struct {
	auth.Principal
	Owner       uuid.UUID // root human; the context of the call carries it as litopys.owner
	Email, Name string
	Client      string // X-Litopys-Client (declarative)
	UserAgent   string
}

// Security writes a security event as a structured log line for the SIEM track.
func (s *Server) Security(ctx context.Context, event string, kv ...any) {
	s.Log.WarnContext(ctx, "security", append([]any{"event", event}, kv...)...)
}

// CallerOf resolves a request carrying a verified token and returns the context marked with its root human: every
// query of the call runs under that owner (RLS of 0008).
func (s *Server) CallerOf(ctx context.Context, ti *sdkauth.TokenInfo, h http.Header) (context.Context, Caller, error) {
	var c Caller
	if ti == nil {
		return ctx, c, errors.New("no token")
	}
	p, ok := ti.Extra["principal"].(auth.Principal)
	if !ok {
		return ctx, c, errors.New("no principal")
	}
	c.Principal = p
	owner, err := verbs.Owner(ctx, s.DB, verbs.Principal{TenantID: p.TenantID, ActorID: p.ActorID})
	if err != nil {
		return ctx, c, err
	}
	c.Owner = owner
	ctx = pg.WithOwner(ctx, owner)
	// the policy decision of the call: the server's own SQL runs under it as well (0010)
	d, err := policy.Decide(ctx, s.DB, p.TenantID, owner, p.Grant)
	if err != nil {
		return ctx, c, err
	}
	ctx = policy.Scoped(ctx, d)
	if h != nil {
		c.Client = strings.TrimSpace(h.Get("X-Litopys-Client"))
		c.UserAgent = strings.TrimSpace(h.Get("User-Agent"))
	}
	var ext *string
	err = s.DB.QueryRow(ctx, `SELECT display_name, external_identity FROM projection.actor WHERE actor_id = $1`, p.ActorID).Scan(&c.Name, &ext)
	if err != nil {
		return ctx, c, err
	}
	if ext != nil {
		c.Email = *ext
	}
	return ctx, c, nil
}

// TakeWrite charges written bytes to a person's budget.
func (s *Server) TakeWrite(owner uuid.UUID, n int) (bool, time.Duration) {
	return s.writes.take(owner, n, RateWriteKB*1024)
}

// RememberClient keeps what a client said in initialize for its later calls (clients.go).
func (s *Server) RememberClient(token uuid.UUID, label string) { s.seen.remember(token, label) }

// ClientOf is the label RememberClient kept for a token.
func (s *Server) ClientOf(token uuid.UUID) string { return s.seen.of(token) }

// Handler is the HTTP surface.
func (s *Server) Handler() (http.Handler, error) {
	verifier := func(ctx context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		if s.OAuth != nil && auth.LooksLikeJWT(token) { // everything that is not LTP_ is an OAuth token
			p, exp, err := s.oauthPrincipal(ctx, token)
			if err != nil {
				if !errors.Is(err, errOAuthRefused) { // refusals log their own event; this is a bad or expired token
					s.Security(ctx, "oauth_token_invalid", "reason", err.Error())
				}
				return nil, sdkauth.ErrInvalidToken
			}
			return &sdkauth.TokenInfo{UserID: p.ActorID.String(), Expiration: exp, Extra: map[string]any{"principal": p}}, nil
		}
		p, err := auth.Verify(ctx, s.DB, token)
		if err != nil {
			return nil, sdkauth.ErrInvalidToken
		}
		if p.TenantID != s.Tenant {
			return nil, sdkauth.ErrInvalidToken
		}
		return &sdkauth.TokenInfo{UserID: p.ActorID.String(), Extra: map[string]any{"principal": p}}, nil
	}
	sdkBearer := sdkauth.RequireBearerToken(verifier, &sdkauth.RequireBearerTokenOptions{AllowMissingExpiration: true})
	limiter := newUnauthorizedLimiter()
	w := Wraps{Bearer: func(h http.Handler) http.Handler { return s.edge(limiter, sdkBearer(h)) },
		RateLimited: s.rateLimited, Admitted: s.admitted}
	mux := http.NewServeMux()
	// the native API v1: same tokens, limits and audit, its own surface
	native := s.Native()
	nativeMCP, err := native.MCP()
	if err != nil {
		return nil, err
	}
	nativeHTTP, err := native.HTTP()
	if err != nil {
		return nil, err
	}
	mux.Handle("/v1/mcp", w.Bearer(w.RateLimited(w.Admitted(nativeMCP))))
	mux.Handle("/v1/{domain}/{verb}", w.Bearer(w.RateLimited(w.Admitted(nativeHTTP))))
	// the knowledge viewer: read only, the same token check, a cookie instead of a header
	var viewerOIDC *api.ViewerOIDC
	if s.OAuth != nil && s.OAuth.OIDC != nil { // Keycloak sign-in of the viewer (P6c): a public client with PKCE
		cid := os.Getenv("LITOPYS_VIEWER_CLIENT_ID")
		if cid == "" {
			cid = "litopys-viewer"
		}
		viewerOIDC = &api.ViewerOIDC{Issuer: s.OAuth.OIDC.Issuer, ClientID: cid, Scope: "openid litopys:core",
			Redirect: s.OAuth.base() + "/viewer/callback"}
	}
	mux.Handle("/viewer/", native.Viewer(verifier, viewerOIDC))
	mux.HandleFunc("GET /version", s.version)
	if s.OAuth != nil { // RFC 9728 on both paths
		mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResource)
		mux.HandleFunc("GET /.well-known/oauth-protected-resource/v1/mcp", s.protectedResource)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.Ping(r.Context()); err != nil {
			http.Error(w, "db", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /metrics", s.metrics) // aggregates only, for monitoring
	return mux, nil
}

// rateLimited: MCP requests per person and minute — HTTP 429 with Retry-After.
func (s *Server) rateLimited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ti := sdkauth.TokenInfoFromContext(r.Context()); ti != nil {
			if p, ok := ti.Extra["principal"].(auth.Principal); ok {
				if ok, wait := s.calls.take(s.roots.of(r, s, p.ActorID), 1, RateCalls); !ok {
					s.Security(r.Context(), "rate_limited", "actor", p.ActorID)
					TooMany(w, wait, "rate limited")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Native is the native API v1 over this server's services: its tokens, owner and policy resolution, per-person write
// budget and audit line.
func (s *Server) Native() *api.API {
	return &api.API{V: s.V, Q: s.Q, DB: s.DB, Log: s.Log, Canary: s.Canary, Sealer: s.Sealer, RunnerSecret: s.RunnerSecret, Files: s.Files,
		Resolve: func(ctx context.Context, ti *sdkauth.TokenInfo, h http.Header) (context.Context, api.Caller, error) {
			ctx, c, err := s.CallerOf(ctx, ti, h)
			if err != nil {
				return ctx, api.Caller{}, err
			}
			return ctx, api.Caller{TenantID: c.TenantID, ActorID: c.ActorID, Owner: c.Owner, Role: c.Role, Grant: c.Grant,
				OAuth: c.OAuth, Email: c.Email, Name: c.Name, Client: c.Client, UserAgent: c.UserAgent}, nil
		},
		WriteBudget: s.TakeWrite,
		ClientSeen: func(ti *sdkauth.TokenInfo, label string) {
			if ti == nil {
				return
			}
			if p, ok := ti.Extra["principal"].(auth.Principal); ok {
				s.seen.remember(p.TokenID, label)
			}
		},
		ClientOf: func(ti *sdkauth.TokenInfo) string {
			if ti == nil {
				return ""
			}
			if p, ok := ti.Extra["principal"].(auth.Principal); ok {
				return s.seen.of(p.TokenID)
			}
			return ""
		},
		Audit: func(ctx context.Context, tool string, c api.Caller, code string, d time.Duration) {
			attrs := []any{"api", "v1", "tool", tool, "actor", c.ActorID, "client", c.ClientLabel(), "oauth", c.OAuth,
				"vendor", c.Grant.Vendor, "profile", c.Grant.Profile, "ok", code == "", "ms", d.Milliseconds()}
			if code != "" { // the code only, never the message: messages may quote the caller's input
				attrs = append(attrs, "code", code)
			}
			s.Log.InfoContext(ctx, "call", attrs...)
		},
	}
}

// WriteJSON answers with a JSON document.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// RestError answers with {"error": msg}.
func RestError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

var startedAt = time.Now().UTC().Format(time.RFC3339Nano)

// GET /version: which build runs, no token.
func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, s.ServerInfo(r.Context()))
}

// ServerInfo: which build serves (GET /version and the server block of whoami, contract 1.19).
func (s *Server) ServerInfo(ctx context.Context) map[string]any {
	out := map[string]any{"commit": nil, "committedAt": nil, "migration": nil, "startedAt": startedAt, "go": runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, st := range bi.Settings {
			switch st.Key {
			case "vcs.revision":
				out["commit"] = st.Value
			case "vcs.time":
				out["committedAt"] = st.Value
			}
		}
	}
	var v *int
	if err := s.DB.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v); err == nil && v != nil {
		out["migration"] = strconv.Itoa(*v)
		if all, err := migrate.List(); err == nil {
			for _, m := range all {
				if m.Version == *v {
					out["migration"] = m.Name
				}
			}
		}
	}
	return out
}

// ServerVersion: the build's short commit instead of a fixed version; "dev" without VCS info.
func ServerVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, st := range bi.Settings {
			if st.Key == "vcs.revision" && len(st.Value) >= 7 {
				return st.Value[:7]
			}
		}
	}
	return "dev"
}

// metrics: Prometheus text with aggregates only — no names, no content. An alert on
// litopys_projector_lag > 0 for minutes or litopys_up == 0 covers the failures that matter.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx := pg.WithScope(r.Context(), pg.ScopeSystem)
	up := 1
	var head, applied int64
	var version string
	if err := s.DB.QueryRow(ctx, `SELECT h.last_seq, COALESCE(ps.last_seq, 0), COALESCE(ps.version, '')
	        FROM journal_head h LEFT JOIN projection.projector_state ps ON ps.tenant_id = h.tenant_id
	       WHERE h.tenant_id = $1`, s.Tenant).Scan(&head, &applied, &version); err != nil {
		up = 0
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE litopys_up gauge\nlitopys_up %d\n", up)
	fmt.Fprintf(w, "# TYPE litopys_journal_head gauge\nlitopys_journal_head %d\n", head)
	fmt.Fprintf(w, "# TYPE litopys_projector_lag gauge\nlitopys_projector_lag %d\n", head-applied)
	fmt.Fprintf(w, "# TYPE litopys_projector_version_ok gauge\nlitopys_projector_version_ok %d\n", map[bool]int{true: 1, false: 0}[version == projector.Version])
	fmt.Fprintf(w, "# TYPE litopys_info gauge\nlitopys_info{version=%q} 1\n", ServerVersion())
	// admission control (admission.go): shed = refused for lack of a slot, limited = over the tenant budget
	fmt.Fprintf(w, "# TYPE litopys_inflight gauge\nlitopys_inflight %d\n", s.admit.inflight.Load())
	fmt.Fprintf(w, "# TYPE litopys_admitted_total counter\nlitopys_admitted_total %d\n", s.admit.admitted.Load())
	fmt.Fprintf(w, "# TYPE litopys_shed_total counter\nlitopys_shed_total %d\n", s.admit.shed.Load())
	fmt.Fprintf(w, "# TYPE litopys_tenant_limited_total counter\nlitopys_tenant_limited_total %d\n", s.admit.limited.Load())
}

// Human is one person of the directory.
type Human struct {
	ID        uuid.UUID
	Email     string
	Name      string
	CreatedAt time.Time
	Active    bool
	Role      string // admin if any active admin token, else user
}

// Humans lists the people of the tenant, or the one with this e-mail.
func (s *Server) Humans(ctx context.Context, email string) ([]Human, error) {
	// the directory spans people: admin scope reads actors and their registration events (RLS of 0008)
	rows, err := s.DB.Query(pg.WithScope(ctx, pg.ScopeAdmin), `
		SELECT a.actor_id, COALESCE(a.external_identity, ''), a.display_name, j.recorded_at,
		       EXISTS (SELECT 1 FROM auth.token t WHERE t.actor_id = a.actor_id AND t.revoked_at IS NULL),
		       CASE WHEN EXISTS (SELECT 1 FROM auth.token t WHERE t.actor_id = a.actor_id AND t.revoked_at IS NULL AND t.role = 'admin')
		            THEN 'admin' ELSE 'user' END
		  FROM projection.actor a JOIN journal j ON j.tenant_id = a.tenant_id AND j.seq = a.registered_seq
		 WHERE a.tenant_id = $1 AND a.kind = 'human' AND ($2 = '' OR a.external_identity = $2)
		 ORDER BY j.recorded_at`, s.Tenant, email)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Human, error) {
		var h Human
		return h, r.Scan(&h.ID, &h.Email, &h.Name, &h.CreatedAt, &h.Active, &h.Role)
	})
}

// StartSweeps runs the daily retention pass of execution events until ctx ends.
func (s *Server) StartSweeps(ctx context.Context, ttlDays int) {
	go func() {
		hour := time.NewTicker(time.Hour)
		defer hour.Stop()
		lastTTL := time.Time{}
		for {
			if time.Since(lastTTL) >= 24*time.Hour {
				if n, err := s.SweepExecutionEvents(ctx, ttlDays); err != nil {
					s.Log.Error("ttl sweep", "err", err)
				} else if n > 0 {
					s.Log.Info("ttl sweep", "removed", n, "days", ttlDays)
				}
				lastTTL = time.Now()
			}
			select {
			case <-ctx.Done():
				return
			case <-hour.C:
			}
		}
	}()
}

// SweepExecutionEvents deletes execution events older than ttlDays.
func (s *Server) SweepExecutionEvents(ctx context.Context, ttlDays int) (int64, error) {
	tag, err := s.DB.Exec(pg.WithScope(ctx, pg.ScopeSystem),
		`DELETE FROM execution.event WHERE recorded_at < now() - make_interval(days => $1)`, ttlDays)
	return tag.RowsAffected(), err
}
