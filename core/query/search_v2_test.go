package query_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/migrate"
	"litopys.smart-gw.co.ua/core/query"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
	"litopys.smart-gw.co.ua/internal/pgtest"
)

// TestSearchV2: word forms meet, a reference finds what it names and not prose
// numbers, identifiers form a tier above words, all words rank above some, noise is out by default and counted,
// a mixed-script code is found, another person's records stay invisible.
func TestSearchV2(t *testing.T) {
	ctx := context.Background()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	db := pgtest.New(t)
	if _, err := migrate.Up(ctx, db.MigratorConn(t)); err != nil {
		t.Fatal(err)
	}
	app := db.RolePool(t, "litopys_app", 4)
	v := &verbs.Service{DB: app}
	q := &query.Service{DB: app}
	var tenant uuid.UUID
	must(t, "tenant", app.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant))
	ada, _, err := v.RegisterHuman(ctx, tenant, "Ada", "ada@example")
	must(t, "human", err)
	other, _, err := v.RegisterHuman(ctx, tenant, "Other", "other@example")
	must(t, "other", err)

	write := func(who uuid.UUID, title, body string) uuid.UUID {
		t.Helper()
		me := verbs.Principal{TenantID: tenant, ActorID: who}
		open, err := v.ContextOpen(ctx, me, verbs.ContextOpenIn{Project: "litopys", Attach: "none", AutoCreate: true})
		must(t, "open", err)
		me.RunID = open.RunID
		out, err := v.StatementAssert(ctx, me, verbs.StatementAssertIn{Project: "litopys", Type: "note", Title: title, Body: ptr(body), ContextID: &open.ContextID})
		must(t, "assert "+title, err)
		return out.StatementID
	}
	mig := write(ada, "Перенесли міграцію на новий пул", "Пул fastpool, без простою.")
	task44 := write(ada, "Realm у Keycloak", "Закрито #44: scopes і DCR.")
	prose := write(ada, "Опис домену", "44 події, 62 дієслова, 31 файл.")
	ref := write(ada, "Огляд вимірів", "Доповнює R-498 і уточнює висновки.")
	both := write(ada, "Оновлення Windows не вдалось", "BitLocker заважав оновленню.")
	part := write(ada, "Windows на ноуті", "Звичайні нотатки.")
	draft := write(ada, "Чернетка: правки 3 файл. — a.go", "Автоматична чернетка про міграції.")
	code := write(ada, "Розділ HP_TOOLS", "FAT32 заважав.")
	foreign := write(other, "Міграції чужого пулу", "Не моє.")
	me := query.Principal{TenantID: tenant, ActorID: ada}

	search := func(qs string, include bool) query.SearchOut {
		t.Helper()
		out, err := q.Search(ctx, me, query.SearchIn{Query: qs, IncludeNoise: include, Limit: 20})
		must(t, "search "+qs, err)
		return out
	}
	first := func(out query.SearchOut) uuid.UUID {
		if len(out.Items) == 0 {
			return uuid.Nil
		}
		return out.Items[0].StatementID
	}

	// word forms meet; another person's record is invisible; the draft is noise, counted
	r := search("міграції пулу", false)
	if got := ids(r.Items); !got[mig] || got[foreign] || got[draft] || r.NoiseHidden != 1 {
		t.Errorf("міграції пулу: %d items, noise %d (mig %v, foreign %v, draft %v)", len(r.Items), r.NoiseHidden, got[mig], got[foreign], got[draft])
	}
	if got := ids(search("міграції пулу", true).Items); !got[draft] {
		t.Error("include_noise must show the draft")
	}
	// a task reference finds "#44", not "44 події"
	r = search("#44", false)
	if got := ids(r.Items); first(r) != task44 || got[prose] {
		t.Errorf("#44: first %v, prose found %v", first(r) == task44, got[prose])
	}
	// a record reference in either case meets (the previous service's forms are rewritten by its import)
	for _, form := range []string{"R-498", "r-498"} {
		r := search(form, false)
		if first(r) != ref {
			t.Errorf("%s must find the record that mentions R-498", form)
			continue
		}
		// the row names the identifier as typed, the canonical form beside it
		m := r.Items[0].Match
		want := "" // case alone is not another form
		if m == nil || len(m.Matched) != 1 || m.Matched[0] != form || m.Canonical[form] != want {
			t.Errorf("%s: match %+v, want matched [%s] and canonical %q", form, m, form, want)
		}
	}
	// another form is no number: "rec 498" does not name R-498
	if r := search("rec 498", false); first(r) == ref {
		t.Errorf("rec 498 reads as R-498")
	}
	// all words rank above some words
	r = search("оновлення windows bitlocker", false)
	if first(r) != both || !ids(r.Items)[part] {
		t.Errorf("all words first, a partial match still listed: first both %v, part listed %v", first(r) == both, ids(r.Items)[part])
	}
	// honest results: each row says what it holds and misses, the answer how well the list covers the query
	match := func(out query.SearchOut, id uuid.UUID) *query.Match {
		for _, it := range out.Items {
			if it.StatementID == id {
				return it.Match
			}
		}
		return nil
	}
	if m := match(r, both); m == nil || m.Coverage != "full" || len(m.Missing) != 0 || len(m.Matched) != 3 {
		t.Errorf("the full match: %+v", m)
	}
	if m := match(r, part); m == nil || m.Coverage != "partial" || len(m.Missing) != 2 || m.Matched[0] != "windows" {
		t.Errorf("the partial match must name what it misses: %+v", m)
	}
	if r.Coverage != "full" || r.Relaxed || len(r.UnknownWords) != 0 {
		t.Errorf("a list with a full match: coverage %q, relaxed %v, unknown %v", r.Coverage, r.Relaxed, r.UnknownWords)
	}
	// a word no record holds: the list is partial, relaxed, and says which word nothing holds
	r = search("windows ксилофонщик", false)
	if r.Coverage != "partial" || !r.Relaxed || len(r.UnknownWords) != 1 || r.UnknownWords[0] != "ксилофонщик" {
		t.Errorf("unknown word: coverage %q, relaxed %v, unknown %v", r.Coverage, r.Relaxed, r.UnknownWords)
	}
	// x6: one rare word of the query outweighs one common word, even in a newer record
	rare := write(ada, "Термін сертифіката", "Сертифікат Let's Encrypt оновлюється сам.")
	for i := 0; i < 3; i++ {
		write(ada, "Нотатки зустрічі", "Звичайні нотатки зустрічі.")
	}
	common := write(ada, "Ще нотатки", "Нотатки без суті.")
	r = search("нотатки сертифікат", false)
	if pos := positions(r.Items); pos[rare] == 0 || pos[common] == 0 || pos[rare] > pos[common] {
		t.Errorf("rare word first: rare at %d, common at %d", pos[rare], pos[common])
	}
	// x7: of rows matching equally, the title carrying the match wins over a newer mention in passing — for an
	// identifier and for words — but a title with one word never beats a fuller match in the body (x5)
	idTitle := write(ada, "Коміт 7c1e0ab3 у main", "Злиття гілки.")
	idBody := write(ada, "Нотатки релізу", "Між іншим, згадано 7c1e0ab3.")
	if r = search("7c1e0ab3", false); first(r) != idTitle || !ids(r.Items)[idBody] {
		t.Errorf("identifier tie: first titled %v, passing listed %v", first(r) == idTitle, ids(r.Items)[idBody])
	}
	wTitle := write(ada, "Ротація ключа резервування", "Ключ змінено.")
	wBody := write(ada, "Звіт тижня", "Ротація ключа резервування відбулась без простою.")
	oneWord := write(ada, "Ротація", "Без подробиць.")
	r = search("ротація ключа резервування", false)
	if pos := positions(r.Items); first(r) != wTitle || pos[wBody] == 0 || pos[oneWord] < pos[wBody] {
		t.Errorf("words tie: first titled %v, body at %d, one-word title at %d", first(r) == wTitle, pos[wBody], pos[oneWord])
	}
	// x8: neither title names the code; the record naming it again and again ranks above a newer one in passing
	tfMany := write(ada, "Розбір логу установника", "У setuperr.log видно збій; setuperr.log повторює його; третій рядок setuperr.log.")
	tfOnce := write(ada, "Нотатки апгрейду", "Подивились і setuperr.log.")
	if r = search("setuperr.log", false); first(r) != tfMany || !ids(r.Items)[tfOnce] {
		t.Errorf("identifier frequency: first frequent %v, passing listed %v", first(r) == tfMany, ids(r.Items)[tfOnce])
	}
	// x9: a word no record holds is a typo and meets its nearest stem; with it, the typed query finds the record
	if r = search("мграції", false); first(r) != mig {
		t.Errorf("typo «мграції» must find the migration record first, got %v", first(r))
	}
	// x11: a Latin name typed in Cyrillic meets it through the official transliteration
	bao := write(ada, "Вхід в OpenBao UI", "Вхід через userpass і TOTP.")
	if r = search("опенбао", false); first(r) != bao {
		t.Errorf("«опенбао» must find the OpenBao record first, got %v", first(r))
	}
	// a word typed in the wrong keyboard layout finds the record in either direction
	if r = search("vsuhfwsz", false); first(r) != mig {
		t.Errorf("«vsuhfwsz» (міграція in the English layout) must find the migration record first, got %v", first(r))
	}
	if r = search("щзутифщ", false); first(r) != bao {
		t.Errorf("«щзутифщ» (openbao in the Ukrainian layout) must find the OpenBao record first, got %v", first(r))
	}
	// a name of letters and a number meets its other spellings; a record holding only one of its parts does not
	joined := write(ada, "Апгрейд Win11 відкотився", "Setup не відкрив BCD.")
	apart := write(ada, "Ноутбук сина", "Стоїть Win 11, LM Studio.")
	winOnly := write(ada, "Старий win", "Без версії.")
	scattered := write(ada, "Win Server на стенді", "Розгорнуто 11 серверів.")
	for _, qs := range []string{"win11", "win 11", "win-11"} {
		r = search(qs, false)
		got, pos := ids(r.Items), positions(r.Items)
		if !got[joined] || !got[apart] || got[winOnly] && pos[winOnly] < pos[apart] || got[scattered] && pos[scattered] < pos[apart] {
			t.Errorf("%s: joined %v, apart %v, win alone at %d, scattered at %d (apart at %d)", qs, got[joined], got[apart],
				pos[winOnly], pos[scattered], pos[apart])
			continue
		}
		if qs == "win11" && got[scattered] {
			t.Errorf("win11: «win» and «11» apart in the text are not the name")
		}
		for _, id := range []uuid.UUID{joined, apart} {
			if m := match(r, id); m == nil || m.Coverage != "full" {
				t.Errorf("%s: the other spelling is a full match: %+v", qs, m)
			}
		}
	}
	// K1: the frame of the question is not its topic — a record that only says «вирішили» is not pulled in
	decided := write(ada, "Нотатки вечора", "Тоді вирішили відкласти все на завтра.")
	if r = search("що вирішили щодо міграції пулу", false); first(r) != mig || ids(r.Items)[decided] {
		t.Errorf("framed query: first mig %v, the «вирішили» record listed %v", first(r) == mig, ids(r.Items)[decided])
	}
	// a code typed with Cyrillic look-alikes (Н, Р) finds the Latin one
	if first(search("НР_TOOLS", false)) != code {
		t.Error("НР_TOOLS (Cyrillic Н, Р) must find HP_TOOLS")
	}
	// function words alone search nothing; the list without a query still works
	if r = search("на і the", false); len(r.Items) == 0 {
		t.Error("a query of function words only falls back to the recent list, not an error")
	}
}

// positions: 1-based place of each result, 0 when absent.
func positions(rows []query.IndexRow) map[uuid.UUID]int {
	m := map[uuid.UUID]int{}
	for i, r := range rows {
		m[r.StatementID] = i + 1
	}
	return m
}
