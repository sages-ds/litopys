package query

import (
	"context"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Meaning search: beside the lexical ranks, the records closest to the query by their vectors
// (derived.statement_embedding, cosine of normalised vectors), merged by reciprocal rank fusion. A record only the
// vectors found carries match.coverage "semantic": the words differ, the meaning is near ("countertop price" →
// «стільниця»). Off without an embedder (LITOPYS_EMBED_URL).

// Embedder turns a query into a normalised vector of the model the records were embedded with.
type Embedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	ModelName() string
}

// MinSimilarity: below it a vector neighbour is noise, not meaning (bge-m3: related texts are above ~0.5).
var MinSimilarity = 0.45

// vectorRows: up to limit records under the same filters as the lexical search, nearest first, with their similarity.
func (s *Service) vectorRows(ctx context.Context, p Principal, root uuid.UUID, in SearchIn, q []float32, limit int) ([]IndexRow, error) {
	var a args
	where, _ := searchFilters(in, p, root, &a)
	model := a.add(s.Embedder.ModelName())
	vec := a.add(q)
	sim := "(SELECT sum(x * y) FROM unnest(e.vec, " + vec + "::real[]) AS t(x, y))"
	sql := `SELECT ` + indexCols + provCols("j") + `, ` + sim + `::real, NULL::text
		  FROM projection.statement s
		  JOIN projection.project p ON p.project_id = s.project_id
		  JOIN derived.statement_embedding e ON e.statement_id = s.statement_id AND e.revision = s.revision AND e.model = ` + model +
		provJoin("j", "s.tenant_id", "s.last_seq") + `
		 WHERE ` + strings.Join(where, " AND ") + ` AND ` + sim + ` >= ` + a.add(MinSimilarity) + `
		 ORDER BY 1 DESC LIMIT ` + a.add(limit)
	// ORDER BY the similarity column: indexCols starts with the id, so name the expression instead
	sql = strings.Replace(sql, "ORDER BY 1 DESC", "ORDER BY "+sim+" DESC", 1)
	return s.collectIndex(ctx, sql, a)
}

// fuse merges the lexical list and the vector list by reciprocal rank fusion, keeping limit rows; rows the vectors
// alone found get match.coverage "semantic".
func fuse(lexical, vector []IndexRow, limit int) []IndexRow {
	score := map[uuid.UUID]float64{}
	row := map[uuid.UUID]IndexRow{}
	for i, r := range lexical {
		score[r.StatementID] += 1 / float64(rrfK+i+1)
		row[r.StatementID] = r
	}
	for i, r := range vector {
		score[r.StatementID] += 1 / float64(rrfK+i+1)
		if _, ok := row[r.StatementID]; !ok {
			r.Match = &Match{Coverage: "semantic"}
			row[r.StatementID] = r
		}
	}
	ids := make([]uuid.UUID, 0, len(row))
	for id := range row {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool { return score[ids[i]] > score[ids[j]] })
	out := make([]IndexRow, 0, min(limit, len(ids)))
	for _, id := range ids {
		if len(out) == limit {
			break
		}
		out = append(out, row[id])
	}
	return out
}
