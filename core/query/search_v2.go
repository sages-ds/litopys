package query

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/textnorm"
)

// Search v2 over projection.statement_search, the search engine of recall.search.
// The query goes through core/textnorm.Analyze — the function every record went through
// when it was indexed — and two channels rank the matches:
//   - identifiers: rows sharing canonical identifiers with the query (r-498, t-44, hp_tools, 3b80ed5), ranked by
//     how many they share;
//   - stems: rows sharing uk/en stems, ranked by the summed BM25 IDF of the query's stems they have (x6) — all
//     words first, a partial match lower in the same channel, a rare word above a common one, no second phase and
//     no switch to OR;
//   - typo variants (x9): a word of the query that no record of the person holds meets its nearest stem of the
//     person's records by trigrams — as typed or, if Cyrillic, in its official Latin transliteration (x11) —
//     ranked the same way as stems; before that, a word typed in the wrong keyboard layout stands for its retyped
//     form when the person's records hold that stem exactly;
//   - names of letters and a number: «win11» also meets the records holding both «win» and «11», ranked by how
//     many such names they hold; «win 11» also asks for the identifier win11 when the person's records hold it.
// The channels are fused by Reciprocal Rank Fusion with the published constant k = 60 (Cormack, Clarke, Büttcher,
// SIGIR 2009); nothing is tuned. Rows matching an identifier of the query form a tier above the fusion. Within a
// channel, a tie goes to the row whose title carries the match (x7, 0026); in the identifier tier, then to the row
// naming the identifiers more often (x8, 0027); then to recency. Noise — legacy hook drafts and todo snapshots — is out by default and counted.
// Texts are Ukrainian and English only — NO RUSSIAN SUPPORT.

// rrfK is the RRF constant of the paper, not a tuned value.
const rrfK = 60

// noiseSQL: the legacy noise of the imported corpus, found by its titles. Until the write lanes and vocabularies mark
// such records at the source, the titles are the only signal; IncludeNoise shows them.
const noiseSQL = `(s.title LIKE 'Чернетка: правки %' OR s.title LIKE 'Стан todo — %' OR s.title LIKE 'Стан todo - %')`

