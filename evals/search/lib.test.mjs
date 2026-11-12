// node --test evals/search/
import { test } from "node:test";
import assert from "node:assert/strict";
import { dcg, ndcg, junk, recall, bootstrapDelta, signFlip, MIN_N, fullRows, claimsFull, split, gradeIndex, calibrate, gate, collectionErrors, excludeQuoting } from "./lib.mjs";

const close = (a, b, eps = 1e-9) => assert.ok(Math.abs(a - b) < eps, `${a} ≈ ${b}`);

test("dcg and ndcg on a worked example", () => {
  // grades 3,2,0 → (7/1) + (3/log2 3) + 0
  close(dcg([3, 2, 0], 10), 7 + 3 / Math.log2(3));
  close(ndcg([3, 2, 0], [3, 2], 10), 1);
  close(ndcg([0, 3, 2], [3, 2], 10), (7 / Math.log2(3) + 3 / 2) / (7 + 3 / Math.log2(3)));
  assert.equal(ndcg([0, 0], [0, 0], 10), null); // nothing relevant known: undefined
  close(ndcg([2], [3, 2], 10), 3 / (7 + 3 / Math.log2(3))); // a better document exists but was not returned
});

test("junk and recall", () => {
  close(junk([3, 0, 0, 1], 4), 0.5);
  assert.equal(junk([], 10), 0);
  close(recall([3, 0, 2], [3, 2, 2], 10), 2 / 3);
  assert.equal(recall([1], [1, 0], 10), null);
});

test("bootstrap is reproducible and centred", () => {
  const pairs = Array.from({ length: 40 }, (_, i) => [0.5, 0.5 + (i % 5) * 0.01]);
  const a = bootstrapDelta(pairs, { seed: 7, reps: 2000 }), b = bootstrapDelta(pairs, { seed: 7, reps: 2000 });
  assert.deepEqual(a, b);
  close(a.mean, 0.02);
  assert.ok(a.lo <= a.mean && a.mean <= a.hi && a.lo > 0, JSON.stringify(a));
  assert.equal(bootstrapDelta([]).mean, null);
});

test("split is stable", () => {
  assert.equal(split("q-17"), split("q-17"));
  const holdout = Array.from({ length: 1000 }, (_, i) => split(`q-${i}`)).filter((s) => s === "holdout").length;
  assert.ok(holdout > 250 && holdout < 350, `${holdout} of 1000 in holdout`);
});

test("owner grades win; judges count only when calibrated", () => {
  const grades = [
    { query_id: "q", statement_id: "s1", grade: 3, grader: "judge:m@1#abc", at: "2026-10-08" },
    { query_id: "q", statement_id: "s1", grade: 1, grader: "owner", at: "2026-10-07" },
    { query_id: "q", statement_id: "s2", grade: 2, grader: "judge:m@1#abc", at: "2026-10-08" },
  ];
  assert.equal(gradeIndex(grades).get("q", "s1"), 1);
  assert.equal(gradeIndex(grades).get("q", "s2"), undefined);
  assert.equal(gradeIndex(grades, new Set(["judge:m@1#abc"])).get("q", "s2"), 2);
  assert.throws(() => gradeIndex([{ query_id: "q", statement_id: "x", grade: 5, grader: "owner" }]));
});

test("calibration: within ±1 on enough pairs", () => {
  const grades = [];
  for (let i = 0; i < 120; i++) {
    grades.push({ query_id: `q${i}`, statement_id: "s", grade: i % 4, grader: "owner", at: "a" });
    grades.push({ query_id: `q${i}`, statement_id: "s", grade: i % 10 === 0 ? 3 - (i % 4) : i % 4, grader: "judge:x", at: "a" });
  }
  const c = calibrate(grades, "judge:x");
  assert.equal(c.pairs, 120);
  assert.ok(c.within1 >= 0.9 && c.calibrated, JSON.stringify(c));
  assert.equal(calibrate(grades.slice(0, 20), "judge:x").calibrated, false); // too few pairs
});

