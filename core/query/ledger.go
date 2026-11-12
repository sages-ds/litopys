package query

import (
	"context"
	"strings"
	"time"
)

// ledger.summary: the sums of the project's current records that carry an amount (expense,
// payment, repair cost, contract price), by currency and a grouping — no model adds them up. A record without a
// currency counts under "".

type LedgerIn struct {
	Project string     `json:"project"`
	Type    string     `json:"type,omitempty"`     // one stored type, e.g. household.expense; empty — every type with an amount
	Types   []string   `json:"-"`                  // what a short type resolved to
	GroupBy string     `json:"group_by,omitempty"` // type (default) | payee | month
	From    *time.Time `json:"date_from,omitempty"`
	To      *time.Time `json:"date_to,omitempty"`
}

type LedgerRow struct {
	Group    string  `json:"group"`
	Currency string  `json:"currency"`
	Total    float64 `json:"total"`
	Count    int     `json:"count"`
}

type LedgerOut struct {
	Rows   []LedgerRow `json:"rows"`
	Totals []LedgerRow `json:"totals"` // per currency, group ""
	Hidden []Hidden    `json:"hidden"`
}

// Ledger adds up amount (or cost / price) of the current records; the date is paid_at, else the record's effective time.
func (s *Service) Ledger(ctx context.Context, p Principal, in LedgerIn) (LedgerOut, error) {
	out := LedgerOut{Rows: []LedgerRow{}, Totals: []LedgerRow{}, Hidden: []Hidden{}}
	ctx, root, err := s.enter(ctx, p)
	if err != nil {
		return out, err
	}
	out.Hidden = hiddenOf(ctx)
	projectID, err := s.projectID(ctx, p, in.Project)
	if err != nil {
		return out, err
	}
	group := "s.type"
	switch in.GroupBy {
	case "", "type":
	case "payee":
		group = "COALESCE(s.fields->>'payee', s.fields->>'party', s.fields->>'by', '')"
	case "month":
		group = "to_char(" + ledgerDay + ", 'YYYY-MM')"
	default:
		return out, fail("invalid_input", "group_by: type | payee | month")
	}
	var a args
	where := []string{"s.project_id = " + a.add(projectID), mineSQL("s.actor_id", a.add(root)), "s.status = 'current'",
		"NOT s.hidden", "jsonb_typeof(" + ledgerAmount + ") = 'number'"}
	if len(in.Types) > 0 {
		where = append(where, "s.type = ANY("+a.add(in.Types)+"::text[])")
	} else if in.Type != "" {
		where = append(where, "s.type = "+a.add(in.Type))
	}
	if in.From != nil {
		where = append(where, ledgerDay+" >= "+a.add(*in.From)+"::date")
	}
	if in.To != nil {
		where = append(where, ledgerDay+" <= "+a.add(*in.To)+"::date")
	}
	rows, err := s.DB.Query(ctx, `SELECT `+group+`, COALESCE(s.fields->>'currency', ''), sum((`+ledgerAmount+`)::numeric)::float8, count(*)
		  FROM projection.statement s WHERE `+strings.Join(where, " AND ")+`
		 GROUP BY 1, 2 ORDER BY 2, 3 DESC`, a...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	totals := map[string]*LedgerRow{}
	var order []string
	for rows.Next() {
		var r LedgerRow
		if err := rows.Scan(&r.Group, &r.Currency, &r.Total, &r.Count); err != nil {
			return out, err
		}
		out.Rows = append(out.Rows, r)
		t, ok := totals[r.Currency]
		if !ok {
			t = &LedgerRow{Currency: r.Currency}
			totals[r.Currency] = t
			order = append(order, r.Currency)
		}
		t.Total += r.Total
		t.Count += r.Count
	}
	for _, c := range order {
		out.Totals = append(out.Totals, *totals[c])
	}
	return out, rows.Err()
}

const (
	ledgerAmount = "COALESCE(s.fields->'amount', s.fields->'cost', s.fields->'price')"
	ledgerDay    = "COALESCE((s.fields->>'paid_at')::date, (s.effective_at AT TIME ZONE 'Europe/Kyiv')::date)"
)
