package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/executors"
	"litopys.smart-gw.co.ua/core/projector"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
)

// The status page and the executors' settings: what the app and its runners are doing, and the
// executors a person adds from vendor presets — with the key entered here (never through a chat) and the access
// widened only by the person's explicit consent (owner's decision 2026-11-01). Both need the Keycloak sign-in, as the
// retention confirmation: a token of the form could be a model's.

// viewerPerson resolves the person of a Keycloak session; not signed in that way — an explanation or the sign-in.
func (a *API) viewerPerson(verify TokenVerifier, oidc *ViewerOIDC, w http.ResponseWriter, r *http.Request, why string) (context.Context, *Caller, string, bool) {
	ck, err := r.Cookie(viewerSessionCookie)
	if oidc == nil || err != nil {
		w.WriteHeader(http.StatusForbidden)
		_ = page(w, "error", map[string]any{"Code": "needs_sign_in", "Error": why})
		return nil, nil, "", false
	}
	if token, err := oidc.access(r.Context(), ck.Value); err == nil {
		if ti, verr := verify(r.Context(), token, r); verr == nil {
			if ctx, c, verr := a.Resolve(r.Context(), ti, r.Header); verr == nil {
				return ctx, &c, ck.Value, true
			}
		}
	}
	http.Redirect(w, r, "/viewer/login", http.StatusSeeOther)
	return nil, nil, "", false
}

// executorRow is one executor on the settings page.
type executorRow struct {
	ActorID    uuid.UUID
	Name       string
	P          verbs.ExecutorProfile
	Preset     executors.Preset
	KeySet     bool
	CheckedAt  *time.Time
	CheckOK    *bool
	CheckNote  *string
	Widened    bool
	AccessText string
}

func (a *API) executorRows(ctx context.Context, c *Caller) ([]executorRow, error) {
	rows, err := a.DB.Query(pg.WithScope(ctx, pg.ScopeSystem), `
		SELECT a.actor_id, a.display_name, a.executor, k.actor_id IS NOT NULL, k.checked_at, k.check_ok, k.check_note
		  FROM projection.actor a LEFT JOIN auth.executor_key k ON k.actor_id = a.actor_id
		 WHERE a.tenant_id = $1 AND litopys_root(a.actor_id) = $2 AND a.kind IN ('agent', 'service')
		   AND a.revoked_at IS NULL AND a.executor IS NOT NULL
		 ORDER BY a.display_name`, c.TenantID, c.Owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []executorRow
	for rows.Next() {
		var x executorRow
		var raw []byte
		if err := rows.Scan(&x.ActorID, &x.Name, &raw, &x.KeySet, &x.CheckedAt, &x.CheckOK, &x.CheckNote); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &x.P)
		if x.P.Preset != nil {
			x.Preset, _ = executors.Lookup(*x.P.Preset)
		}
		x.Widened = x.P.Access != nil && x.P.Access.Mode != "" && x.P.Access.Mode != "preset"
		switch {
		case !x.Widened:
			x.AccessText = "за класом: " + x.P.MaxDataClass
		case x.P.Access.Mode == "all":
			x.AccessText = "усі проєкти (ваше рішення)"
		default:
			x.AccessText = "клас " + x.P.MaxDataClass + " + проєкти: " + strings.Join(x.P.Access.Projects, ", ")
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// consentText is what the person accepts when widening an executor's access: the vendor's terms in words.
func consentText(p verbs.ExecutorProfile, mode string, projects []string) string {
	yn := func(b *bool) string {
		if b == nil {
			return "невідомо"
		}
		if *b {
			return "так"
		}
		return "ні"
	}
	var dp verbs.ExecutorDataPolicy
	if p.DataPolicy != nil {
		dp = *p.DataPolicy
	}
	scope := "усі ваші проєкти"
	if mode == "projects" {
		scope = "проєкти: " + strings.Join(projects, ", ")
	}
	terms := ""
	if p.TermsURL != nil {
		terms = "; умови: " + *p.TermsURL
		if p.TermsCheckedAt != nil {
			terms += " (перевірено " + *p.TermsCheckedAt + ")"
		}
	}
	return fmt.Sprintf("%s / %s отримає %s. Навчання на даних: %s; зберігання: %s; люди-рецензенти: %s%s.",
		p.Provider, p.Model, scope, yn(dp.Training), yn(dp.Retention), yn(dp.HumanReview), terms)
}

// checkKey asks the vendor for its models with the key: the key works and the address answers.
func checkKey(ctx context.Context, p verbs.ExecutorProfile, key string) (bool, string) {
	if p.BaseURL == nil {
		return false, "немає адреси"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(*p.BaseURL, "/")+"/models", nil)
	if err != nil {
		return false, err.Error()
	}
	if p.Adapter != nil && *p.Adapter == "anthropic" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "немає відповіді: " + err.Error()
	}
	defer resp.Body.Close()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(&body)
	if resp.StatusCode/100 != 2 {
		return false, fmt.Sprintf("вендор відповів HTTP %d", resp.StatusCode)
	}
	has := p.Model == "" || len(body.Data) == 0 || slices.ContainsFunc(body.Data, func(m struct {
		ID string `json:"id"`
	}) bool {
		return m.ID == p.Model
	})
	if !has {
		return true, fmt.Sprintf("ключ працює, але моделі %s у списку вендора немає (%d моделей)", p.Model, len(body.Data))
	}
	return true, fmt.Sprintf("ключ працює, моделей: %d", len(body.Data))
}

func buildCommit() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				return s.Value[:12]
			}
		}
	}
	return "dev"
}

