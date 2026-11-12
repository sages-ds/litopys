package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/verbs"
)

// The person confirms a retention label outside the chat (owner decision
// 2026-10-30): the first POST action of the viewer. Only a Keycloak session of the viewer counts — its tokens come from the
// viewer's own code exchange (client litopys-viewer), so neither the token form nor any API connection of a model can
// confirm. The page shows the label and its impact from the server only; the form carries the version and the impact
// it showed, and a CSRF token of the session.

// pendingConfirmations: the human-only retention labels of a project not confirmed in their current version, with
// the page where the person confirms each.
func (a *API) pendingConfirmations(ctx context.Context, c *Caller, project string, id uuid.UUID) ([]map[string]any, error) {
	rows, err := a.Q.UnconfirmedRetentions(ctx, c.query(), id)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, x := range rows {
		v := x.Value
		if r, err := verbs.ParseRetention(&v); err == nil && r.HumanOnly() {
			out = append(out, map[string]any{"key": x.Key, "value": x.Value, "confirm_url": verbs.ConfirmURL(project, x.Key),
				"hint": "ask the user to confirm or reject it in the Litopys viewer; you cannot confirm it"})
		}
	}
	return out, nil
}

// confirmRoutes adds GET / POST /viewer/confirm/{project}/{key}; oidc nil — the routes explain that the Keycloak
// sign-in is required.
func (a *API) confirmRoutes(mux *http.ServeMux, verify TokenVerifier, oidc *ViewerOIDC) {
	person := func(w http.ResponseWriter, r *http.Request) (context.Context, *Caller, string, bool) {
		ck, err := r.Cookie(viewerSessionCookie)
		if oidc == nil || err != nil {
			w.WriteHeader(http.StatusForbidden)
			_ = page(w, "error", map[string]any{"Code": "confirmation_needs_sign_in",
				"Error": "Підтвердити мітку можна лише після входу через Keycloak (кнопка на сторінці входу), не токеном."})
			return nil, nil, "", false
		}
		token, err := oidc.access(r.Context(), ck.Value)
		if err == nil {
			var c Caller
			ti, verr := verify(r.Context(), token, r)
			if verr == nil {
				var ctx context.Context
				if ctx, c, verr = a.Resolve(r.Context(), ti, r.Header); verr == nil {
					return ctx, &c, ck.Value, true
				}
			}
		}
		http.Redirect(w, r, "/viewer/login", http.StatusSeeOther)
		return nil, nil, "", false
	}
	show := func(w http.ResponseWriter, ctx context.Context, c *Caller, sid, project, key, problem string) error {
		l, err := a.V.RetentionLabelOf(ctx, c.verbs(), project, key, time.Now())
		if err != nil {
			return err
		}
		var pretty map[string]any
		_ = json.Unmarshal([]byte(l.Value), &pretty)
		return page(w, "confirm", map[string]any{"L": l, "Rule": pretty, "CSRF": oidc.csrf(sid), "Problem": problem})
	}
	fail := func(w http.ResponseWriter, err error) {
		e := a.public("viewer confirm", err)
		w.WriteHeader(e.status)
		_ = page(w, "error", map[string]any{"Error": e.Message, "Code": e.Code})
	}
	audit := func(ctx context.Context, c *Caller, what string, start time.Time, err error) {
		if a.Audit != nil {
			code := ""
			if err != nil {
				code = a.public("viewer confirm", err).Code
			}
			a.Audit(ctx, "viewer confirm "+what, *c, code, time.Since(start))
		}
	}

	mux.HandleFunc("GET /viewer/confirm/{project}/{key}", func(w http.ResponseWriter, r *http.Request) {
		ctx, c, sid, ok := person(w, r)
		if !ok {
			return
		}
		start := time.Now()
		err := show(w, ctx, c, sid, r.PathValue("project"), r.PathValue("key"), "")
		if err != nil {
			fail(w, err)
		}
		audit(ctx, c, "show", start, err)
	})

	mux.HandleFunc("POST /viewer/confirm/{project}/{key}", func(w http.ResponseWriter, r *http.Request) {
		ctx, c, sid, ok := person(w, r)
		if !ok {
			return
		}
		start := time.Now()
		project, key := r.PathValue("project"), r.PathValue("key")
		want := oidc.csrf(sid)
		if got := r.PostFormValue("csrf"); want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.WriteHeader(http.StatusForbidden)
			_ = page(w, "error", map[string]any{"Code": "csrf", "Error": "Форма застаріла або прийшла не з цієї сторінки — відкрийте її знову."})
			audit(ctx, c, "csrf", start, &Error{Code: "forbidden"})
			return
		}
		var err error
		switch r.PostFormValue("action") {
		case "confirm":
			var ev uuid.UUID
			if ev, err = uuid.Parse(r.PostFormValue("event_id")); err != nil {
				err = invalid("event_id")
				break
			}
			records, _ := strconv.Atoi(r.PostFormValue("records"))
			tasks, _ := strconv.Atoi(r.PostFormValue("tasks"))
			_, err = a.V.FactConfirm(ctx, c.verbs(), project, key, ev, verbs.RetentionImpact{Records: records, Tasks: tasks}, time.Now())
		case "reject":
			reason := "rejected in the viewer"
			_, err = a.V.FactRetire(ctx, c.verbs(), project, "retention", key, &reason)
		default:
			err = invalid("action: confirm | reject")
		}
		audit(ctx, c, r.PostFormValue("action"), start, err)
		switch code := verbs.Code(err); {
		case err == nil:
			http.Redirect(w, r, "/viewer/p/"+project, http.StatusSeeOther)
		case code == "label_changed" || code == "impact_changed":
			// what the person saw is no longer what would be applied: show the new version, nothing is confirmed
			w.WriteHeader(http.StatusConflict)
			if serr := show(w, ctx, c, sid, project, key, strings.TrimSpace(a.public("viewer confirm", err).Message)); serr != nil {
				fail(w, serr)
			}
		default:
			fail(w, err)
		}
	})
}