// a tiny world: 3 holdout queries of one class, everything graded
function world(candOrder) {
  const queries = ["a", "b", "c", "d", "e", "f", "g", "h"].map((id) => ({ id, class: "identifier", split: "holdout" }));
  const grades = [];
  const base = [], cand = [];
  for (const q of queries) {
    grades.push({ query_id: q.id, statement_id: `${q.id}1`, grade: 3, grader: "owner", at: "x" });
    grades.push({ query_id: q.id, statement_id: `${q.id}2`, grade: 0, grader: "owner", at: "x" });
    base.push({ id: q.id, items: [{ statement_id: `${q.id}1` }, { statement_id: `${q.id}2` }] });
    cand.push({ id: q.id, items: candOrder(q.id).map((s) => ({ statement_id: s })) });
  }
  return { queries, grades, base, cand };
}

test("verdict GREEN when nothing changed and all is graded", () => {
  const w = world((q) => [`${q}1`, `${q}2`]);
  const r = gate({ ...w, reps: 500 });
  assert.equal(r.verdict, "GREEN", JSON.stringify(r.reasons));
  assert.equal(r.exploratory, true); // 8 < 100 queries
});

test("verdict RED when the answer drops below junk everywhere", () => {
  const w = world((q) => [`${q}2`, `${q}1`]);
  const r = gate({ ...w, reps: 500 });
  assert.equal(r.verdict, "RED");
  assert.ok(r.classes.identifier.worse);
});

test("verdict NOT_CALIBRATED when the candidate brings an ungraded result", () => {
  const w = world((q) => [`${q}1`, `${q}new`]);
  const r = gate({ ...w, reps: 500 });
  assert.equal(r.verdict, "NOT_CALIBRATED");
  assert.equal(r.ungraded, 8);
});

test("ungraded results that only look worse (counted as 0) are NOT_CALIBRATED, not RED", () => {
  // the candidate pushes the graded answer out for an ungraded newcomer: as 0 it reads as a drop
  const w = world((q) => [`${q}new`, `${q}2`]);
  const r = gate({ ...w, reps: 500 });
  assert.equal(r.verdict, "NOT_CALIBRATED", JSON.stringify(r.reasons));
  assert.equal(r.classes.identifier.worse, false);
  assert.ok(r.classes.identifier.looksWorseIfUngradedAreJunk);
});

test("incidents: lost is RED, open stays open", () => {
  const w = world((q) => [`${q}1`, `${q}2`]);
  w.cand.find((r) => r.id === "a").items = [{ statement_id: "a2" }];
  const r = gate({ ...w, reps: 500, incidents: [{ id: "a", must_find: ["a1"] }, { id: "b", must_find: ["zzz"] }] });
  assert.equal(r.verdict, "RED");
  assert.deepEqual(r.incidents.map((i) => i.state), ["LOST", "open"]);
});

test("sign-flip is exact for small n", () => {
  assert.equal(signFlip([1, 1, 1]), 1 / 8); // only the observed signs reach the observed sum
  assert.equal(signFlip([0, 0]), 1); // nothing differs: every flip ties
  assert.equal(signFlip([]), null);
  assert.ok(signFlip([1, -1, 1, -1]) > 0.5);
});

test("a ratchet guard must be found even when the base misses it", () => {
  const w = world((q) => [`${q}1`, `${q}2`]);
  w.base.find((r) => r.id === "a").items = [{ statement_id: "a2" }];
  w.cand.find((r) => r.id === "a").items = [{ statement_id: "a2" }];
  const r = gate({ ...w, reps: 500, incidents: [{ id: "a", must_find: ["a1"], kind: "guard" }, { id: "b", must_find: ["zzz"], kind: "miss" }] });
  assert.equal(r.verdict, "RED");
  assert.deepEqual(r.incidents.map((i) => i.state), ["LOST", "open"]);
});

test("a class below MIN_N is descriptive: it never decides", () => {
  const w = world((q) => [`${q}2`, `${q}1`]); // every query worse
  w.queries = w.queries.slice(0, MIN_N - 1);
  const r = gate({ ...w, reps: 500 });
  assert.equal(r.classes.identifier.status, "descriptive");
  assert.equal(r.verdict, "GREEN", JSON.stringify(r.reasons));
});

test("neither proven worse nor proven not worse is UNDERPOWERED, not GREEN", () => {
  // half the queries worse, half better: no evidence either way
  const w = world((q) => ("abcd".includes(q) ? [`${q}2`, `${q}1`] : [`${q}1`, `${q}2`]));
  w.base = w.base.map((r) => ("abcd".includes(r.id) ? r : { ...r, items: [{ statement_id: `${r.id}2` }, { statement_id: `${r.id}1` }] }));
  const r = gate({ ...w, reps: 500 });
  assert.equal(r.classes.identifier.status, "underpowered");
  assert.equal(r.verdict, "UNDERPOWERED", JSON.stringify(r.reasons));
});

