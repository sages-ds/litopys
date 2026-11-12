package api

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/vocab"
)

// graph draws the links between the records of a project as an inline SVG: no script, so
// the strict CSP of the viewer stays. Nodes on a circle ordered by time, the most connected ones larger; a
// node is a link to its record. At most 160 records with links.
func (a *API) graph(ctx context.Context, c *Caller, project string) (template.HTML, int, int, error) {
	type node struct {
		id     uuid.UUID
		title  string
		typ    string
		at     string
		degree int
	}
	nodes := map[uuid.UUID]*node{}
	type edge struct {
		from, to uuid.UUID
		rel      string
	}
	var edges []edge
	rows, err := a.DB.Query(ctx, `
		SELECT f.statement_id, f.title, f.type, f.effective_at::text, t.statement_id, t.title, t.type, t.effective_at::text, k.relation
		  FROM projection.link k
		  JOIN projection.statement f ON f.statement_id = k.from_id
		  JOIN projection.statement t ON t.statement_id = k.to_id
		  JOIN projection.project p ON p.project_id = f.project_id
		 WHERE p.tenant_id = $1 AND p.name = $2 AND NOT k.retired AND f.status <> 'retracted' AND t.status <> 'retracted' AND NOT f.hidden AND NOT t.hidden
		 LIMIT 600`, c.TenantID, project)
	if err != nil {
		return "", 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var fn, tn node
		var rel string
		if err := rows.Scan(&fn.id, &fn.title, &fn.typ, &fn.at, &tn.id, &tn.title, &tn.typ, &tn.at, &rel); err != nil {
			return "", 0, 0, err
		}
		for _, x := range []node{fn, tn} {
			if nodes[x.id] == nil {
				if len(nodes) >= 160 {
					continue
				}
				cp := x
				nodes[x.id] = &cp
			}
		}
		if nodes[fn.id] != nil && nodes[tn.id] != nil {
			nodes[fn.id].degree++
			nodes[tn.id].degree++
			edges = append(edges, edge{fn.id, tn.id, rel})
		}
	}
	if err := rows.Err(); err != nil {
		return "", 0, 0, err
	}
	if len(nodes) == 0 {
		return "", 0, 0, nil
	}
	list := make([]*node, 0, len(nodes))
	for _, n := range nodes {
		list = append(list, n)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at < list[j].at })
	const size, margin = 900.0, 70.0
	r := size/2 - margin
	pos := map[uuid.UUID][2]float64{}
	for i, n := range list {
		ang := 2*math.Pi*float64(i)/float64(len(list)) - math.Pi/2
		pos[n.id] = [2]float64{size/2 + r*math.Cos(ang), size/2 + r*math.Sin(ang)}
	}
	esc := template.HTMLEscapeString
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="links of %s" style="width:100%%;height:auto;max-width:900px">`, size, size, esc(project))
	for _, e := range edges {
		p, q := pos[e.from], pos[e.to]
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="currentColor" stroke-opacity=".25"><title>%s</title></line>`,
			p[0], p[1], q[0], q[1], esc(e.rel))
	}
	// colours by family: dba.migration is an event like bugfix, exec.board-decision a decision
	colors := map[string]string{"decision": "#0b5cad", "document": "#8250df", "finding": "#1a7f37", "proposal": "#bf8700",
		"event": "#cf222e", "question": "#1b7c83"}
	for _, n := range list {
		p := pos[n.id]
		family, _ := vocab.FamilyOf(n.typ)
		col := colors[family]
		if col == "" {
			col = "#6e7781"
		}
		rad := 4 + math.Min(10, float64(n.degree))
		fmt.Fprintf(&b, `<a href="/viewer/s/%s"><circle cx="%.1f" cy="%.1f" r="%.1f" stroke="var(--fg)" stroke-opacity=".35" fill="%s"><title>%s · %s · %s</title></circle></a>`,
			n.id, p[0], p[1], rad, col, esc(n.title), esc(n.typ), esc(n.at[:10]))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()), len(list), len(edges), nil //nolint:gosec // every text is escaped above
}
