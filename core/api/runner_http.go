package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"

	"litopys.smart-gw.co.ua/internal/pg"
)

// The runner sidecar's endpoints, not tools: a chat client holds an owner token too, so every
// request needs both the owner's token and LITOPYS_RUNNER_SECRET in X-Litopys-Runner — only the runner next to the app
// has the second. GET /v1/runner/config gives the owner's live executors with their keys opened; POST
// /v1/runner/heartbeat records what the runner saw (auth.runner_status) for the status page.

// RunnerExecutor is one executor as the runner takes it.
type RunnerExecutor struct {
	ActorID   uuid.UUID      `json:"actor_id"`
	Name      string         `json:"name"`
	Adapter   string         `json:"adapter"`
	BaseURL   string         `json:"base_url"`
	Model     string         `json:"model"`
	Limits    map[string]int `json:"limits,omitempty"`
	Key       string         `json:"key,omitempty"`        // opened from auth.executor_key
	SecretRef string         `json:"secret_ref,omitempty"` // a key the runner reads itself (OpenBao, as on the stand)
}

type runnerBeat struct {
	Runner    string            `json:"runner"`
	Version   string            `json:"version,omitempty"`
	Executors []json.RawMessage `json:"executors"`
}

func (a *API) runnerHTTP(w http.ResponseWriter, r *http.Request, write func(int, any)) {
	if a.RunnerSecret == "" {
		write(http.StatusNotFound, map[string]any{"error": &Error{Code: "unknown_verb", Message: "runner endpoints are off (LITOPYS_RUNNER_SECRET)"}})
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Litopys-Runner")), []byte(a.RunnerSecret)) != 1 {
		a.Log.WarnContext(r.Context(), "security", "event", "runner_secret_mismatch", "path", r.URL.Path)
		write(http.StatusForbidden, map[string]any{"error": &Error{Code: "forbidden", Message: "not the runner"}})
		return
	}
	ctx, c, err := a.Resolve(r.Context(), sdkauth.TokenInfoFromContext(r.Context()), r.Header)
	if err != nil || c.Grant.Derived || c.OAuth {
		write(http.StatusUnauthorized, map[string]any{"error": &Error{Code: "unauthenticated", Message: "the runner's own owner token (LTP_) is required"}})
		return
	}
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runner/config":
		rows, err := a.DB.Query(sys, `
			SELECT a.actor_id, a.display_name, a.executor, k.nonce, k.sealed
			  FROM projection.actor a LEFT JOIN auth.executor_key k ON k.actor_id = a.actor_id
			 WHERE a.tenant_id = $1 AND litopys_root(a.actor_id) = $2 AND a.kind IN ('agent', 'service')
			   AND a.revoked_at IS NULL AND a.executor IS NOT NULL AND NOT COALESCE((a.executor->>'disabled')::boolean, false)
			 ORDER BY a.display_name`, c.TenantID, c.Owner)
		if err != nil {
			write(http.StatusInternalServerError, map[string]any{"error": &Error{Code: "internal", Message: "internal error"}})
			return
		}
		defer rows.Close()
		out := []RunnerExecutor{}
		for rows.Next() {
			var x RunnerExecutor
			var profile []byte
			var nonce, sealed []byte
			if err := rows.Scan(&x.ActorID, &x.Name, &profile, &nonce, &sealed); err != nil {
				write(http.StatusInternalServerError, map[string]any{"error": &Error{Code: "internal", Message: "internal error"}})
				return
			}
			var p struct {
				Adapter   string         `json:"adapter"`
				BaseURL   string         `json:"base_url"`
				Model     string         `json:"model"`
				SecretRef string         `json:"secret_ref"`
				Limits    map[string]int `json:"limits"`
			}
			if json.Unmarshal(profile, &p) != nil || p.BaseURL == "" {
				continue // a profile without an address is not runnable from here (the old file config covers it)
			}
			if p.Adapter == "" {
				p.Adapter = "openai"
			}
			x.Adapter, x.BaseURL, x.Model, x.Limits, x.SecretRef = p.Adapter, p.BaseURL, p.Model, p.Limits, p.SecretRef
			if len(sealed) > 0 && a.Sealer != nil {
				key, err := a.Sealer.Open(nonce, sealed, x.ActorID[:])
				if err != nil {
					a.Log.WarnContext(ctx, "runner config: a key does not open", "executor", x.ActorID)
					continue
				}
				x.Key = key
			}
			out = append(out, x)
		}
		if a.Audit != nil {
			a.Audit(ctx, "runner config", c, "", 0)
		}
		write(http.StatusOK, map[string]any{"executors": out, "server_time": time.Now().UTC()})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runner/heartbeat":
		var b runnerBeat
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b); err != nil ||
			strings.TrimSpace(b.Runner) == "" || len(b.Runner) > 100 || len(b.Version) > 60 || len(b.Executors) > 50 {
			write(http.StatusBadRequest, map[string]any{"error": invalid("heartbeat: {runner, version, executors[≤50]}")})
			return
		}
		ex, _ := json.Marshal(b.Executors)
		var seen time.Time
		if err := a.DB.QueryRow(sys, `
			INSERT INTO auth.runner_status (tenant_id, owner_id, runner, version, executors) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, owner_id, runner) DO UPDATE SET version = $4, executors = $5, seen_at = now()
			RETURNING seen_at`, c.TenantID, c.Owner, b.Runner, b.Version, ex).Scan(&seen); err != nil {
			write(http.StatusInternalServerError, map[string]any{"error": &Error{Code: "internal", Message: "internal error"}})
			return
		}
		write(http.StatusOK, map[string]any{"ok": true})
	default:
		write(http.StatusNotFound, map[string]any{"error": &Error{Code: "unknown_verb", Message: r.Method + " " + r.URL.Path,
			Hint: "GET /v1/runner/config, POST /v1/runner/heartbeat"}})
	}
}