// settingsRoutes adds /viewer/status and /viewer/settings/executors.
func (a *API) settingsRoutes(mux *http.ServeMux, verify TokenVerifier, oidc *ViewerOIDC) {
	const why = "Стан і налаштування відкриваються лише після входу через Keycloak (кнопка на сторінці входу), не токеном."
	fail := func(w http.ResponseWriter, err error) {
		e := a.public("viewer settings", err)
		w.WriteHeader(e.status)
		_ = page(w, "error", map[string]any{"Error": e.Message, "Code": e.Code})
	}
	audit := func(ctx context.Context, c *Caller, what string, start time.Time, err error) {
		if a.Audit != nil {
			code := ""
			if err != nil {
				code = a.public("viewer settings", err).Code
			}
			a.Audit(ctx, "viewer settings "+what, *c, code, time.Since(start))
		}
	}

	mux.HandleFunc("GET /viewer/status", func(w http.ResponseWriter, r *http.Request) {
		ctx, c, _, ok := a.viewerPerson(verify, oidc, w, r, why)
		if !ok {
			return
		}
		start := time.Now()
		err := func() error {
			h, err := a.Q.Health(ctx, c.query(), projector.Version)
			if err != nil {
				return err
			}
			cons, err := a.Q.ConsumerList(ctx, c.query(), "")
			if err != nil {
				return err
			}
			type runnerRow struct {
				Runner, Version string
				SeenAt          time.Time
				Stale           bool
				Executors       []map[string]any
			}
			var runners []runnerRow
			rows, err := a.DB.Query(pg.WithScope(ctx, pg.ScopeSystem), `SELECT runner, COALESCE(version, ''), seen_at, executors
				  FROM auth.runner_status WHERE tenant_id = $1 AND owner_id = $2 ORDER BY runner`, c.TenantID, c.Owner)
			if err != nil {
				return err
			}
			for rows.Next() {
				var x runnerRow
				var raw []byte
				if err := rows.Scan(&x.Runner, &x.Version, &x.SeenAt, &raw); err != nil {
					rows.Close()
					return err
				}
				_ = json.Unmarshal(raw, &x.Executors)
				x.Stale = time.Since(x.SeenAt) > 5*time.Minute
				runners = append(runners, x)
			}
			rows.Close()
			ex, err := a.executorRows(ctx, c)
			if err != nil {
				return err
			}
			type consumerRow struct {
				query.ConsumerRow
				Stale bool
			}
			var cs []consumerRow
			for _, x := range cons.Items {
				cs = append(cs, consumerRow{x, time.Since(x.UpdatedAt) > query.StaleConsumer})
			}
			return page(w, "status", map[string]any{"H": h, "Commit": buildCommit(), "Projector": projector.Version,
				"Runners": runners, "Executors": ex, "Consumers": cs})
		}()
		if err != nil {
			fail(w, err)
		}
		audit(ctx, c, "status", start, err)
	})

	show := func(w http.ResponseWriter, ctx context.Context, c *Caller, sid, preset, problem string) error {
		ex, err := a.executorRows(ctx, c)
		if err != nil {
			return err
		}
		data := map[string]any{"Executors": ex, "Presets": executors.Presets(), "CSRF": oidc.csrf(sid), "Problem": problem,
			"CanStoreKeys": a.Sealer != nil}
		if p, ok := executors.Lookup(preset); ok {
			data["P"] = p
		}
		return page(w, "executors", data)
	}
	mux.HandleFunc("GET /viewer/settings/executors", func(w http.ResponseWriter, r *http.Request) {
		ctx, c, sid, ok := a.viewerPerson(verify, oidc, w, r, why)
		if !ok {
			return
		}
		start := time.Now()
		err := show(w, ctx, c, sid, r.URL.Query().Get("preset"), "")
		if err != nil {
			fail(w, err)
		}
		audit(ctx, c, "show", start, err)
	})
	mux.HandleFunc("POST /viewer/settings/executors", func(w http.ResponseWriter, r *http.Request) {
		ctx, c, sid, ok := a.viewerPerson(verify, oidc, w, r, why)
		if !ok {
			return
		}
		start := time.Now()
		if want := oidc.csrf(sid); want == "" || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(want)) != 1 {
			w.WriteHeader(http.StatusForbidden)
			_ = page(w, "error", map[string]any{"Code": "csrf", "Error": "Форма застаріла або прийшла не з цієї сторінки — відкрийте її знову."})
			audit(ctx, c, "csrf", start, &Error{Code: "forbidden"})
			return
		}
		action := r.PostFormValue("action")
		problem, err := a.settingsAction(ctx, c, r, action)
		audit(ctx, c, action, start, err)
		switch {
		case err != nil:
			fail(w, err)
		case problem != "":
			w.WriteHeader(http.StatusUnprocessableEntity)
			if serr := show(w, ctx, c, sid, r.PostFormValue("preset"), problem); serr != nil {
				fail(w, serr)
			}
		default:
			http.Redirect(w, r, "/viewer/settings/executors", http.StatusSeeOther)
		}
	})
}

