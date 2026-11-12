// Command searcheval collects the search results of the search-quality gate
// (evals/search/README.md): every query of a query file through core/query Search on one frozen database,
// top-k statement ids (with their legacy ids, when imported) as JSON. Ids only — no titles or bodies, so
// the output may be kept next to the code; -show adds the title and the head of the body for the owner's
// local grading page (bin/, never committed). A dev tool: it is not part of the image.
//
//	searcheval -queries q.json -k 10 -actor <email> > results.json
//	searcheval -queries q.json -quoting > quoting.json
//
// -quoting lists the statements of the frozen base that name a query id of the gate (o-6, golden-3, m-57) — memory
// records about the gate itself — as legacy ids, for its exclusion list (litopys-golden search/exclude.json, #276):
// such a record cites the query word for word and pushes the real answers out of the top k, so it measures the
// gate's diary, not the search. The query text alone is no mark: a record that contains it is often the answer
// (m-61 /etc/semaphore/config.json, golden-5 — 2026-11-10 review), so only the id decides.
//
// Database as everywhere (LITOPYS_DATABASE_URL or PG*, LITOPYS_SET_ROLE=litopys_app). The base and the
// candidate of a gate run are two builds of this command on copies of the same frozen database.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/internal/pg"
)

type queryIn struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Project string `json:"project,omitempty"`
	Type    string `json:"type,omitempty"`
}

type hit struct {
	StatementID uuid.UUID `json:"statement_id"`
	LegacyID    *int64    `json:"legacy_id,omitempty"`
	Status      string    `json:"status"`
	Type        string    `json:"type"`
	Title       string    `json:"title,omitempty"`    // -show only
	Head        string    `json:"head,omitempty"`     // -show only: the first 600 characters of the body
	Coverage    string    `json:"coverage,omitempty"` // search v2: full | partial (the row's match)
}

type result struct {
	ID       string `json:"id"`
	Items    []hit  `json:"items"`
	Relaxed  bool   `json:"relaxed,omitempty"`
	Coverage string `json:"coverage,omitempty"` // search v2: full | partial | none
	Error    string `json:"error,omitempty"`
	Ms       int64  `json:"ms"`
}

func main() {
	file := flag.String("queries", "", "JSON array of {id, text, project?, type?}")
	k := flag.Int("k", 10, "results per query")
	actor := flag.String("actor", "", "e-mail (external identity) of the person whose memory is searched")
	show := flag.Bool("show", false, "add title and body head (for the local grading page only)")
	engine := flag.String("engine", "v2", "search engine: v2 only (the ported v1 was removed)")
	quoting := flag.Bool("quoting", false, "list the statements that name a query id of the gate (exclusion list), no search")
	flag.Parse()
	if *quoting {
		if err := listQuoting(*file); err != nil {
			fmt.Fprintln(os.Stderr, "searcheval:", err)
			os.Exit(1)
		}
		return
	}
	if *engine != "v2" {
		fmt.Fprintf(os.Stderr, "searcheval: -engine %q: only v2 exists\n", *engine)
		os.Exit(2)
	}
	if err := run(*file, *k, *actor, *show); err != nil {
		fmt.Fprintln(os.Stderr, "searcheval:", err)
		os.Exit(1)
	}
}

type quote struct {
	LegacyID    int64     `json:"legacy_id"`
	StatementID uuid.UUID `json:"statement_id"`
	Quotes      []string  `json:"quotes"` // the query ids it names
}

func readQueries(file string) ([]queryIn, error) {
	if file == "" {
		return nil, fmt.Errorf("-queries is required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var qs []queryIn
	if err := json.Unmarshal(raw, &qs); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return qs, nil
}

// listQuoting: imported statements (with a legacy id) whose title or body names a query id as a whole token
// (case-insensitive). Ordered by legacy id; ids only, no content.
func listQuoting(file string) error {
	qs, err := readQueries(file)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := pg.Pool(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	found := map[int64]*quote{}
	for _, in := range qs {
		// a whole token: not glued to a letter, digit or hyphen (m-5 is not m-57, o-6 is not foo-6)
		pat := `(^|[^[:alnum:]-])` + regexp.QuoteMeta(in.ID) + `($|[^[:alnum:]-])`
		rows, err := db.Query(sys, `SELECT s.statement_id, (j.payload->'imported_from'->>'observation_id')::bigint
		   FROM projection.statement s
		   JOIN journal j ON j.tenant_id = s.tenant_id AND j.seq = s.asserted_seq
		  WHERE j.payload ? 'imported_from' AND s.title || ' ' || COALESCE(s.body, '') ~* $1`, pat)
		if err != nil {
			return fmt.Errorf("%s: %w", in.ID, err)
		}
		for rows.Next() {
			var sid uuid.UUID
			var legacy int64
			if err := rows.Scan(&sid, &legacy); err != nil {
				rows.Close()
				return err
			}
			qt := found[legacy]
			if qt == nil {
				qt = &quote{LegacyID: legacy, StatementID: sid}
				found[legacy] = qt
			}
			qt.Quotes = append(qt.Quotes, in.ID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	out := make([]*quote, 0, len(found))
	for _, q := range found {
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LegacyID < out[j].LegacyID })
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	return enc.Encode(out)
}

func run(file string, k int, actor string, show bool) error {
	if actor == "" {
		return fmt.Errorf("-actor is required")
	}
	qs, err := readQueries(file)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := pg.Pool(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	var p query.Principal
	if err := db.QueryRow(sys, `SELECT tenant_id, actor_id FROM projection.actor WHERE kind = 'human' AND external_identity = $1`, actor).
		Scan(&p.TenantID, &p.ActorID); err != nil {
		return fmt.Errorf("actor %s: %w", actor, err)
	}
	q := &query.Service{DB: db}
	out := make([]result, 0, len(qs))
	for _, in := range qs {
		start := time.Now()
		r := result{ID: in.ID, Items: []hit{}}
		res, err := q.Search(ctx, p, query.SearchIn{Query: in.Text, Project: in.Project, Type: in.Type, Limit: k})
		r.Ms = time.Since(start).Milliseconds()
		if err != nil {
			r.Error = err.Error()
			out = append(out, r)
			continue
		}
		r.Relaxed, r.Coverage = res.Relaxed, res.Coverage
		for _, it := range res.Items {
			h := hit{StatementID: it.StatementID, Status: it.Status, Type: it.Type}
			if it.Match != nil {
				h.Coverage = it.Match.Coverage
			}
			var legacy int64
			// the source id of an imported statement: imported_from of its asserted event (as stand fix-ids reads it)
			if err := db.QueryRow(sys, `SELECT (j.payload->'imported_from'->>'observation_id')::bigint FROM projection.statement s
			        JOIN journal j ON j.tenant_id = s.tenant_id AND j.seq = s.asserted_seq
			       WHERE s.statement_id = $1 AND j.payload ? 'imported_from'`, it.StatementID).Scan(&legacy); err == nil {
				h.LegacyID = &legacy
			}
			if show {
				if err := db.QueryRow(sys, `SELECT title, left(COALESCE(body, ''), 600) FROM projection.statement WHERE statement_id = $1`,
					it.StatementID).Scan(&h.Title, &h.Head); err != nil {
					return err
				}
			}
			r.Items = append(r.Items, h)
		}
		out = append(out, r)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	return enc.Encode(out)
}