// searchV2 returns the rows, the noise it hid, the words no record holds, and whether the query had anything to search.
func (s *Service) searchV2(ctx context.Context, p Principal, root uuid.UUID, in SearchIn) ([]IndexRow, int, []string, bool, error) {
	var a args
	where, tie := searchFilters(in, p, root, &a)
	from := `
		  FROM projection.statement s
		  JOIN projection.project p ON p.project_id = s.project_id
		  JOIN projection.statement_search x ON x.statement_id = s.statement_id`
	limit := clamp(in.Limit, 20, 1, 50)
	// the frame of the question («нагадай», «що вирішили щодо», "what did we decide about") says what kind of answer
	// is wanted, not what it is about: it is taken off before the words are weighed (K1)
	q, _ := textnorm.StripFrame(in.Query)
	d := textnorm.Analyze(q)
	stems := uniq(d.Stems)
	// the words of the query, weighed by IDF; a word no record of this person holds (df = 0) is taken for a typo and
	// meets its nearest stem of the person's records instead (x9) — a channel of its own
	ws := uniq(d.WordStems)
	var idf, df, vidf []float64
	var variants []string
	variantOf := map[string]string{}
	if len(ws) > 0 {
		var err error
		if idf, df, err = s.stemIDF(ctx, p, root, ws); err != nil {
			return nil, 0, nil, false, err
		}
		var unknown []string
		for i, w := range ws {
			if df[i] == 0 {
				unknown = append(unknown, w)
			}
		}
		var swapped []string
		var swappedOf map[string]string
		if swapped, swappedOf, unknown, err = s.layoutVariants(ctx, p, root, d, unknown); err != nil {
			return nil, 0, nil, false, err
		}
		if variants, variantOf, err = s.stemVariants(ctx, p, root, unknown); err != nil {
			return nil, 0, nil, false, err
		}
		variants = uniq(append(swapped, variants...))
		for w, v := range swappedOf {
			variantOf[w] = v
		}
		if len(variants) > 0 {
			if vidf, _, err = s.stemIDF(ctx, p, root, variants); err != nil {
				return nil, 0, nil, false, err
			}
		}
	}

	// a name of letters and a number in its other spellings: «win 11» also finds the identifier win11 when the
	// person's records hold it, and «win11» finds the records holding both «win» and «11»
	names := textnorm.AlnumVariants(q)
	aliases, err := s.heldIdents(ctx, p, root, names, d.Idents)
	if err != nil {
		return nil, 0, nil, false, err
	}

	// the match condition first: the noise count below runs with exactly these parameters
	var match, qi string
	var arms []string
	if idents := append(append([]string{}, d.Idents...), aliases...); len(idents) > 0 {
		qi = a.add(idents)
		arms = append(arms, "x.idents && "+qi+"::text[]")
	}
	if all := append(append([]string{}, stems...), variants...); len(all) > 0 {
		arms = append(arms, "x.stems @@ "+a.add(orQuery(all))+"::tsquery")
	}
	// a name spelled apart: the record holds both parts as stems, side by side in its text — the stems alone do not
	// say so, and «win» in one line with «11» in another is not the name (x16, the first run on train)
	var apart []string
	for _, n := range names {
		apart = append(apart, "(x.stems @@ "+a.add(andQuery(n.Parts))+"::tsquery AND (coalesce(s.title, '') || ' ' || coalesce(s.body, '')) ~* "+
			a.add(apartPattern(n.Runs))+")")
	}
	if len(apart) > 0 {
		arms = append(arms, "("+strings.Join(apart, " OR ")+")")
	}
	if len(arms) > 0 {
		match = "(" + strings.Join(arms, " OR ") + ")"
	}

	noise := 0
	if !in.IncludeNoise {
		// how much noise this query would have shown — the answer says what it hides
		cw := append(append([]string{}, where...), noiseSQL)
		if match != "" {
			cw = append(cw, match)
		}
		if err := s.DB.QueryRow(ctx, "SELECT count(*)"+from+" WHERE "+strings.Join(cw, " AND "), a...).Scan(&noise); err != nil {
			return nil, 0, nil, false, err
		}
		where = append(where, "NOT "+noiseSQL)
	}

	snippet := "NULL::text"
	if in.IncludeSnippet && in.Query != "" {
		snippet = "ts_headline('simple', coalesce(s.body, ''), plainto_tsquery('simple', " + a.add(textnorm.Normalize(in.Query)) + "), " +
			"'MaxFragments=1, MaxWords=22, MinWords=8, StartSel=«, StopSel=», FragmentDelimiter= … ')"
	}
	if match == "" { // no query, or nothing searchable in it: the filtered list by recency
		sql := `SELECT ` + indexCols + provCols("j") + `, NULL::real, ` + snippet + from + provJoin("j", "s.tenant_id", "s.last_seq") +
			` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + tie + ` LIMIT ` + a.add(limit)
		items, err := s.collectIndex(ctx, sql, a)
		return items, noise, nil, false, err
	}
	// n* — how well the whole record matches; t* — the same for its title alone, which only breaks ties (x7)
	// fi — how often the record names the query's identifiers (x8), the next tie-break after the title
	// nv, tv — the same as ns, ts for the typo variants (x9)
	// np — how many of the query's names the record spells apart; a record spelling it joined is already in the
	// identifier tier, and its evidence is counted once, there (x8)
	ni, ti, fi, ns, ts, nv, tv, np := "0", "0", "0", "0", "0", "0", "0", "0"
	if len(names) > 0 {
		var terms []string
		for i, n := range names {
			terms = append(terms, "("+apart[i]+" AND NOT x.idents && "+a.add(n.Joined)+"::text[])::int")
		}
		np = "(" + strings.Join(terms, " + ") + ")"
	}
	if qi != "" {
		ni, ti = overlap("x.idents", qi), overlap("x.title_idents", qi)
		fi = "(SELECT coalesce(sum(f.tf), 0) FROM unnest(x.idents, x.ident_tf) f(id, tf) WHERE f.id = ANY(" + qi + "::text[]))"
	}
	// the words channel ranks by the query's words only: an identifier's own stems ("setuperr", "log") still match,
	// but its evidence is counted once, in the identifier tier (x8)
	if len(ws) > 0 {
		qs, qw := a.add(ws), a.add(idf)
		ns, ts = idfSum(qs, qw, "tsvector_to_array(x.stems)"), idfSum(qs, qw, "x.title_stems")
	}
	if len(variants) > 0 {
		qs, qw := a.add(variants), a.add(vidf)
		nv, tv = idfSum(qs, qw, "tsvector_to_array(x.stems)"), idfSum(qs, qw, "x.title_stems")
	}
	// ranks within each channel; a row outside a channel contributes nothing from it. Of two rows matching equally,
	// the one whose title carries the match ranks first, then the newer — never the title over a fuller match (x5).
	ri := "CASE WHEN m.ni > 0 THEN rank() OVER (ORDER BY m.ni DESC, m.ti DESC, m.fi DESC, " + tie + ") END"
	rs := "CASE WHEN m.ns > 0 THEN rank() OVER (ORDER BY m.ns DESC, m.ts DESC, " + tie + ") END"
	rv := "CASE WHEN m.nv > 0 THEN rank() OVER (ORDER BY m.nv DESC, m.tv DESC, " + tie + ") END"
	rp := "CASE WHEN m.np > 0 THEN rank() OVER (ORDER BY m.np DESC, " + tie + ") END"
	k := a.add(rrfK)
	score := "(coalesce(1.0 / (" + k + " + " + ri + "), 0) + coalesce(1.0 / (" + k + " + " + rs + "), 0) + " +
		"coalesce(1.0 / (" + k + " + " + rv + "), 0) + coalesce(1.0 / (" + k + " + " + rp + "), 0))::real"
	sql := `SELECT ` + indexCols + provCols("j") + `, ` + score + `, ` + snippet + from + `
		  CROSS JOIN LATERAL (SELECT ` + ni + ` AS ni, ` + ti + ` AS ti, ` + fi + ` AS fi, ` + ns + ` AS ns, ` + ts + ` AS ts, ` +
		nv + ` AS nv, ` + tv + ` AS tv, ` + np + ` AS np) m` + provJoin("j", "s.tenant_id", "s.last_seq") + `
		 WHERE ` + strings.Join(append(where, match), " AND ") + `
		 ORDER BY (m.ni > 0) DESC, ` + score + ` DESC, ` + tie + ` LIMIT ` + a.add(limit)
	items, err := s.collectIndex(ctx, sql, a)
	if err != nil {
		return nil, 0, nil, false, err
	}
	unknownWords, err := s.markMatches(ctx, items, d, ws, df, variantOf, names)
	return items, noise, unknownWords, true, err
}

// heldIdents: the joined spellings of the query's names that are not already identifiers of the query and
// that one of the person's live records holds — the same rule as layoutVariants: no record, no variant.
func (s *Service) heldIdents(ctx context.Context, p Principal, root uuid.UUID, names []textnorm.Alnum, have []string) ([]string, error) {
	in := map[string]bool{}
	for _, id := range have {
		in[id] = true
	}
	var cand []string
	for _, n := range names {
		for _, j := range n.Joined {
			if !in[j] {
				cand = append(cand, j)
			}
		}
	}
	if len(cand) == 0 {
		return nil, nil
	}
	var held []string
	err := s.DB.QueryRow(ctx, `
		SELECT coalesce(array_agg(c ORDER BY c), '{}') FROM unnest($1::text[]) c
		 WHERE EXISTS (SELECT 1 FROM projection.statement s JOIN projection.statement_search x ON x.statement_id = s.statement_id
		                WHERE x.idents @> ARRAY[c] AND `+mineWhere+`)`, uniq(cand), p.TenantID, root).Scan(&held)
	return held, err
}

// stemIDF (x6): the BM25 inverse document frequency of each query stem, ln(1 + (N − df + 0.5) / (df + 0.5))
// (Robertson & Zaragoza, 2009), over the principal's own live records — N of them, df holding the stem; another
// person's words never weigh in. The stems channel sums it over the stems a row holds, so one common word no longer
// counts as much as a rare one (x1, x4 and x5 lost to exactly that). A published formula, no constant tuned.
// It also gives each stem's df: 0 marks a word none of the person's records holds (x9).
func (s *Service) stemIDF(ctx context.Context, p Principal, root uuid.UUID, stems []string) (idf, df []float64, err error) {
	lex := make([]string, len(stems))
	for i, w := range stems {
		lex[i] = orQuery([]string{w}) // one quoted lexeme: no character of the stem is tsquery syntax
	}
	err = s.DB.QueryRow(ctx, `
		WITH `+mineCTE+`
		SELECT array_agg(ln(1 + (n.total - d.df + 0.5) / (d.df + 0.5)) ORDER BY d.ord), array_agg(d.df ORDER BY d.ord)
		  FROM (SELECT count(*)::float8 AS total FROM mine) n
		 CROSS JOIN LATERAL (
		       SELECT u.ord, (SELECT count(*) FROM mine y WHERE y.stems @@ u.q::tsquery)::float8 AS df
		         FROM unnest($1::text[]) WITH ORDINALITY u(q, ord)) d
		 GROUP BY n.total`, lex, p.TenantID, root).Scan(&idf, &df)
	return idf, df, err
}

// mineWhere: s is one of the principal's own live records ($2 tenant, $3 root actor).
var mineWhere = `s.tenant_id = $2 AND ` + mineSQL("s.actor_id", "$3") + ` AND s.status <> 'retracted' AND NOT s.hidden`

// mineCTE: the search rows of the principal's own live records.
var mineCTE = `mine AS (
		  SELECT x.stems FROM projection.statement s JOIN projection.statement_search x ON x.statement_id = s.statement_id
		   WHERE ` + mineWhere + `)`

// layoutVariants: an unknown word typed in the wrong keyboard layout («vsuhfwsz» for «міграція», «щзутифщ» for
// openbao) is retyped in the other one; the retyped word stands for it only when one of the person's live records
// holds its stem exactly — no similarity, no threshold, so a word that merely is not in the records is never swapped
// into another one. The rest stay unknown and go on to the typo variants.
func (s *Service) layoutVariants(ctx context.Context, p Principal, root uuid.UUID, d textnorm.Doc, unknown []string) (variants []string, of map[string]string, rest []string, err error) {
	of = map[string]string{}
	word := map[string]string{}
	for i, st := range d.WordStems {
		if _, ok := word[st]; !ok && i < len(d.Words) {
			word[st] = d.Words[i]
		}
	}
	var from, cand []string
	for _, st := range unknown {
		if w := textnorm.SwapLayout(word[st]); w != word[st] {
			if ws := textnorm.Analyze(w).WordStems; len(ws) == 1 {
				from, cand = append(from, st), append(cand, ws[0])
			}
		}
	}
	if len(cand) == 0 {
		return nil, of, unknown, nil
	}
	_, df, err := s.stemIDF(ctx, p, root, cand)
	if err != nil {
		return nil, nil, nil, err
	}
	for i, st := range from {
		if df[i] > 0 {
			of[st], variants = cand[i], append(variants, cand[i])
		}
	}
	for _, st := range unknown {
		if of[st] == "" {
			rest = append(rest, st)
		}
	}
	return uniq(variants), of, rest, nil
}

// stemVariants (x9): for each unknown stem, the nearest stem of the person's own records by trigram similarity —
// pg_trgm's % with its documented default threshold (pg_trgm.similarity_threshold, 0.3), not one of ours; the most
// similar wins, ties by code point. Only words no record holds come here, so a known word is never "corrected".
// It also says which unknown stem each variant stands for. The candidates come from the trigram index of stem_vocab
// (0028) — a superset of the stems — and only a stem one of the person's live records holds may win.
func (s *Service) stemVariants(ctx context.Context, p Principal, root uuid.UUID, unknown []string) ([]string, map[string]string, error) {
	of := map[string]string{}
	if len(unknown) == 0 {
		return nil, of, nil
	}
	// each unknown word is tried as typed and, if Cyrillic, in its official Latin transliteration (x11: «опенбао» is
	// openbao typed in Cyrillic); of all its forms the most similar stem wins
	var forms []string
	var idx []int
	for i, w := range unknown {
		forms, idx = append(forms, w), append(idx, i)
		if l := textnorm.Latin(w); l != w {
			forms, idx = append(forms, l), append(idx, i)
		}
	}
	// the stem as one quoted lexeme, as orQuery quotes it
	lexeme := `('''' || replace(replace(c.stem, '\', '\\'), '''', '''''') || '''')::tsquery`
	rows, err := s.DB.Query(ctx, `
		SELECT DISTINCT ON (u.i) u.i, v.t FROM unnest($1::text[], $4::int[]) u(w, i)
		 CROSS JOIN LATERAL (
		       SELECT c.stem AS t, similarity(c.stem, u.w) AS sim FROM projection.stem_vocab c
		        WHERE c.tenant_id = $2 AND c.stem % u.w
		          AND EXISTS (SELECT 1 FROM projection.statement s JOIN projection.statement_search x ON x.statement_id = s.statement_id
		                       WHERE x.stems @@ `+lexeme+` AND `+mineWhere+`)
		        ORDER BY similarity(c.stem, u.w) DESC, c.stem COLLATE "C" LIMIT 1) v
		 ORDER BY u.i, v.sim DESC, v.t COLLATE "C"`,
		forms, p.TenantID, root, idx)
	if err != nil {
		return nil, of, err
	}
	var out []string
	for rows.Next() {
		var i int
		var t string
		if err := rows.Scan(&i, &t); err != nil {
			return nil, of, err
		}
		out, of[unknown[i]] = append(out, t), t
	}
	return uniq(out), of, rows.Err()
}

// markMatches: sets Match on every row — the query's identifiers and words it holds and misses, a word
// counted as held through its typo variant — and returns the words no record holds at all (df = 0, no variant).
// One extra read of the rows' search projection; the ranking is not touched.
// A record spelling a name of the query another way (win11 for «win 11», «win 11» for win11) holds all its parts.
func (s *Service) markMatches(ctx context.Context, items []IndexRow, d textnorm.Doc, ws []string, df []float64, variantOf map[string]string, names []textnorm.Alnum) ([]string, error) {
	word := map[string]string{} // stem → the word as typed (the first one)
	for i, st := range d.WordStems {
		if _, ok := word[st]; !ok && i < len(d.Words) {
			word[st] = d.Words[i]
		}
	}
	var unknown []string
	for i, st := range ws {
		if df[i] == 0 && variantOf[st] == "" {
			unknown = append(unknown, word[st])
		}
	}
	if len(items) == 0 {
		return unknown, nil
	}
	ids := make([]uuid.UUID, len(items))
	for i, r := range items {
		ids[i] = r.StatementID
	}
	rows, err := s.DB.Query(ctx, `SELECT statement_id, idents, tsvector_to_array(stems) FROM projection.statement_search
		 WHERE statement_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	held := map[uuid.UUID]map[string]bool{}
	for rows.Next() {
		var id uuid.UUID
		var idents, stems []string
		if err := rows.Scan(&id, &idents, &stems); err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, t := range append(idents, stems...) {
			set[t] = true
		}
		for _, n := range names {
			spelled := set[n.Joined[0]] || set[n.Joined[1]]
			apart := true
			for _, pt := range n.Parts {
				apart = apart && set[pt]
			}
			if spelled || apart {
				for _, t := range append(append([]string{}, n.Joined...), n.Parts...) {
					set[t] = true
				}
			}
		}
		held[id] = set
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range items {
		set := held[items[i].StatementID]
		m := &Match{Coverage: "full"}
		for j, id := range d.Idents {
			form := id
			if j < len(d.IdentForms) && d.IdentForms[j] != "" {
				form = d.IdentForms[j]
			}
			if !strings.EqualFold(form, id) { // "25H2" is not another form of 25h2: no canonical for case alone
				if m.Canonical == nil {
					m.Canonical = map[string]string{}
				}
				m.Canonical[form] = id
			}
			if set[id] {
				m.Matched = append(m.Matched, form)
			} else {
				m.Missing = append(m.Missing, form)
			}
		}
		for _, st := range ws {
			if set[st] || variantOf[st] != "" && set[variantOf[st]] {
				m.Matched = append(m.Matched, word[st])
			} else {
				m.Missing = append(m.Missing, word[st])
			}
		}
		if len(m.Missing) > 0 {
			m.Coverage = "partial"
		}
		items[i].Match = m
	}
	return unknown, nil
}

// idfSum: the summed IDF of the query stems (param qs, weights qw) that the row's array col holds.
func idfSum(qs, qw, col string) string {
	return "(SELECT coalesce(sum(w.idf), 0) FROM unnest(" + qs + "::text[], " + qw + "::float8[]) w(stem, idf) WHERE w.stem = ANY(" + col + "))"
}

// overlap: how many of the query's terms the row's array holds.
func overlap(col, param string) string {
	return "cardinality(ARRAY(SELECT unnest(" + col + ") INTERSECT SELECT unnest(" + param + "::text[])))"
}

// orQuery: a tsquery of the stems joined by OR; every stem is a quoted lexeme, so no character of it is syntax.
func orQuery(stems []string) string {
	q := make([]string, len(stems))
	for i, w := range stems {
		q[i] = "'" + strings.ReplaceAll(strings.ReplaceAll(w, `\`, `\\`), "'", "''") + "'"
	}
	return strings.Join(q, " | ")
}

// apartPattern: a PostgreSQL regular expression for the runs of a name written apart — by spaces, an underscore or a
// hyphen of any kind — as whole words («Win 11», «Win-11», not «darwin 110»). The runs are letters or digits only.
func apartPattern(runs []string) string {
	return `(^|[^[:alnum:]])` + strings.Join(runs, `[-_[:space:]‐‑‒–—]{1,3}`) + `([^[:alnum:]]|$)`
}

// andQuery: the same, joined by AND.
func andQuery(stems []string) string {
	return strings.ReplaceAll(orQuery(stems), " | ", " & ")
}