// settingsAction does one form action; problem is a message for the person (the form is shown again with it).
func (a *API) settingsAction(ctx context.Context, c *Caller, r *http.Request, action string) (problem string, err error) {
	projectsOf := func(s string) []string {
		var out []string
		for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
			out = append(out, strings.TrimSpace(p))
		}
		return out
	}
	access := func(p *verbs.ExecutorProfile) (string, error) {
		mode := r.PostFormValue("access")
		if mode == "" || mode == "preset" {
			p.Access = nil
			return "", nil
		}
		projects := projectsOf(r.PostFormValue("projects"))
		if mode == "projects" && len(projects) == 0 {
			return "Для «частково» назвіть проєкти.", nil
		}
		text := consentText(*p, mode, projects)
		if r.PostFormValue("consent") != "yes" {
			return "Щоб розширити доступ, підтвердіть умови вендора: " + text, nil
		}
		now := time.Now().UTC().Format(time.RFC3339)
		p.Access = &verbs.ExecutorAccess{Mode: mode, Projects: projects, Consent: &text, GrantedAt: &now}
		return "", nil
	}
	seal := func(id uuid.UUID, key string) error {
		if a.Sealer == nil {
			return invalid("the app has no LITOPYS_SECRET_KEY: it cannot store keys")
		}
		nonce, sealed, err := a.Sealer.Seal(key, id[:])
		if err != nil {
			return err
		}
		_, err = a.V.ExecutorKeySet(ctx, c.verbs(), id, executors.Fingerprint(key), nonce, sealed)
		return err
	}
	load := func() (uuid.UUID, verbs.ExecutorProfile, error) {
		var p verbs.ExecutorProfile
		id, err := uuid.Parse(r.PostFormValue("executor"))
		if err != nil {
			return id, p, invalid("executor")
		}
		rows, err := a.executorRows(ctx, c)
		if err != nil {
			return id, p, err
		}
		for _, x := range rows {
			if x.ActorID == id {
				return id, x.P, nil
			}
		}
		return id, p, invalid("executor: not yours or gone")
	}
	amend := func(id uuid.UUID, p verbs.ExecutorProfile) error {
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		_, err = a.V.ActorAmend(ctx, c.verbs(), verbs.ActorAmendIn{ActorID: id, Executor: raw})
		return err
	}

	switch action {
	case "add":
		pr, ok := executors.Lookup(r.PostFormValue("preset"))
		if !ok {
			return "Оберіть вендора зі списку.", nil
		}
		fields := map[string]string{}
		for _, f := range pr.Fields {
			fields[f] = strings.TrimSpace(r.PostFormValue("field_" + f))
		}
		url, ferr := pr.URL(fields)
		if ferr != nil {
			return "Поле " + ferr.Error(), nil
		}
		model := strings.TrimSpace(r.PostFormValue("model"))
		if model == "" {
			model = pr.Model
		}
		name := strings.TrimSpace(r.PostFormValue("name"))
		if name == "" {
			name = pr.Title + " " + model
		}
		key := strings.TrimSpace(r.PostFormValue("key"))
		if pr.KeyKind != "local" && key == "" {
			return "Вставте ключ API (інструкція поруч).", nil
		}
		p := verbs.ExecutorProfile{Provider: pr.Key, Model: model, KeyKind: pr.KeyKind, MaxDataClass: pr.DataClass,
			Preset: &pr.Key, Adapter: &pr.Adapter, BaseURL: &url}
		if len(pr.Fields) > 0 {
			p.Fields = fields
		}
		if len(pr.Limits) > 0 {
			l := pr.Limits
			p.Limits = &verbs.ExecutorLimits{}
			for k, v := range map[string]**int{"rpm": &p.Limits.RPM, "rpd": &p.Limits.RPD, "tpm": &p.Limits.TPM, "tpd": &p.Limits.TPD} {
				if n, ok := l[k]; ok {
					*v = &n
				}
			}
		}
		if len(pr.DataPolicy) > 0 {
			t, re, hr := pr.DataPolicy["training"], pr.DataPolicy["retention"], pr.DataPolicy["human_review"]
			p.DataPolicy = &verbs.ExecutorDataPolicy{Training: &t, Retention: &re, HumanReview: &hr}
		}
		if pr.TermsURL != "" {
			p.TermsURL = &pr.TermsURL
		}
		if pr.TermsCheckedAt != "" {
			p.TermsCheckedAt = &pr.TermsCheckedAt
		}
		if problem, err := access(&p); problem != "" || err != nil {
			return problem, err
		}
		ext := "executor:" + pr.Key + "/" + model
		id, _, err := a.V.ActorRegister(ctx, c.verbs(), verbs.ActorRegisterIn{Kind: "service", DisplayName: name, ExternalIdentity: &ext, Executor: &p})
		if err != nil {
			return "", err
		}
		if key != "" {
			return "", seal(id, key)
		}
		return "", nil
	case "key":
		id, _, err := load()
		if err != nil {
			return "", err
		}
		key := strings.TrimSpace(r.PostFormValue("key"))
		if key == "" {
			return "Вставте новий ключ.", nil
		}
		return "", seal(id, key)
	case "check":
		id, p, err := load()
		if err != nil {
			return "", err
		}
		key := ""
		if a.Sealer != nil {
			var nonce, sealed []byte
			if err := a.DB.QueryRow(pg.WithScope(ctx, pg.ScopeSystem), `SELECT nonce, sealed FROM auth.executor_key WHERE actor_id = $1`, id).Scan(&nonce, &sealed); err == nil {
				if key, err = a.Sealer.Open(nonce, sealed, id[:]); err != nil {
					return "Ключ не відкривається ключем апки — введіть його знову.", nil
				}
			}
		}
		ok, note := checkKey(ctx, p, key)
		var at time.Time
		_ = a.DB.QueryRow(pg.WithScope(ctx, pg.ScopeSystem), `UPDATE auth.executor_key SET checked_at = now(), check_ok = $2, check_note = $3
			 WHERE actor_id = $1 RETURNING checked_at`, id, ok, note).Scan(&at)
		if !ok {
			return "Перевірка: " + note, nil
		}
		return "", nil
	case "access":
		id, p, err := load()
		if err != nil {
			return "", err
		}
		if problem, err := access(&p); problem != "" || err != nil {
			return problem, err
		}
		return "", amend(id, p)
	case "disable", "enable":
		id, p, err := load()
		if err != nil {
			return "", err
		}
		off := action == "disable"
		p.Disabled = &off
		return "", amend(id, p)
	}
	return "", invalid("action: add | key | check | access | disable | enable")
}
