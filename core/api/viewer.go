package api

// The knowledge viewer /viewer: server-rendered pages built live from the
// projections, under the same row-level security and policy as the API — read only. Sign-in exchanges a
// token for an HttpOnly cookie (P6a; Keycloak sign-in is the next step). Markdown is rendered by goldmark
// with raw HTML off, every other value by html/template: nothing a statement contains runs in the browser.

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/query"
)

// TokenVerifier checks a raw token the way the bearer middleware does.
type TokenVerifier func(ctx context.Context, token string, r *http.Request) (*sdkauth.TokenInfo, error)

const viewerCookie = "litopys_viewer"

var md = goldmark.New(goldmark.WithExtensions(extension.GFM)) // raw HTML is dropped (goldmark default: unsafe off)

// wikilinks: [[R-123]] (a record of the owner), [[anything]] (a search)
var wikiRe = regexp.MustCompile(`\[\[([^\]|]{1,200})(\|([^\]]{1,200}))?\]\]`)

func renderMD(text string) template.HTML {
	text = wikiRe.ReplaceAllStringFunc(text, func(m string) string {
		p := wikiRe.FindStringSubmatch(m)
		target, label := strings.TrimSpace(p[1]), strings.TrimSpace(p[3])
		if label == "" {
			label = target
		}
		if len(target) > 2 && strings.EqualFold(target[:2], "R-") { // R-12 and r-12, as the search reads them
			if n := target[2:]; func() bool { _, err := strconv.Atoi(n); return err == nil }() {
				return fmt.Sprintf("[%s](/viewer/r/%s)", label, n)
			}
		}
		return fmt.Sprintf("[%s](/viewer/search?q=%s)", label, url.QueryEscape(target))
	})
	var b bytes.Buffer
	if err := md.Convert([]byte(text), &b); err != nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	return template.HTML(b.String()) //nolint:gosec // goldmark output with raw HTML disabled
}

// derivedToken: the token is an executor's derived grant — its step's verbs of /v1 only, never the viewer.
func derivedToken(ti *sdkauth.TokenInfo) bool {
	if ti == nil {
		return false
	}
	p, ok := ti.Extra["principal"].(auth.Principal)
	return ok && p.Grant.Derived
}

