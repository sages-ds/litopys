package importer_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"litopys.smart-gw.co.ua/core/importer"
	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

func count(t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := q.QueryRow(pg.WithScope(context.Background(), pg.ScopeSystem), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func writeNote(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCurated: curated notes become documents — dry run leaves nothing, derived_from links
// reach the records of the previous service, a superseding note supersedes, a repeat writes nothing, a changed
// note is amended, a note without uid is skipped with the reason.
func TestCurated(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 2)
	var tenant uuid.UUID
	if err := app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	v := &verbs.Service{DB: app}
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	if err != nil {
		t.Fatal(err)
	}
	me := verbs.Principal{TenantID: tenant, ActorID: ada}
	open, err := v.ContextOpen(ctx, me, verbs.ContextOpenIn{Project: "courses", Attach: "none", AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	me.RunID = open.RunID
	body := "the record the document sums up"
	src, err := v.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "courses", Type: "decision", Title: "Source record", Body: &body})
	if err != nil {
		t.Fatal(err)
	}
	// derived_from names records of the owner by number: R-<num> of the source record
	var num int64
	if err := app.QueryRow(sys, `SELECT num FROM projection.statement WHERE statement_id = $1`, src.StatementID).Scan(&num); err != nil {
		t.Fatal(err)
	}
	r := fmt.Sprintf("[[R-%d]]", num)

	vault := t.TempDir()
	writeNote(t, vault, "curated/courses/docker.md", "---\nuid: cur-1-docker\ntype: decision\nproject: \"[[courses]]\"\ncreated: 2026-09-12\n"+
		"title: 'Docker on the same VM'\nderived_from: \""+r+"\"\n---\n\n# Docker on the same VM\n\nnginx and PostgreSQL stay on the host.\n")
	// R-9999 is no record of the owner; a link in any other form ([[note-291]]) is no reference to a record
	writeNote(t, vault, "curated/courses/docker-v2.md", "---\nuid: cur-2-docker\nproject: \"[[courses]]\"\ntitle: Appliance VM instead\n"+
		"supersedes: \"[[cur-1-docker]]\"\nderived_from: [\""+r+"\", \"[[R-9999]]\", \"[[note-291]]\"]\n---\n\nIncus inside one VM.\n")
	// a secret-like line is masked when stored: the repeat must still find the note unchanged
	writeNote(t, vault, "curated/garden/plan.md", "---\nuid: cur-3-garden\nproject: \"[[garden]]\"\n---\n\n# Garden plan\n\nWater at dawn.\npassword=hunter2\n")
	writeNote(t, vault, "curated/garden/loose.md", "# no frontmatter at all\n")
	notes, skipped, err := importer.ParseCurated(vault)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 3 || len(skipped) != 1 || !strings.Contains(skipped[0], "no frontmatter") {
		t.Fatalf("parse: %d notes, skipped %v", len(notes), skipped)
	}
	journal := func() int { return count(t, app, `SELECT count(*) FROM journal`) }

	before := journal()
	tx, err := app.Begin(sys)
	if err != nil {
		t.Fatal(err)
	}
	dry, err := importer.Curated(ctx, tx, tenant, "ada@example", notes)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(sys)
	if journal() != before || dry.Created != 3 {
		t.Fatalf("dry run: %+v, journal %d -> %d", dry, before, journal())
	}

	rep, err := importer.Curated(ctx, app, tenant, "ada@example", notes)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 3 || rep.Links != 2 || len(rep.Projects) != 1 || rep.Projects[0] != "garden" {
		t.Fatalf("import: %+v", rep)
	}
	if !strings.Contains(strings.Join(rep.Skipped, " "), "R-9999 not found") {
		t.Errorf("an unknown R-N must be reported: %v", rep.Skipped)
	}
	var status, title, file string
	if err := app.QueryRow(sys, `SELECT status FROM projection.statement WHERE external_ref = 'cur-1-docker'`).Scan(&status); err != nil || status != "superseded" {
		t.Errorf("the superseded document: %q %v", status, err)
	}
	if err := app.QueryRow(sys, `SELECT title, files[1] FROM projection.statement WHERE external_ref = 'cur-3-garden' AND type = 'document'`).
		Scan(&title, &file); err != nil || title != "Garden plan" || file != "curated/garden/plan.md" {
		t.Errorf("garden document: %q %q %v", title, file, err)
	}
	// the import run carries the client of the import: no event falls back to run = actor
	if n := count(t, app, `SELECT count(*) FROM journal WHERE run_id = actor_id AND type <> 'actor.registered'`); n != 0 {
		t.Errorf("%d events without a run", n)
	}

	after := journal()
	again, err := importer.Curated(ctx, app, tenant, "ada@example", notes)
	if err != nil {
		t.Fatal(err)
	}
	if journal() != after || again.Unchanged != 3 || again.Created != 0 || again.Amended != 0 {
		t.Fatalf("repeat: %+v, journal %d -> %d", again, after, journal())
	}

	writeNote(t, vault, "curated/garden/plan.md", "---\nuid: cur-3-garden\nproject: \"[[garden]]\"\n---\n\n# Garden plan\n\nWater at dusk.\n")
	notes, _, _ = importer.ParseCurated(vault)
	changed, err := importer.Curated(ctx, app, tenant, "ada@example", notes)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Amended != 1 {
		t.Fatalf("changed note: %+v", changed)
	}
}
