package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/embed"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/server"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// fakeEmbeddings: an OpenAI-compatible /embeddings whose vectors carry meaning by concept, across languages — a
// worktop in Ukrainian and in English point the same way, money another way.
func fakeEmbeddings(t *testing.T) *httptest.Server {
	concepts := [][]string{{"стільниц", "countertop", "worktop"}, {"плит", "tile"}, {"price", "ціна", "грн", "коштує"}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		var data []map[string]any
		for i, text := range in.Input {
			v := make([]float32, len(concepts)+1)
			v[len(concepts)] = 0.1
			low := strings.ToLower(text)
			for c, words := range concepts {
				for _, w := range words {
					if strings.Contains(low, w) {
						v[c] = 1
					}
				}
			}
			data = append(data, map[string]any{"index": i, "embedding": v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

// TestGateMeaningSearch: the worker's pass embeds the records; "countertop price" finds the
// Ukrainian record about the worktop that shares no word with it, marked coverage semantic; a model that is down
// leaves the lexical search working.
func TestGateMeaningSearch(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 8)
	admin := db.RolePool(t, "litopys_admin", 2)
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	ada, _, err := (&verbs.Service{DB: app}).RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.IssueGrant(ctx, app, tenant, ada, "owner", "gate", nil, policy.Grant{Profile: "full"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	model := fakeEmbeddings(t)
	defer model.Close()
	ec := &embed.Client{URL: model.URL, Model: "fake", MaxClass: "local", HTTP: model.Client()}
	f := server.New(app, admin, tenant)
	f.Q.Embedder = ec
	h, err := f.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	a := connect(t, srv.URL, tok)
	a.ok("project_declare", map[string]any{"name": "kitchen"})
	top := a.ok("statement_assert", map[string]any{"project": "kitchen", "type": "decision", "title": "Стільниця HPL",
		"body": "Беремо стільницю HPL за 18 000 грн"})
	a.ok("statement_assert", map[string]any{"project": "kitchen", "type": "note", "title": "Плитка на фартух", "body": "керамограніт"})

	if n, err := ec.Pass(sys, app, tenant, 16); err != nil || n != 2 {
		t.Fatalf("embedding pass: %d %v", n, err)
	}
	if n, _ := ec.Pass(sys, app, tenant, 16); n != 0 {
		t.Errorf("a second pass embedded %d records again", n)
	}
	res := a.ok("recall_search", map[string]any{"query": "countertop price", "project": "kitchen"})
	items, _ := res["items"].([]any)
	if len(items) == 0 || items[0].(map[string]any)["statement_id"] != top["statement_id"] {
		t.Fatalf("meaning search: %v", res)
	}
	if m, _ := items[0].(map[string]any)["match"].(map[string]any); m["coverage"] != "semantic" || res["coverage"] != "semantic" || res["meaning"] != "used" {
		t.Errorf("a meaning hit is not marked semantic: %v / %v", items[0], res["coverage"])
	}

	// the model is down: the lexical search still answers
	model.Close()
	if res := a.ok("recall_search", map[string]any{"query": "стільниця", "project": "kitchen"}); len(res["items"].([]any)) == 0 || res["meaning"] != "model_unavailable" {
		t.Errorf("lexical search without the model: %v", res)
	}
}
