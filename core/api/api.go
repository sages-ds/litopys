// Package api is the native API of Litopys v1: MCP tools <domain>_<verb> on /v1/mcp and
// POST /v1/<domain>/<verb>, both projections of domain/verbs.yaml through the generated
// gen/surface.json (domain/tools/gen.mjs). It is a thin dispatch over core/verbs and core/query; its
// own logic is limited to the *_resume family (resume.go), sessions by client (session.go) and
// onboarding texts (onboarding.go).
//
// Authentication is not done here: the process that mounts the API (the facade binary) passes its
// bearer middleware and a Resolver that turns a verified token into a Caller.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/executors"
	"litopys.smart-gw.co.ua/core/files"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/privacy"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
)

//go:embed gen/surface.json
var surfaceJSON []byte

// Tool is one entry of gen/surface.json.
type Tool struct {
	Name        string              `json:"name"`
	Verb        string              `json:"verb"`
	HTTP        string              `json:"http"`
	Kind        string              `json:"kind"` // command | query
	Auth        string              `json:"auth"` // owner | admin | consumer
	Profiles    []string            `json:"profiles"`
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Annotations mcp.ToolAnnotations `json:"annotations"`
	InputSchema json.RawMessage     `json:"inputSchema"`
	// TextFields: the text fields of a command, path -> class (textclass.go)
	TextFields map[string]string `json:"text_fields,omitempty"`
	// Executor: the variant an executor with a derived grant is listed — fewer parameters, a short text
	Executor *struct {
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	} `json:"executor,omitempty"`
}

// SurfaceDoc is gen/surface.json.
type SurfaceDoc struct {
	APIVersion  string `json:"api_version"`
	SpecVersion int    `json:"spec_version"`
	Tools       []Tool `json:"tools"`
	Hash        string `json:"surface_hash"`
}

var (
	surfaceOnce sync.Once
	surface     SurfaceDoc
	surfaceErr  error
)

// Surface is the generated surface (parsed once).
func Surface() (SurfaceDoc, error) {
	surfaceOnce.Do(func() {
		surfaceErr = json.Unmarshal(surfaceJSON, &surface)
		if surfaceErr == nil && len(surface.Tools) == 0 {
			surfaceErr = errors.New("api: empty surface")
		}
	})
	return surface, surfaceErr
}

// ForExecutor: the description and input schema an executor is given — the executor variant where the verb has one.
func (t Tool) ForExecutor() (string, json.RawMessage) {
	if t.Executor != nil {
		return t.Executor.Description, t.Executor.InputSchema
	}
	return t.Description, t.InputSchema
}

// Visible: is the tool listed (and callable) for this role and grant profile?
func (t Tool) Visible(role, profile string) bool {
	if profile == "" {
		profile = policy.ProfileFull
	}
	switch t.Auth {
	case "admin":
		return role == "admin" && profile != policy.ProfileReadonly && profile != "chat"
	case "consumer":
		return role == "admin" || role == "consumer"
	}
	return slices.Contains(t.Profiles, profile)
}

// Caller is the authenticated principal of one call; the edge resolves it (Resolver).
type Caller struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	Owner     uuid.UUID // root human: the context of the call carries it (RLS of 0008)
	Role      string
	Grant     policy.Grant
	OAuth     bool
	Email     string
	Name      string
	Client    string // declarative client header, if any
	MCPClient string // "name/version" of the MCP clientInfo (declarative, decides nothing)
	UserAgent string

	run uuid.UUID // the run of the session the call is bound to (session.go)
}

// ClientLabel names the client for the audit: its header, else its MCP clientInfo, else "" (as the facade 1.19).
func (c *Caller) ClientLabel() string {
	if c.Client != "" {
		return c.Client
	}
	return c.MCPClient
}

// ClientLabel: "name/version" of an MCP clientInfo, "" when there is none.
func ClientLabel(ci *mcp.Implementation) string {
	if ci == nil || ci.Name == "" {
		return ""
	}
	if ci.Version == "" {
		return ci.Name
	}
	return ci.Name + "/" + ci.Version
}

func (c *Caller) verbs() verbs.Principal {
	return verbs.Principal{TenantID: c.TenantID, ActorID: c.ActorID, RunID: c.run, Owner: c.Owner, Role: c.Role, Grant: c.Grant}
}

func (c *Caller) query() query.Principal {
	return query.Principal{TenantID: c.TenantID, ActorID: c.ActorID, Role: c.Role, Grant: c.Grant}
}

// profile of the call: the grant's, full when the token has none.
func (c *Caller) profile() string {
	if c.Grant.Profile == "" {
		return policy.ProfileFull
	}
	return c.Grant.Profile
}

