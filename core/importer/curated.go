package importer

// Profile vault-curated-v1: the curated zone of an Obsidian vault —
// markdown notes with frontmatter (uid, title, project, created, status, derived_from, supersedes) — becomes
// documents in Litopys: statements of type document with provenance, the server being the source of truth
// from then on. Repeatable by uid (external_ref): a changed note is amended, an unchanged one skipped.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"litopys.smart-gw.co.ua/core/privacy"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
)

const CuratedSource = "vault-curated-v1"

// CuratedNote is one parsed note.
type CuratedNote struct {
	Path        string // relative to the vault root, slash-separated
	UID         string
	Title       string
	Project     string
	Created     *time.Time
	Status      string
	Body        string
	DerivedFrom []int64 // R-N of the owner
	Supersedes  string  // uid of the note this one replaces
}

// CuratedReport is the report of one run.
type CuratedReport struct {
	Notes     int      `json:"notes"`
	Created   int      `json:"created"`
	Amended   int      `json:"amended"`
	Unchanged int      `json:"unchanged"`
	Links     int      `json:"links"`
	Projects  []string `json:"projects_created,omitempty"`
	Skipped   []string `json:"skipped,omitempty"`
}

// recordRef: a record of the owner in derived_from, [[R-N]]; a vault that names records otherwise is
// rewritten to this form before the import.
var recordRef = regexp.MustCompile(`\[\[R-(\d+)\]\]`)

func unlink(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "[["), "]]")
}

// ParseCurated reads every *.md under root/curated (or root itself when it is the curated folder).
func ParseCurated(root string) ([]CuratedNote, []string, error) {
	base := root
	if st, err := os.Stat(filepath.Join(root, "curated")); err == nil && st.IsDir() {
		base = filepath.Join(root, "curated")
	}
	var notes []CuratedNote
	var skipped []string
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.Dir(base), path)
		n, why := parseNote(filepath.ToSlash(rel), strings.ReplaceAll(string(raw), "\r\n", "\n"))
		if why != "" {
			skipped = append(skipped, rel+": "+why)
			return nil
		}
		notes = append(notes, n)
		return nil
	})
	sort.Slice(notes, func(i, j int) bool { return notes[i].Path < notes[j].Path })
	return notes, skipped, err
}

func parseNote(rel, text string) (CuratedNote, string) {
	n := CuratedNote{Path: rel}
	if !strings.HasPrefix(text, "---\n") {
		return n, "no frontmatter"
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return n, "unterminated frontmatter"
	}
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(text[4:4+end]), &fm); err != nil {
		return n, "frontmatter: " + err.Error()
	}
	n.Body = strings.TrimLeft(text[4+end+4:], "\n")
	str := func(k string) string {
		if v, ok := fm[k]; ok && v != nil {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	n.UID, n.Title, n.Project, n.Status = str("uid"), str("title"), unlink(str("project")), str("status")
	if n.UID == "" {
		return n, "no uid"
	}
	if n.Project == "" {
		return n, "no project"
	}
	if n.Title == "" { // the first heading, else the file name
		for _, l := range strings.Split(n.Body, "\n") {
			if t, ok := strings.CutPrefix(l, "# "); ok {
				n.Title = strings.TrimSpace(t)
				break
			}
		}
		if n.Title == "" {
			n.Title = strings.TrimSuffix(filepath.Base(rel), ".md")
		}
	}
	switch v := fm["created"].(type) {
	case time.Time:
		n.Created = &v
	case string:
		if t, err := time.Parse("2006-01-02", v); err == nil {
			n.Created = &t
		}
	}
	var refs []string
	switch v := fm["derived_from"].(type) {
	case string:
		refs = []string{v}
	case []any:
		for _, x := range v {
			refs = append(refs, fmt.Sprint(x))
		}
	}
	for _, r := range refs {
		for _, m := range recordRef.FindAllStringSubmatch(r, -1) {
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				n.DerivedFrom = append(n.DerivedFrom, id)
			}
		}
	}
	n.Supersedes = unlink(str("supersedes"))
	return n, ""
}