test("section metrics: hit@k, junk among full rows, false confidence on a query with no answer", () => {
  const w = world((q) => [`${q}1`, `${q}2`]);
  // the candidate marks rows like search v2: the answer full, the junk partial
  w.cand = w.cand.map((r) => ({ ...r, coverage: "full", items: r.items.map((it) => ({ ...it, coverage: it.statement_id.endsWith("1") ? "full" : "partial" })) }));
  // one query has no answer in memory: its only graded result is junk, and the candidate still claims a full match
  w.queries.push({ id: "z", class: "identifier", split: "holdout", answerable: false });
  w.grades.push({ query_id: "z", statement_id: "z1", grade: 0, grader: "owner", at: "x" });
  w.base.push({ id: "z", relaxed: true, items: [{ statement_id: "z1" }] });
  w.cand.push({ id: "z", coverage: "full", items: [{ statement_id: "z1", coverage: "full" }] });
  const s = gate({ ...w, reps: 500 }).sections;
  assert.equal(s.base.hit_at_k, 1);
  assert.equal(s.cand.hit_at_k, 1);
  assert.equal(s.base.junk_in_full, 0.5); // the old engine offers both rows as full
  assert.equal(s.cand.junk_in_full, (8 * 0 + 1) / 9); // only the answers are full, and z1 (junk) — 1 of 9 lists
  assert.equal(s.base.false_confidence, 0); // relaxed: it did not claim a full match
  assert.equal(s.cand.false_confidence, 1);
  assert.equal(fullRows({ relaxed: false, items: [{ statement_id: "a" }] }).length, 1);
  assert.equal(claimsFull({ coverage: "partial", items: [{}] }), false);
});

test("a named delegate counts below the owner and above a judge", () => {
  const grades = [
    { query_id: "q", statement_id: "s1", grade: 2, grader: "delegate:x", at: "2" },
    { query_id: "q", statement_id: "s1", grade: 0, grader: "owner", at: "1" },
    { query_id: "q", statement_id: "s2", grade: 3, grader: "delegate:x", at: "2" },
    { query_id: "q", statement_id: "s2", grade: 1, grader: "judge:j", at: "3" },
  ];
  assert.equal(gradeIndex(grades, new Set(["judge:j"])).get("q", "s2"), 1); // delegate not named: judge
  const g = gradeIndex(grades, new Set(["judge:j"]), new Set(["delegate:x"]));
  assert.equal(g.get("q", "s1"), 0); // the owner wins
  assert.equal(g.get("q", "s2"), 3); // the delegate beats the judge
});

test("a failed or missing search is a collection error, so the verdict refuses instead of a GREEN over empty lists", () => {
  const queries = [{ id: "a" }, { id: "b" }];
  const ok = [{ id: "a", items: [] }, { id: "b", items: [] }];
  assert.deepEqual(collectionErrors(queries, ok, ok), []);
  const broken = [{ id: "a", items: [], error: "column missing" }];
  const errs = collectionErrors(queries, ok, broken);
  assert.equal(errs.length, 2);
  assert.deepEqual(errs.map((e) => [e.side, e.id]), [["cand", "a"], ["cand", "b"]]);
  assert.equal(errs[1].error, "no result");
});

test("excludeQuoting drops the listed statements before the cut to k", () => {
  const it = (n) => ({ statement_id: `s${n}`, legacy_id: n });
  const rs = [{ id: "o-6", items: [it(213), it(866), it(861), it(212), it(770), it(771)] }, { id: "m-1", items: [it(1)] }];
  const out = excludeQuoting(rs, [{ legacy_id: 866 }, { legacy_id: "861" }, { statement_id: "s1" }], 3);
  assert.deepEqual(out[0].items.map((x) => x.legacy_id), [213, 212, 770]);
  assert.deepEqual(out[1].items, []);
  assert.equal(rs[0].items.length, 6); // the input is not changed
  assert.deepEqual(excludeQuoting(rs, [], 2)[0].items.map((x) => x.legacy_id), [213, 866]);
});