// Resolver turns a verified bearer token (and the request headers) into the caller and a context that
// carries its owner and policy decision.
type Resolver func(ctx context.Context, ti *sdkauth.TokenInfo, h http.Header) (context.Context, Caller, error)

// DB is what the API reads directly (sessions, project resolution): the litopys_app pool.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// API holds the services of one process.
type API struct {
	V       *verbs.Service
	Q       *query.Service
	DB      DB
	Resolve Resolver
	Log     *slog.Logger
	// WriteBudget, when set, takes n bytes of the person's write budget (shared with the facade).
	WriteBudget func(owner uuid.UUID, n int) (ok bool, wait time.Duration)
	// Audit, when set, is called once per tool call (no arguments, no results); code is the error code of
	// a refused or failed call, "" when it succeeded.
	Audit func(ctx context.Context, tool string, c Caller, code string, d time.Duration)
	// Canary, when set, gives its actors the MCP surface with channel markers (canary.go).
	Canary *Canary
	// ClientSeen and ClientOf, when set, keep the clientInfo of a token's initialize for its later calls: the
	// stateless server gives every request a session of its own, so the old protocol's initialize never reaches a
	// tool call by itself.
	ClientSeen func(ti *sdkauth.TokenInfo, label string)
	ClientOf   func(ti *sdkauth.TokenInfo) string
	// Sealer seals and opens the provider keys of executors (LITOPYS_SECRET_KEY); nil — the app stores no keys.
	Sealer *executors.Sealer
	// RunnerSecret is the secret the runner sidecar sends with its owner token on /v1/runner/* (LITOPYS_RUNNER_SECRET);
	// empty — those endpoints are off. A chat client holds a token but never this secret.
	RunnerSecret string
	// Files keeps the bytes of attachments (LITOPYS_FILES_DIR); nil — attachments are off.
	Files *files.Store
}

// handler implements one verb.
type handler func(ctx context.Context, a *API, c *Caller, args json.RawMessage) (any, error)

var handlers = map[string]handler{} // verb name → implementation (verbs.go, resume.go)

func handle(verb string, h handler) { handlers[verb] = h }

// Error is what a caller sees: a stable code, a message and an optional hint.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	Ref     string `json:"ref,omitempty"`
	status  int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *Error { return errorf("invalid_input", format, args...) }