// Curated imports parsed notes as the human with this e-mail. db: litopys_app pool (or a transaction for a dry
// run); every write goes through the verbs, in one import run that carries the client of the import.
func Curated(ctx context.Context, db verbs.DB, tenant uuid.UUID, email string, notes []CuratedNote) (CuratedReport, error) {
	rep := CuratedReport{Notes: len(notes)}
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	var human uuid.UUID
	if err := db.QueryRow(sys, `SELECT actor_id FROM projection.actor WHERE tenant_id = $1 AND kind = 'human' AND external_identity = $2`,
		tenant, email).Scan(&human); err != nil {
		return rep, fmt.Errorf("owner %s: %w", email, err)
	}
	v := &verbs.Service{DB: db}
	p := verbs.Principal{TenantID: tenant, ActorID: human}
	// the import run starts with the first write: a repeated import that finds nothing new writes nothing
	ensureRun := func() error {
		if p.RunID != uuid.Nil {
			return nil
		}
		purpose := "import " + CuratedSource
		run, err := v.RunStart(ctx, p, verbs.RunStartIn{Client: []byte(`{"runtime":"litopys-import","client_type":"worker"}`), Purpose: &purpose})
		p.RunID = run
		return err
	}

	existing := func(uid string) (id uuid.UUID, title string, body *string, err error) {
		err = db.QueryRow(sys, `SELECT statement_id, title, body FROM projection.statement
		   WHERE tenant_id = $1 AND owner_id = $2 AND type = 'document' AND external_ref = $3 AND status <> 'retracted'
		   ORDER BY recorded_at DESC LIMIT 1`, tenant, human, uid).Scan(&id, &title, &body)
		return
	}
	projects := map[string]bool{}
	ensureProject := func(name string) error {
		if projects[name] {
			return nil
		}
		var ok bool
		if err := db.QueryRow(sys, `SELECT EXISTS (SELECT 1 FROM projection.project WHERE tenant_id = $1 AND owner_id = $2 AND name = $3)`,
			tenant, human, name).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			if err := ensureRun(); err != nil {
				return err
			}
			if _, _, err := v.ProjectDeclare(ctx, p, verbs.ProjectDeclareIn{Name: name}); err != nil {
				return err
			}
			rep.Projects = append(rep.Projects, name)
		}
		projects[name] = true
		return nil
	}
	// a note that supersedes another goes after it
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].Supersedes == "" && notes[j].Supersedes != "" })
	for _, n := range notes {
		if err := ensureProject(n.Project); err != nil {
			rep.Skipped = append(rep.Skipped, n.Path+": project: "+err.Error())
			continue
		}
		title, _ := cut(strings.TrimSpace(n.Title), 500)
		body, wasCut := cut(strings.TrimSpace(n.Body), 32768) // the server keeps bodies trimmed; compare like it stores
		if wasCut {
			rep.Skipped = append(rep.Skipped, n.Path+": body cut to 32768 characters")
		}
		uid := n.UID
		id, oldTitle, oldBody, err := existing(uid)
		switch {
		case err == nil:
			// compare with what the verbs store — masked, private parts stripped — or a note with a secret-like line
			// is "changed" on every run (rehearsal 2026-10-31: obsidian-livesync-per-user-couchdb.md)
			storedTitle := privacy.MaskTitle(privacy.NormalizeOneLine(privacy.StripPrivate(title)))
			storedBody := strings.TrimSpace(privacy.MaskText(privacy.StripPrivate(body)))
			if oldTitle == storedTitle && oldBody != nil && strings.TrimSpace(*oldBody) == storedBody {
				rep.Unchanged++
			} else {
				if err := ensureRun(); err != nil {
					return rep, err
				}
				reason := "the curated note changed"
				if _, err := v.StatementAmend(ctx, p, verbs.StatementAmendIn{StatementID: id, Title: &title, Body: &body, Reason: &reason}); err != nil {
					return rep, fmt.Errorf("%s: %w", n.Path, err)
				}
				rep.Amended++
			}
		case errors.Is(err, pgx.ErrNoRows):
			if err := ensureRun(); err != nil {
				return rep, err
			}
			in := verbs.StatementAssertIn{Project: n.Project, Type: "document", Title: title, Body: &body, ExternalRef: &uid,
				Files: []string{n.Path}, EffectiveAt: n.Created}
			var out verbs.StatementAssertOut
			if old, _, _, oerr := existing(n.Supersedes); n.Supersedes != "" && oerr == nil {
				reason := "superseded in the curated zone"
				out, err = v.StatementSupersede(ctx, p, old, in, &reason)
			} else {
				out, err = v.StatementAssert(ctx, p, in)
			}
			if err != nil {
				return rep, fmt.Errorf("%s: %w", n.Path, err)
			}
			id = out.StatementID
			rep.Created++
		default:
			return rep, err
		}
		// derived_from: the records of the owner the document sums up, by number (R-N; num is unique per owner, 0030)
		for _, num := range n.DerivedFrom {
			var src uuid.UUID
			err := db.QueryRow(sys, `SELECT statement_id FROM projection.statement WHERE tenant_id = $1 AND owner_id = $2 AND num = $3`,
				tenant, human, num).Scan(&src)
			if errors.Is(err, pgx.ErrNoRows) {
				rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: R-%d not found", n.Path, num))
				continue
			}
			if err != nil {
				return rep, err
			}
			var has bool
			if err := db.QueryRow(sys, `SELECT EXISTS (SELECT 1 FROM projection.link WHERE from_id = $1 AND to_id = $2
			        AND relation = 'derived_from' AND NOT retired)`, id, src).Scan(&has); err != nil {
				return rep, err
			}
			if has {
				continue
			}
			if err := ensureRun(); err != nil {
				return rep, err
			}
			if _, _, err := v.LinkAssert(ctx, p, verbs.EntityRef{EntityType: "statement", EntityID: id},
				verbs.EntityRef{EntityType: "statement", EntityID: src}, "derived_from", nil); err != nil {
				return rep, fmt.Errorf("%s: link to R-%d: %w", n.Path, num, err)
			}
			rep.Links++
		}
	}
	if p.RunID != uuid.Nil {
		note := fmt.Sprintf("curated: %d created, %d amended, %d unchanged", rep.Created, rep.Amended, rep.Unchanged)
		if _, err := v.RunFinish(ctx, p, p.RunID, "ok", &note); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// cut: at most max characters, the last of them an ellipsis when cut — what every import of this repo writes.
func cut(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	r := []rune(s)
	return string(r[:max-1]) + "…", true
}