// Viewer is the handler of /viewer/. oidc: the Keycloak sign-in (nil: the token form only).
func (a *API) Viewer(verify TokenVerifier, oidc *ViewerOIDC) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /viewer/login", func(w http.ResponseWriter, r *http.Request) { page(w, "login", map[string]any{"OIDC": oidc != nil}) })
	if oidc != nil {
		mux.HandleFunc("GET /viewer/oidc", oidc.start)
		mux.HandleFunc("GET /viewer/callback", func(w http.ResponseWriter, r *http.Request) { oidc.callback(w, r, verify) })
	}
	mux.HandleFunc("POST /viewer/login", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(r.FormValue("token"))
		if ti, err := verify(r.Context(), token, r); token == "" || err != nil || derivedToken(ti) {
			w.WriteHeader(http.StatusUnauthorized)
			page(w, "login", map[string]any{"Error": "The token is not valid.", "OIDC": oidc != nil})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: viewerCookie, Value: token, Path: "/viewer", HttpOnly: true, Secure: secure(r),
			SameSite: http.SameSiteStrictMode, MaxAge: 7 * 24 * 3600})
		http.Redirect(w, r, "/viewer/", http.StatusSeeOther)
	})
	a.confirmRoutes(mux, verify, oidc)  // the one POST action besides sign-in: a person confirms a retention label
	a.settingsRoutes(mux, verify, oidc) // status page and executors' settings, Keycloak sign-in only
	mux.HandleFunc("GET /viewer/logout", func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie(viewerSessionCookie); err == nil && oidc != nil {
			oidc.forget(ck.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: viewerCookie, Value: "", Path: "/viewer", HttpOnly: true, Secure: secure(r), MaxAge: -1})
		http.SetCookie(w, &http.Cookie{Name: viewerSessionCookie, Value: "", Path: "/viewer", HttpOnly: true, Secure: secure(r), MaxAge: -1})
		http.Redirect(w, r, "/viewer/login", http.StatusSeeOther)
	})
	signed := func(name string, h func(ctx context.Context, c *Caller, w http.ResponseWriter, r *http.Request) error) {
		mux.HandleFunc(name, func(w http.ResponseWriter, r *http.Request) {
			token := ""
			if ck, err := r.Cookie(viewerSessionCookie); err == nil && oidc != nil { // Keycloak session (P6c)
				if token, err = oidc.access(r.Context(), ck.Value); err != nil {
					token = ""
				}
			}
			if ck, err := r.Cookie(viewerCookie); token == "" && err == nil { // token sign-in (P6a)
				token = ck.Value
			}
			if token == "" {
				http.Redirect(w, r, "/viewer/login", http.StatusSeeOther)
				return
			}
			ti, err := verify(r.Context(), token, r)
			if err != nil {
				http.Redirect(w, r, "/viewer/logout", http.StatusSeeOther)
				return
			}
			ctx, c, err := a.Resolve(r.Context(), ti, r.Header)
			if err != nil || c.Grant.Derived { // an executor's grant: its step's verbs of /v1 only
				http.Redirect(w, r, "/viewer/logout", http.StatusSeeOther)
				return
			}
			start := time.Now()
			err = h(ctx, &c, w, r)
			code := ""
			if err != nil {
				e := a.public("viewer", err)
				code = e.Code
				w.WriteHeader(e.status)
				page(w, "error", map[string]any{"Error": e.Message, "Code": e.Code})
			}
			if a.Audit != nil {
				a.Audit(ctx, "viewer "+strings.TrimPrefix(r.Pattern, "GET /viewer/"), c, code, time.Since(start))
			}
		})
	}

	signed("GET /viewer/{$}", func(ctx context.Context, c *Caller, w http.ResponseWriter, r *http.Request) error {
		cat, err := a.Q.Catalog(ctx, c.query())
		if err != nil {
			return err
		}
		return page(w, "catalog", map[string]any{"Projects": cat})
	})
	signed("GET /viewer/p/{slug}", func(ctx context.Context, c *Caller, w http.ResponseWriter, r *http.Request) error {
		b, err := a.Q.ContextPack(ctx, c.query(), r.PathValue("slug"), 32000)
		if err != nil {
			return err
		}
		// the documents of the project (former curated notes) are listed apart from the stream of records
		docs, err := a.Q.Search(ctx, c.query(), query.SearchIn{Project: b.Project.Name, Family: "document", Limit: 100, ByCreation: true})
		if err != nil {
			return err
		}
		return page(w, "project", map[string]any{"B": b, "Docs": docs.Items, "Current": md2(b.Current), "Last": md2(summaryOf(b.LastSummary))})
	})
	signed("GET /viewer/p/{slug}/all", func(ctx context.Context, c *Caller, w http.ResponseWriter, r *http.Request) error {
		in := query.SearchIn{Project: r.PathValue("slug"), Limit: 100, ByCreation: true}
		if v, err := uuid.Parse(r.URL.Query().Get("before")); err == nil {
			in.IDBefore = &v
		}
		res, err := a.Q.Search(ctx, c.query(), in)
		if err != nil {
			return err
		}
		next := ""
		if len(res.Items) == 100 {
			next = res.Items[len(res.Items)-1].StatementID.String()
		}
		return page(w, "list", map[string]any{"Title": "Усі записи " + in.Project, "Items": res.Items, "Project": in.Project, "Next": next})
	})
	signed("GET /viewer/p/{slug}/graph", func(ctx context.Context, c *Caller, w http.ResponseWriter, r *http.Request) error {
		slug := r.PathValue("slug")
		if err := a.slugOf(ctx, c, &slug); err != nil {
			return err
		}
		svg, n, e, err := a.graph(ctx, c, slug)
		if err != nil {
			return err
		}
		return page(w, "graph", map[string]any{"Project": slug, "SVG": svg, "Nodes": n, "Edges": e})
	})
	signed("GET /viewer/s/{id}", func(ctx context.Context, c *Caller, w http.ResponseWriter, r *http.Request) error {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			return invalid("not a statement id")
		}
		out, err := a.Q.Read(ctx, c.query(), []uuid.UUID{id}, true)
		if err != nil {
			return err
		}
		if len(out.Items) == 0 {
			return errorf("statement_not_found", "statement %s", id)
		}
		s := out.Items[0]
		// titles of the linked statements (both directions)
		var ids []uuid.UUID
		for _, l := range s.Links {
			if l.To["entity_type"] == "statement" {
				if v, ok := l.To["entity_id"].(uuid.UUID); ok && len(ids) < 20 {
					ids = append(ids, v)
				}
			}
		}
		titles := map[uuid.UUID]string{}
		if len(ids) > 0 {
			if linked, err := a.Q.Read(ctx, c.query(), ids, false); err == nil {
				for _, x := range linked.Items {
					titles[x.StatementID] = x.Title
				}
			}
		}
		type link struct {
			Relation, Title, Href string
		}
		var links []link
		for _, l := range s.Links {
			v, _ := l.To["entity_id"].(uuid.UUID)
			t, _ := l.To["entity_type"].(string)
			if t == "statement" {
				links = append(links, link{l.Relation, titles[v], "/viewer/s/" + v.String()})
			} else {
				links = append(links, link{l.Relation, t + " " + v.String()[:8], ""})
			}
		}
		body := ""
		if s.Body != nil {
			body = *s.Body
		}
		return page(w, "statement", map[string]any{"S": s, "Body": renderMD(body), "Links": links})
	})
	signed("GET /viewer/r/{n}", func(ctx context.Context, c *Caller, w http.ResponseWriter, r *http.Request) error {
		n, err := strconv.ParseInt(r.PathValue("n"), 10, 64)
		if err != nil {
			return invalid("not a record number")
		}
		// R-<num> within the owner (0030: num is unique per owner)
		var id uuid.UUID
		if err := a.DB.QueryRow(ctx, `SELECT statement_id FROM projection.statement WHERE tenant_id = $1 AND owner_id = $2 AND num = $3`,
			c.TenantID, c.Owner, n).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return errorf("statement_not_found", "record R-%d", n)
		} else if err != nil {
			return err
		}
		http.Redirect(w, r, "/viewer/s/"+id.String(), http.StatusFound)
		return nil
	})
	signed("GET /viewer/search", func(ctx context.Context, c *Caller, w http.ResponseWriter, r *http.Request) error {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		var items []query.IndexRow
		if q != "" {
			res, err := a.Q.Search(ctx, c.query(), query.SearchIn{Query: q, Project: r.URL.Query().Get("project"), Limit: 50, IncludeSnippet: true})
			if err != nil {
				return err
			}
			items = res.Items
		}
		return page(w, "list", map[string]any{"Title": "Пошук", "Q": q, "Items": items, "Search": true})
	})
	// an attachment of a project: always a download — a file is never rendered in the viewer's origin
	signed("GET /viewer/att/{sha}", func(ctx context.Context, c *Caller, w http.ResponseWriter, r *http.Request) error {
		sha := r.PathValue("sha")
		if a.Files == nil || len(sha) != 64 {
			return invalid("att/<sha256>")
		}
		rows, err := a.Q.Attachments(ctx, c.query(), "", sha)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return &Error{Code: "attachment_not_found", Message: "no file " + sha[:12] + " of yours", status: http.StatusNotFound}
		}
		f, err := a.Files.Open(sha)
		if err != nil {
			return &Error{Code: "attachment_not_found", Message: "the file is gone from the data directory", status: http.StatusNotFound}
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename*=UTF-8''%s`, url.PathEscape(rows[0].Name)))
		_, err = io.Copy(w, f)
		return err
	})
	signed("GET /viewer/export/{file}", func(ctx context.Context, c *Caller, w http.ResponseWriter, r *http.Request) error {
		slug, ok := strings.CutSuffix(r.PathValue("file"), ".zip")
		if !ok {
			return invalid("export/<project>.zip")
		}
		data, err := a.export(ctx, c, slug)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, slug))
		_, err = w.Write(data)
		return err
	})
	return mux
}

func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func md2(s *string) template.HTML {
	if s == nil {
		return ""
	}
	return renderMD(*s)
}

func summaryOf(l *query.LastSummary) *string {
	if l == nil {
		return nil
	}
	return l.Summary
}

// export is a markdown package of a project in the Obsidian / Quartz layout: index.md with the
// state and open tasks, one note per statement with frontmatter, links between notes as wikilinks.
func (a *API) export(ctx context.Context, c *Caller, slug string) ([]byte, error) {
	b, err := a.Q.ContextPack(ctx, c.query(), slug, 32000)
	if err != nil {
		return nil, err
	}
	var all []query.IndexRow
	in := query.SearchIn{Project: slug, Limit: 100, ByCreation: true}
	for len(all) < 20000 {
		res, err := a.Q.Search(ctx, c.query(), in)
		if err != nil {
			return nil, err
		}
		all = append(all, res.Items...)
		if len(res.Items) < 100 {
			break
		}
		last := res.Items[len(res.Items)-1].StatementID
		in.IDBefore = &last
	}
	names := map[uuid.UUID]string{}
	for _, s := range all {
		names[s.StatementID] = noteName(s)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, text string) error {
		f, err := zw.Create(name)
		if err == nil {
			_, err = f.Write([]byte(text))
		}
		return err
	}
	var idx strings.Builder
	fmt.Fprintf(&idx, "---\nproject: %s\ntitle: %q\nexported: %s\nsource: litopys\n---\n\n# %s\n\n", slug, b.Project.Title, time.Now().UTC().Format(time.RFC3339), b.Project.Title)
	if b.Project.Summary != nil {
		idx.WriteString(*b.Project.Summary + "\n\n")
	}
	if b.Current != nil {
		idx.WriteString("## Where it stands\n\n" + *b.Current + "\n\n")
	}
	idx.WriteString("## Open tasks\n\n")
	for _, t := range b.Tasks {
		fmt.Fprintf(&idx, "- #%d %s (%s)\n", t.Num, t.Title, t.Status)
	}
	idx.WriteString("\n## Records\n\n")
	for _, s := range all {
		fmt.Fprintf(&idx, "- [[%s]] — %s, %s\n", names[s.StatementID], s.Type, s.EffectiveAt.Format("2006-01-02"))
	}
	if err := add(slug+"/index.md", idx.String()); err != nil {
		return nil, err
	}
	for i := 0; i < len(all); i += 20 {
		ids := []uuid.UUID{}
		for _, s := range all[i:min(i+20, len(all))] {
			ids = append(ids, s.StatementID)
		}
		out, err := a.Q.Read(ctx, c.query(), ids, false)
		if err != nil {
			return nil, err
		}
		for _, s := range out.Items {
			var n strings.Builder
			fmt.Fprintf(&n, "---\nid: %s\ntype: %s\nstatus: %s\neffective_at: %s\nproject: %s\n", s.StatementID, s.Type, s.Status, s.EffectiveAt.Format(time.RFC3339), slug)
			if len(s.Concepts) > 0 {
				fmt.Fprintf(&n, "concepts: [%s]\n", strings.Join(quoteAll(s.Concepts), ", "))
			}
			fmt.Fprintf(&n, "source: litopys\n---\n\n# %s\n\n", s.Title)
			if s.Body != nil {
				n.WriteString(*s.Body + "\n")
			}
			var rel []string
			for _, l := range s.Links {
				if v, ok := l.To["entity_id"].(uuid.UUID); ok && names[v] != "" {
					rel = append(rel, fmt.Sprintf("- %s [[%s]]", l.Relation, names[v]))
				}
			}
			if len(rel) > 0 {
				n.WriteString("\n## Links\n\n" + strings.Join(rel, "\n") + "\n")
			}
			if err := add(slug+"/"+names[s.StatementID]+".md", n.String()); err != nil {
				return nil, err
			}
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// noteName: date, a readable part of the title and a short id — unique and stable across exports.
func noteName(s query.IndexRow) string {
	t := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(s.Title), "-"), "-")
	if r := []rune(t); len(r) > 60 {
		t = strings.TrimRight(string(r[:60]), "-")
	}
	return fmt.Sprintf("%s-%s-%s", s.AssertedAt.Format("2006-01-02"), t, s.StatementID.String()[:8])
}

func quoteAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Quote(x)
	}
	return out
}
