package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"

	"litopys.smart-gw.co.ua/core/executors"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestGateViewerSettings: with the Keycloak sign-in a person sees the status page and adds an
// executor from a preset with its key (sealed, not in the journal); widening the access needs the person's consent;
// the token form opens neither page.
func TestGateViewerSettings(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	v := &verbs.Service{DB: app}
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	sealer, err := executors.NewSealer(base64.StdEncoding.EncodeToString(k))
	if err != nil {
		t.Fatal(err)
	}
	a := &API{V: v, Q: &query.Service{DB: app}, DB: app, Log: slog.Default(), Sealer: sealer,
		Resolve: func(ctx context.Context, _ *sdkauth.TokenInfo, _ http.Header) (context.Context, Caller, error) {
			return ctx, Caller{TenantID: tenant, ActorID: ada, Owner: ada, Role: "owner"}, nil
		}}
	verify := func(_ context.Context, tok string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		if tok == "AT-person" || tok == "LTP_form" {
			return &sdkauth.TokenInfo{}, nil
		}
		return nil, errors.New("bad token")
	}
	o := &ViewerOIDC{ClientID: "litopys-viewer"}
	sid := o.remember(tokenReply{Access: "AT-person", ExpiresIn: 3600})
	h := a.Viewer(verify, o)
	do := func(method, path, cookieName, cookie string, form url.Values) *httptest.ResponseRecorder {
		body := strings.NewReader("")
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		r := httptest.NewRequest(method, path, body)
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	if rec := do(http.MethodGet, "/viewer/status", viewerSessionCookie, sid, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Стан Litopys") {
		t.Fatalf("status page: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/viewer/settings/executors", viewerCookie, "LTP_form", nil); rec.Code != http.StatusForbidden {
		t.Errorf("the token form opened the settings: %d", rec.Code)
	}
	pg := do(http.MethodGet, "/viewer/settings/executors?preset=gemini", viewerSessionCookie, sid, nil)
	if pg.Code != http.StatusOK || !strings.Contains(pg.Body.String(), "aistudio.google.com") {
		t.Fatalf("settings page with the gemini help: %d", pg.Code)
	}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]*)"`).FindStringSubmatch(pg.Body.String())[1]
	const key = "AIzaPageKey0123456789"
	add := url.Values{"csrf": {csrf}, "action": {"add"}, "preset": {"gemini"}, "key": {key}, "access": {"all"}}
	if rec := do(http.MethodPost, "/viewer/settings/executors", viewerSessionCookie, sid, add); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "підтвердіть умови") {
		t.Errorf("widening without consent: %d", rec.Code)
	}
	add.Set("consent", "yes")
	if rec := do(http.MethodPost, "/viewer/settings/executors", viewerSessionCookie, sid, add); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	var mode, fp string
	if err := app.QueryRow(sys, `SELECT a.executor->'access'->>'mode', k.fingerprint FROM projection.actor a
		JOIN auth.executor_key k ON k.actor_id = a.actor_id WHERE a.executor->>'preset' = 'gemini'`).Scan(&mode, &fp); err != nil || mode != "all" || fp != executors.Fingerprint(key) {
		t.Fatalf("executor added: mode %q fingerprint %q %v", mode, fp, err)
	}
	var inJournal bool
	if err := app.QueryRow(sys, `SELECT EXISTS (SELECT 1 FROM journal WHERE payload::text LIKE '%' || $1 || '%')`, key).Scan(&inJournal); err != nil || inJournal {
		t.Fatalf("the key in the journal: %v %v", inJournal, err)
	}
	if rec := do(http.MethodGet, "/viewer/status", viewerSessionCookie, sid, nil); !strings.Contains(rec.Body.String(), "усі проєкти") {
		t.Errorf("the status page does not show the executor's access")
	}
}