// statusOf maps a code to an HTTP status.
func statusOf(code string) int {
	switch code {
	case "unauthenticated":
		return http.StatusUnauthorized
	case "forbidden", "policy_denied", "not_in_profile", "secret_rejected", "retention_confirm_requires_human":
		return http.StatusForbidden
	case "rate_limited":
		return http.StatusTooManyRequests
	case "maintenance": // read-only for an update (0042)
		return http.StatusServiceUnavailable
	case "revision_conflict", "project_exists", "project_ambiguous", "statement_not_current", "label_changed", "impact_changed":
		return http.StatusConflict
	case "internal":
		return http.StatusInternalServerError
	}
	if strings.HasSuffix(code, "_not_found") {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

// public turns any error into what the caller may see; unknown errors are logged with a ref.
func (a *API) public(tool string, err error) *Error {
	var e *Error
	switch {
	case errors.As(err, &e):
	case verbs.Code(err) != "":
		var ve *verbs.Error
		errors.As(err, &ve)
		e = &Error{Code: ve.Code, Message: ve.Message}
	case query.Code(err) != "":
		var qe *query.Error
		errors.As(err, &qe)
		e = &Error{Code: qe.Code, Message: qe.Message}
	default:
		ref := uuid.NewString()[:8]
		a.Log.Error("api call failed", "tool", tool, "ref", ref, "err", err)
		e = &Error{Code: "internal", Message: "internal error", Ref: ref}
	}
	if e.status == 0 {
		e.status = statusOf(e.Code)
	}
	if e.Hint == "" {
		e.Hint = hints[e.Code]
	}
	return e
}

// hints: what a model should do next.
var hints = map[string]string{
	"project_not_found":   "call project_resume without arguments to see the projects, or pass auto_create: true",
	"project_ambiguous":   "several projects have this title: pass the slug (name) instead",
	"not_in_profile":      "this connection's profile does not include the tool; system_guide explains what to do instead",
	"session_not_found":   "call project_resume to open a session (system_guide start)",
	"task_not_found":      "task numbers are per project: task_list shows them (system_guide tasks)",
	"statement_not_found": "recall_search finds statement ids",
	"type_unknown":        "use one of the allowed types in the message; a vocabulary the project lacks is enabled by the user (fact kind vocabulary; system_guide record)",
	"deletion_not_found":  "deletion_list shows the proposals of a project (system_guide delete)",
	"type_ambiguous":      "pass the full name <vocabulary>.<type> of one of the candidates",
	"type_retired":        "use the type named in the message instead",
	"type_reserved":       "outcomes are written by session_close with outcomes; pick another type",
}

// call runs one tool for an authenticated request and audits it: the error code of a refused or failed
// call goes to the audit line (a code is not memory content), refusals before dispatch included.
func (a *API) call(ctx context.Context, t Tool, c *Caller, args json.RawMessage) (any, *Error) {
	start := time.Now()
	res, e := a.dispatch(ctx, t, c, args)
	if a.Audit != nil {
		code := ""
		if e != nil {
			code = e.Code
		}
		a.Audit(ctx, t.Name, *c, code, time.Since(start))
	}
	return res, e
}

// dispatch: profile check, secrets, write budget, the handler.
func (a *API) dispatch(ctx context.Context, t Tool, c *Caller, args json.RawMessage) (any, *Error) {
	if !t.Visible(c.Role, c.profile()) {
		return nil, a.public(t.Name, &Error{Code: "not_in_profile", Message: fmt.Sprintf("%s is not available for profile %s", t.Name, c.profile())})
	}
	h, ok := handlers[t.Verb]
	if !ok {
		return nil, a.public(t.Name, errorf("internal", "%s has no implementation", t.Verb))
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	if c.Grant.Derived {
		if err := derivedAllows(t, c.Grant, args); err != nil {
			return nil, a.public(t.Name, err)
		}
	}
	if t.Kind == "command" {
		if kind := secretIn(args); kind != "" {
			return nil, a.public(t.Name, &Error{Code: "secret_rejected",
				Message: "the input contains a secret (" + kind + ")", Hint: "replace it with a placeholder like ${VAR}: secrets live in environment variables"})
		}
		if a.WriteBudget != nil {
			if ok, wait := a.WriteBudget(c.Owner, len(args)); !ok {
				return nil, a.public(t.Name, &Error{Code: "rate_limited",
					Message: fmt.Sprintf("write budget exhausted, retry in %d s", int(wait.Seconds())+1)})
			}
		}
		args = cleanArgs(t, args)
	}
	res, err := h(ctx, a, c, args)
	if err != nil {
		return nil, a.public(t.Name, err)
	}
	return res, nil
}

// decode reads arguments strictly: unknown fields are an error that names them (the schema says
// additionalProperties: false, and a model learns from the message).
func decode(args json.RawMessage, v any) error {
	d := json.NewDecoder(strings.NewReader(string(args)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return invalid("%v", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

// secretIn: the first secret kind in any string of the arguments (private blocks are cut before saving).
func secretIn(args json.RawMessage) string {
	var v any
	if json.Unmarshal(args, &v) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(x any) string {
		switch t := x.(type) {
		case string:
			return privacy.DetectSecret(privacy.StripPrivate(t))
		case []any:
			for _, e := range t {
				if k := walk(e); k != "" {
					return k
				}
			}
		case map[string]any:
			for _, e := range t {
				if k := walk(e); k != "" {
					return k
				}
			}
		}
		return ""
	}
	return walk(v)
}

// ---------------------------------------------------------------------------
// MCP
// ---------------------------------------------------------------------------

func toolResult(v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"error":{"code":"internal","message":"result is not serializable"}}`)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: json.RawMessage(b)}
}

func toolError(e *Error) *mcp.CallToolResult {
	r := toolResult(map[string]any{"error": e})
	r.IsError = true
	return r
}

// server builds the MCP server of one role and profile; canary adds the channel markers (canary.go).
func (a *API) server(role, profile string, canary *Canary) (*mcp.Server, error) {
	doc, err := Surface()
	if err != nil {
		return nil, err
	}
	instructions := Instructions
	if canary != nil {
		instructions = canary.instructions()
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "litopys", Title: "Litopys", Version: doc.APIVersion + "+" + doc.Hash},
		&mcp.ServerOptions{Instructions: instructions})
	byName := map[string]Tool{}
	for _, t := range doc.Tools {
		if !t.Visible(role, profile) {
			continue
		}
		byName[t.Name] = t
		var schema map[string]any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			return nil, fmt.Errorf("surface %s: %w", t.Name, err)
		}
		ann := t.Annotations
		desc, marked := t.Description, canary != nil && t.Name == "system_whoami"
		if marked {
			desc = canary.description(desc)
		}
		s.AddTool(&mcp.Tool{Name: t.Name, Title: t.Title, Description: desc, InputSchema: schema, Annotations: &ann},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var ti *sdkauth.TokenInfo
				var h http.Header
				if req.Extra != nil {
					ti, h = req.Extra.TokenInfo, req.Extra.Header
				}
				ctx, c, err := a.Resolve(ctx, ti, h)
				if err != nil {
					a.Log.WarnContext(ctx, "security", "event", "v1_unresolved_caller", "tool", t.Name, "reason", err.Error())
					return toolError(&Error{Code: "unauthenticated", Message: "not authenticated"}), nil
				}
				// the new sessionless protocol carries clientInfo in every request; the old one said it once, in initialize
				if c.MCPClient = ClientLabel(req.ClientInfo()); c.MCPClient == "" && a.ClientOf != nil {
					c.MCPClient = a.ClientOf(ti)
				}
				var args json.RawMessage
				if req.Params != nil {
					args = req.Params.Arguments
				}
				res, e := a.call(ctx, t, &c, args)
				if e != nil {
					return toolError(e), nil
				}
				if marked {
					res = canary.result(res)
				}
				return toolResult(res), nil
			})
	}
	a.onboarding(s)
	// an executor with a derived grant is listed only what it may call — its step's verbs and its own step tools —
	// in their executor variant: the definitions of the whole profile would not fit a small model's
	// context. Calling an unlisted tool still answers why (not_in_grant), as before.
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			lt, ok := res.(*mcp.ListToolsResult)
			if method != "tools/list" || err != nil || !ok {
				return res, err
			}
			g, derived := derivedGrant(req)
			if !derived {
				return res, err
			}
			narrowed := *lt // keeps the cursor and _meta; the list is this principal's own
			narrowed.Tools = executorTools(lt.Tools, byName, g)
			narrowed.CacheScope = "private"
			return &narrowed, nil
		}
	})
	// initialize, tools/list, resources/* and prompts/* go to the audit log (which clients ask for
	// them; a client that connects but never calls a tool is diagnosable by what it did see)
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "initialize" || method == "tools/list" || strings.HasPrefix(method, "resources/") || strings.HasPrefix(method, "prompts/") {
				res, err := next(ctx, method, req)
				kv := []any{"method", method, "role", role, "profile", profile, "ok", err == nil}
				if lt, ok := res.(*mcp.ListToolsResult); ok {
					kv = append(kv, "tools", len(lt.Tools))
				}
				if ip, ok := req.GetParams().(*mcp.InitializeParams); ok && ip != nil && ip.ClientInfo != nil {
					kv = append(kv, "client", ip.ClientInfo.Name, "client_version", ip.ClientInfo.Version)
					if x := req.GetExtra(); a.ClientSeen != nil && x != nil && x.TokenInfo != nil {
						a.ClientSeen(x.TokenInfo, ClientLabel(ip.ClientInfo))
					}
				}
				a.Log.InfoContext(ctx, "api_method", kv...)
				return res, err
			}
			return next(ctx, method, req)
		}
	})
	return s, nil
}

// roles and profiles that get their own MCP server
var (
	roles    = []string{"admin", "owner", "consumer"}
	profiles = []string{policy.ProfileFull, "core", "chat", policy.ProfileReadonly}
)

// MCP is the Streamable HTTP handler of /v1/mcp (stateless, JSON responses — like the facade). The
// bearer middleware of the mounting process must run before it.
func (a *API) MCP() (http.Handler, error) {
	servers := map[string]*mcp.Server{}
	for _, r := range roles {
		for _, p := range profiles {
			s, err := a.server(r, p, nil)
			if err != nil {
				return nil, err
			}
			servers[r+"/"+p] = s
			if a.Canary != nil {
				if s, err = a.server(r, p, a.Canary); err != nil {
					return nil, err
				}
				servers[r+"/"+p+"/canary"] = s
			}
		}
	}
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		role, profile, suffix := "owner", policy.ProfileFull, ""
		if ti := sdkauth.TokenInfoFromContext(r.Context()); ti != nil {
			if p, ok := ti.Extra["principal"].(auth.Principal); ok { // set by the bearer verifier of the facade
				if p.Role == "admin" || p.Role == "consumer" {
					role = p.Role
				}
				if p.Grant.Profile != "" {
					profile = p.Grant.Profile
				}
				if a.Canary.For(p) {
					suffix = "/canary"
				}
			}
		}
		if s, ok := servers[role+"/"+profile+suffix]; ok {
			return s
		}
		return servers["owner/"+policy.ProfileReadonly] // an unknown profile gets the narrowest surface
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}), nil
}

// HTTP is POST /v1/<domain>/<verb> with a JSON body; same tools, same checks as MCP.
func (a *API) HTTP() (http.Handler, error) {
	doc, err := Surface()
	if err != nil {
		return nil, err
	}
	byPath := map[string]Tool{}
	for _, t := range doc.Tools {
		byPath[t.HTTP] = t
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(status int, v any) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		if strings.HasPrefix(r.URL.Path, "/v1/runner/") {
			a.runnerHTTP(w, r, write)
			return
		}
		t, ok := byPath[r.URL.Path]
		if !ok || r.Method != http.MethodPost {
			write(http.StatusNotFound, map[string]any{"error": &Error{Code: "unknown_verb", Message: r.Method + " " + r.URL.Path,
				Hint: "POST /v1/<domain>/<verb>; system_capabilities lists the verbs"}})
			return
		}
		ctx, c, err := a.Resolve(r.Context(), sdkauth.TokenInfoFromContext(r.Context()), r.Header)
		if err != nil {
			a.Log.WarnContext(ctx, "security", "event", "v1_unresolved_caller", "tool", t.Name, "reason", err.Error())
			write(http.StatusUnauthorized, map[string]any{"error": &Error{Code: "unauthenticated", Message: "not authenticated"}})
			return
		}
		body := http.MaxBytesReader(w, r.Body, 256<<10)
		var raw json.RawMessage
		if err := json.NewDecoder(body).Decode(&raw); err != nil && err.Error() != "EOF" {
			write(http.StatusBadRequest, map[string]any{"error": invalid("body: %v", err)})
			return
		}
		res, e := a.call(ctx, t, &c, raw)
		if e != nil {
			write(e.status, map[string]any{"error": e})
			return
		}
		write(http.StatusOK, res)
	}), nil
}

// derivedGrant: the grant of the request's principal, when it is a derived (executor's) one.
func derivedGrant(req mcp.Request) (policy.Grant, bool) {
	x := req.GetExtra()
	if x == nil || x.TokenInfo == nil {
		return policy.Grant{}, false
	}
	p, ok := x.TokenInfo.Extra["principal"].(auth.Principal)
	if !ok || !p.Grant.Derived {
		return policy.Grant{}, false
	}
	return p.Grant, true
}

// executorTools keeps the tools a derived grant may call (derivedAllows without arguments) and puts their executor
// variant in place, where the verb has one.
func executorTools(listed []*mcp.Tool, byName map[string]Tool, g policy.Grant) []*mcp.Tool {
	out := make([]*mcp.Tool, 0, len(listed))
	for _, lt := range listed {
		t, ok := byName[lt.Name]
		if !ok || derivedAllows(t, g, json.RawMessage(`{}`)) != nil {
			continue
		}
		if t.Executor != nil {
			desc, raw := t.ForExecutor()
			var schema map[string]any
			if json.Unmarshal(raw, &schema) == nil {
				narrowed := *lt
				narrowed.Description, narrowed.InputSchema = desc, schema
				lt = &narrowed
			}
		}
		out = append(out, lt)
	}
	return out
}

// stepOwn: what a derived grant may always call for its own step, besides the step's verbs.
var stepOwn = []string{"step.start", "step.checkpoint", "step.finish", "step.pack", "execution.read", "system.whoami"}

// derivedAllows: a derived grant calls only its step's verbs and its own step tools, only on its run and
// step, and in candidate mode writes nothing but the step output (until the review queue).
func derivedAllows(t Tool, g policy.Grant, args json.RawMessage) *Error {
	if !slices.Contains(stepOwn, t.Verb) && !slices.Contains(g.Verbs, t.Verb) {
		return &Error{Code: "not_in_grant", Message: fmt.Sprintf("%s is not among the verbs of step %s", t.Name, g.StepKey),
			Hint: "an executor's grant covers its step only: step_pack lists what the step allows"}
	}
	if g.WriteMode == "candidate" && t.Kind == "command" && !slices.Contains([]string{"step.start", "step.checkpoint", "step.finish", "step.delegate"}, t.Verb) {
		return &Error{Code: "candidate_only", Message: "this step writes candidates: put the result into the step output (step_finish)"}
	}
	var bound struct {
		ExecutionID *uuid.UUID `json:"execution_id"`
		StepKey     *string    `json:"step_key"`
	}
	_ = json.Unmarshal(args, &bound)
	if bound.ExecutionID != nil && (g.ExecutionID == nil || *bound.ExecutionID != *g.ExecutionID) ||
		bound.StepKey != nil && *bound.StepKey != g.StepKey {
		return &Error{Code: "outside_grant", Message: fmt.Sprintf("this grant covers step %s of run %v only", g.StepKey, g.ExecutionID)}
	}
	return nil
}
