package embed

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DB is what the pass needs of a pool (system scope is the caller's).
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Pass embeds up to batch current records of the tenant that have no vector for their revision and model, within
// MaxClass; it returns how many it embedded.
func (c *Client) Pass(ctx context.Context, db DB, tenant uuid.UUID, batch int) (int, error) {
	rows, err := db.Query(ctx, `
		SELECT s.statement_id, s.revision, s.title, s.body
		  FROM projection.statement s JOIN projection.project p ON p.project_id = s.project_id
		  LEFT JOIN derived.statement_embedding e ON e.statement_id = s.statement_id
		 WHERE s.tenant_id = $1 AND s.status = 'current' AND NOT s.hidden AND s.type <> 'outcome'
		   AND (e.statement_id IS NULL OR e.revision <> s.revision OR e.model <> $2)
		   AND CASE COALESCE(s.data_class, p.data_class) WHEN 'public' THEN 0 WHEN 'internal' THEN 1 ELSE 2 END
		       <= CASE $3 WHEN 'public' THEN 0 WHEN 'internal' THEN 1 ELSE 2 END
		 ORDER BY s.asserted_seq DESC LIMIT $4`, tenant, c.Model, c.MaxClass, batch)
	if err != nil {
		return 0, err
	}
	type rec struct {
		id       uuid.UUID
		revision int
		text     string
	}
	var recs []rec
	for rows.Next() {
		var r rec
		var title string
		var body *string
		if err := rows.Scan(&r.id, &r.revision, &title, &body); err != nil {
			rows.Close()
			return 0, err
		}
		r.text = Text(title, body)
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(recs) == 0 {
		return 0, err
	}
	texts := make([]string, len(recs))
	for i, r := range recs {
		texts[i] = r.text
	}
	vecs, err := c.Embed(ctx, texts)
	if err != nil {
		return 0, err
	}
	for i, r := range recs {
		var at any
		if err := db.QueryRow(ctx, `
			INSERT INTO derived.statement_embedding (statement_id, tenant_id, revision, model, vec) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (statement_id) DO UPDATE SET revision = $3, model = $4, vec = $5, embedded_at = now()
			RETURNING embedded_at`, r.id, tenant, r.revision, c.Model, vecs[i]).Scan(&at); err != nil {
			return i, err
		}
	}
	return len(recs), nil
}
