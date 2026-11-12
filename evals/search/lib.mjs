// Search-quality gate: metrics, paired bootstrap, grade store, calibration, verdict.
// Pure functions over plain data — no database, no network — so they are unit-tested (lib.test.mjs).
//
// Methods (public sources only):
//   nDCG@k — Järvelin & Kekäläinen, "Cumulated gain-based evaluation of IR techniques", TOIS 2002;
//            gain 2^g − 1, discount log2(rank + 1), ideal = the known graded documents of the query.
//   junk@k — share of the top k graded 0 (nDCG is blind to junk below a relevant hit).
//   paired bootstrap of the mean difference over queries — Smucker, Allan & Carterette, "A comparison of
//            statistical significance tests for IR evaluation", CIKM 2007 (reported, descriptive).
//   paired randomization (sign-flip) test — the same paper's recommendation; exact for n ≤ 20. The verdict rests
//            on it: "proven worse" and "proven not worse than δ" (non-inferiority), owner decision 2026-10-19.
//   LLM relevance judge calibrated against a human — Thomas et al., "Large language models can accurately
//            predict searcher preferences", SIGIR 2024.

import { createHash } from "node:crypto";

export const GRADES = [0, 1, 2, 3]; // 0 junk · 1 related · 2 relevant · 3 the answer

// ---------- metrics ----------

const gain = (g) => 2 ** g - 1;
const disc = (i) => Math.log2(i + 2); // rank i is 0-based

export function dcg(grades, k) {
  return grades.slice(0, k).reduce((s, g, i) => s + gain(g ?? 0) / disc(i), 0);
}

// nDCG@k of a ranked list of grades against the ideal ordering of all known grades of the query.
// null when the query has no relevant document at all (undefined ideal).
export function ndcg(ranked, known, k) {
  const ideal = [...known].sort((a, b) => b - a);
  const idcg = dcg(ideal, k);
  return idcg === 0 ? null : dcg(ranked, k) / idcg;
}

// junk@k: graded-0 share of the first k returned results (an empty list is no junk).
export function junk(ranked, k) {
  const top = ranked.slice(0, k);
  return top.length === 0 ? 0 : top.filter((g) => g === 0).length / top.length;
}

export function recall(ranked, known, k) {
  const rel = known.filter((g) => g >= 2).length;
  return rel === 0 ? null : ranked.slice(0, k).filter((g) => g >= 2).length / rel;
}

// ---------- bootstrap ----------

// mulberry32: a seeded PRNG so a report is reproducible from its inputs.
export function rng(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// Paired bootstrap of mean(cand − base): resample queries with replacement. Returns the observed mean
// and the percentile interval at the given confidence.
export function bootstrapDelta(pairs, { reps = 10000, conf = 0.95, seed = 1 } = {}) {
  const d = pairs.map(([b, c]) => c - b);
  const n = d.length;
  if (n === 0) return { n, mean: null, lo: null, hi: null };
  const mean = d.reduce((s, x) => s + x, 0) / n;
  const r = rng(seed);
  const means = new Float64Array(reps);
  for (let i = 0; i < reps; i++) {
    let s = 0;
    for (let j = 0; j < n; j++) s += d[Math.floor(r() * n)];
    means[i] = s / n;
  }
  means.sort();
  const lo = means[Math.floor(((1 - conf) / 2) * reps)];
  const hi = means[Math.min(reps - 1, Math.ceil((1 - (1 - conf) / 2) * reps) - 1)];
  return { n, mean, lo, hi };
}

// ---------- randomization test ----------

// The rules of the verdict, set by the owner on 2026-10-19 after the review of three specialists:
// a class decides only with MIN_N holdout queries or more; it is fine when proven not worse than −DELTA in nDCG@10,
// RED when proven worse, UNDERPOWERED otherwise. ALPHA is one-sided.
export const MIN_N = 6, DELTA = 0.05, ALPHA = 0.025;

// signFlip: the one-sided p-value of mean(x) > 0 under the paired randomization test — every sign of every x_i
// equally likely under H0. Exact enumeration up to 20 values, a seeded Monte Carlo above.
export function signFlip(x, { reps = 100000, seed = 1 } = {}) {
  const n = x.length;
  if (n === 0) return null;
  const obs = x.reduce((s, v) => s + v, 0);
  const eps = 1e-12 * (1 + Math.abs(obs));
  let hits = 0, total = 0;
  if (n <= 20) {
    for (let m = 0; m < 1 << n; m++) {
      let s = 0;
      for (let i = 0; i < n; i++) s += m & (1 << i) ? -x[i] : x[i];
      if (s >= obs - eps) hits++;
    }
    total = 1 << n;
  } else {
    const r = rng(seed);
    for (let k = 0; k < reps; k++) {
      let s = 0;
      for (let i = 0; i < n; i++) s += r() < 0.5 ? -x[i] : x[i];
      if (s >= obs - eps) hits++;
    }
    total = reps;
  }
  return hits / total;
}

// ---------- sections ----------

// fullRows: the rows a result offers as holding the whole query. Search v2 marks every row (coverage); the old
// engine has no per-row mark, so all its rows count as full unless the list is relaxed (its OR phase).
export function fullRows(r, k = 10) {
  const items = (r?.items ?? []).slice(0, k);
  if (items.some((it) => it.coverage)) return items.filter((it) => it.coverage === "full");
  return r?.relaxed ? [] : items;
}

// claimsFull: does the result tell its reader that some row holds the whole query?
export function claimsFull(r) {
  if (r?.coverage) return r.coverage === "full";
  return !r?.relaxed && (r?.items ?? []).length > 0;
}

// ---------- splits ----------

// train / holdout by a stable hash of the query id (≈ 30 % holdout); a query never moves.
export function split(id, holdoutShare = 0.3) {
  const h = createHash("sha256").update(String(id)).digest();
  return h.readUInt32BE(0) / 2 ** 32 < holdoutShare ? "holdout" : "train";
}

// Stratified, sticky assignment for small sets: a query that has a split keeps it; the others of each class
// are taken in hash order into the holdout while the class's holdout share stays below holdoutShare.
export function assignSplits(queries, holdoutShare = 0.3) {
  const h = (id) => createHash("sha256").update(String(id)).digest().readUInt32BE(0);
  for (const cls of new Set(queries.map((q) => q.class))) {
    const qs = queries.filter((q) => q.class === cls);
    let hold = qs.filter((q) => q.split === "holdout").length;
    for (const q of qs.filter((q) => !q.split).sort((a, b) => h(a.id) - h(b.id))) {
      q.split = hold < Math.ceil(qs.length * holdoutShare) ? "holdout" : "train";
      if (q.split === "holdout") hold++;
    }
  }
  return queries;
}

// ---------- grades ----------

// A grade: { query_id, statement_id, grade 0..3, grader: "owner" | "judge:<model>@<snapshot>#<prompt hash>", at }.
// Tiers: the owner (3) > a delegate the owner named in search/delegates.json (2) > a calibrated judge (1);
// within a tier the newest grade wins. Anything else does not count.
export function gradeIndex(grades, calibratedJudges = new Set(), delegates = new Set()) {
  const tier = (g) => (g.grader === "owner" ? 3 : delegates.has(g.grader) ? 2 : calibratedJudges.has(g.grader) ? 1 : 0);
  const idx = new Map();
  for (const g of grades) {
    if (!GRADES.includes(g.grade)) throw new Error(`grade ${g.grade} of ${g.query_id}/${g.statement_id}: want 0..3`);
    const key = `${g.query_id}\u0000${g.statement_id}`;
    const t = tier(g);
    if (t === 0) continue;
    const prev = idx.get(key);
    if (!prev || t > tier(prev) || (t === tier(prev) && g.at > prev.at)) idx.set(key, g);
  }
  return {
    get: (q, s) => idx.get(`${q}\u0000${s}`)?.grade,
    known: (q) => [...idx.entries()].filter(([k]) => k.startsWith(`${q}\u0000`)).map(([, g]) => g.grade),
  };
}

// Calibration of a judge against the owner on the same (query, statement) pairs: agreement within ±1
// at least `min` (0.9) on at least `minPairs` pairs; exact agreement and Cohen-free counts for the record.
// The reference is human: the owner, else a named delegate (the owner's grade wins where both exist).
export function calibrate(grades, judge, { min = 0.9, minPairs = 100, delegates = new Set() } = {}) {
  const owner = new Map(grades.filter((g) => delegates.has(g.grader)).map((g) => [`${g.query_id}\u0000${g.statement_id}`, g.grade]));
  for (const g of grades) if (g.grader === "owner") owner.set(`${g.query_id}\u0000${g.statement_id}`, g.grade);
  let n = 0, within1 = 0, exact = 0;
  for (const g of grades) {
    if (g.grader !== judge) continue;
    const o = owner.get(`${g.query_id}\u0000${g.statement_id}`);
    if (o === undefined) continue;
    n++;
    if (Math.abs(o - g.grade) <= 1) within1++;
    if (o === g.grade) exact++;
  }
  const agreement = n ? within1 / n : 0;
  return { judge, pairs: n, within1: agreement, exact: n ? exact / n : 0, calibrated: n >= minPairs && agreement >= min, min, minPairs };
}

// Prompt hash pins a judge: model snapshot + this hash identify it (the grader string).
export const promptHash = (text) => createHash("sha256").update(text).digest("hex").slice(0, 12);

// ---------- verdict ----------

// Classes of memory queries. Texts are Ukrainian and English only: no Russian class (owner 2026-10-19).
export const CLASSES = ["identifier", "natural_uk", "natural_en", "typo", "decision"];

// collectionErrors: a query whose search failed (error) or is absent on either side. Such a run measures
// nothing: two empty lists "agree", so a gate over them would be GREEN for any change. The verdict refuses.
// excludeQuoting: drop the statements of the exclusion list (litopys-golden search/exclude.json, searcheval -quoting)
// from every result list, then cut to k. Such a statement quotes the gate itself — a memory record about the gate
// that cites a query word for word — and would push the real answers out of the top k (#276). The lists are
// collected k + |exclude| deep, so a cut list still has k rows when the corpus has them. Pure: new objects.
export function excludeQuoting(results, exclude = [], k = 10) {
  const legacy = new Set(exclude.map((e) => String(e.legacy_id)));
  const sid = new Set(exclude.map((e) => e.statement_id).filter(Boolean));
  const drop = (it) => sid.has(it.statement_id) || (it.legacy_id != null && legacy.has(String(it.legacy_id)));
  return results.map((r) => ({ ...r, items: (r.items ?? []).filter((it) => !drop(it)).slice(0, k) }));
}

export function collectionErrors(queries, base, cand) {
  const out = [];
  for (const [side, rs] of [["base", base], ["cand", cand]]) {
    const byId = new Map(rs.map((r) => [r.id, r]));
    for (const q of queries) {
      const r = byId.get(q.id);
      if (!r) out.push({ side, id: q.id, error: "no result" });
      else if (r.error) out.push({ side, id: q.id, error: r.error });
    }
  }
  return out;
}

// gate: base and candidate results ({id, items:[{statement_id, legacy_id}]}) on the same frozen base. The base of a
// merge is main (the owner's rule of 2026-10-19), not the old engine.
//   - incidents are a ratchet: one the base finds, or one marked kind "guard" (found by a merged version once), the
//     candidate must find — else RED;
//   - a class with MIN_N or more holdout queries: RED when its ΔnDCG@10 is proven negative (sign-flip, ALPHA), fine
//     when proven above −DELTA (non-inferiority), UNDERPOWERED otherwise; a smaller class is descriptive only;
//   - junk@10 is descriptive until the result carries its sections: it favours short lists by design;
//   - NOT_CALIBRATED when a holdout result in the top k of either side has no grade — unmeasured is not green.
// Order: RED, NOT_CALIBRATED, UNDERPOWERED, GREEN. N < 100 marks the report exploratory.
// splitName picks the measured queries: "holdout" (the dev set since 2026-10-19) or "sealed" (the sealed test set).
// Beside the verdict the report carries the section metrics (descriptive): hit@k — an answer (grade ≥ 2) in the
// top k of an answerable query; junk among the rows offered as full; false confidence — a query with no answer in
// memory whose result still claims a full match.
export function gate({ queries, incidents = [], base, cand, grades, calibratedJudges = new Set(), delegates = new Set(), k = 10, seed = 1, reps = 10000, splitName = "holdout" }) {
  const g = gradeIndex(grades, calibratedJudges, delegates);
  const byId = (rs) => new Map(rs.map((r) => [r.id, r]));
  const B = byId(base), C = byId(cand);
  const report = { k, verdict: "GREEN", reasons: [], incidents: [], classes: {}, ungraded: 0, exploratory: false };

  // incidents: zero tolerance — what the base finds, or a guard of the ratchet, the candidate must find too
  for (const inc of incidents) {
    const found = (r) => {
      const ids = (r?.items ?? []).slice(0, inc.k ?? k);
      const hit = (m) => ids.some((it) => it.statement_id === m || String(it.legacy_id) === String(m));
      return inc.must_find_any ? inc.must_find_any.some(hit) : inc.must_find.every(hit);
    };
    const b = found(B.get(inc.id)), c = found(C.get(inc.id));
    const state = c ? "found" : b || inc.kind === "guard" ? "LOST" : "open";
    report.incidents.push({ id: inc.id, base: b, cand: c, state });
    if (state === "LOST") report.reasons.push(`incident ${inc.id} lost`);
  }

  const missing = new Set();
  for (const cls of CLASSES) {
    const qs = queries.filter((q) => q.class === cls && (q.split ?? split(q.id)) === splitName);
    const nd = [], jk = [];
    let unmeasured = 0; // results of this class without a grade: metrics count them as 0, so a "worse" is not a measurement
    for (const q of qs) {
      const rb = (B.get(q.id)?.items ?? []).slice(0, k), rc = (C.get(q.id)?.items ?? []).slice(0, k);
      for (const it of [...rb, ...rc]) if (g.get(q.id, it.statement_id) === undefined) { missing.add(`${q.id}\u0000${it.statement_id}`); unmeasured++; }
      const gb = rb.map((it) => g.get(q.id, it.statement_id) ?? 0), gc = rc.map((it) => g.get(q.id, it.statement_id) ?? 0);
      const known = g.known(q.id);
      const nb = ndcg(gb, known, k), nc = ndcg(gc, known, k);
      if (nb !== null && nc !== null) nd.push([nb, nc]);
      jk.push([junk(gb, k), junk(gc, k)]);
    }
    const dN = bootstrapDelta(nd, { seed, reps }), dJ = bootstrapDelta(jk, { seed: seed + 1, reps });
    const d = nd.map(([b, c]) => c - b);
    const pWorse = signFlip(d.map((x) => -x), { seed }), pNotWorse = signFlip(d.map((x) => x + DELTA), { seed });
    let status = "descriptive"; // fewer than MIN_N queries: reported, never decides
    if (qs.length >= MIN_N) {
      if (pWorse !== null && pWorse < ALPHA) status = "worse";
      else if (pNotWorse !== null && pNotWorse < ALPHA) status = "not_worse";
      else status = "underpowered";
    }
    const worse = status === "worse" && unmeasured === 0; // RED only on graded evidence; an ungraded class is unmeasured
    report.classes[cls] = { queries: qs.length, ndcg10: dN, junk10: dJ, p_worse: pWorse, p_not_worse: pNotWorse, status, worse,
      ungraded: unmeasured, exploratory: qs.length < 100 };
    if (status === "worse" && unmeasured) report.classes[cls].looksWorseIfUngradedAreJunk = true;
    if (qs.length > 0 && qs.length < 100) report.exploratory = true;
    if (worse) report.reasons.push(`class ${cls} worse (ΔnDCG@${k} ${fmt(dN)}, p ${pWorse.toFixed(4)})`);
  }
  // section metrics over every measured query (descriptive; they replace junk@k once results carry sections)
  const measured = queries.filter((q) => (q.split ?? split(q.id)) === splitName);
  const side = (M) => {
    const hit = new Map(), jf = [], fc = [];
    for (const q of measured) {
      const r = M.get(q.id), gr = (it) => g.get(q.id, it.statement_id) ?? 0;
      const answerable = q.answerable ?? g.known(q.id).some((x) => x >= 2);
      if (answerable) hit.set(q.id, (r?.items ?? []).slice(0, k).some((it) => gr(it) >= 2) ? 1 : 0);
      else fc.push(claimsFull(r) ? 1 : 0);
      const full = fullRows(r, k);
      if (full.length) jf.push(full.filter((it) => gr(it) === 0).length / full.length);
    }
    const mean = (a) => (a.length ? a.reduce((s, x) => s + x, 0) / a.length : null);
    return { hit, summary: { hit_at_k: mean([...hit.values()]), answerable: hit.size, junk_in_full: mean(jf), lists_with_full: jf.length,
      false_confidence: mean(fc), no_answer: fc.length } };
  };
  const sb = side(B), sc = side(C);
  const dh = [...sb.hit.keys()].map((id) => sc.hit.get(id) - sb.hit.get(id));
  report.sections = { base: sb.summary, cand: sc.summary,
    hit_delta: { n: dh.length, mean: dh.length ? dh.reduce((s, x) => s + x, 0) / dh.length : null,
      p_worse: signFlip(dh.map((x) => -x), { seed }), p_not_worse: signFlip(dh.map((x) => x + DELTA), { seed }) } };
  report.ungraded = missing.size;
  const weak = Object.entries(report.classes).filter(([, c]) => c.status === "underpowered").map(([cls]) => cls);
  if (report.reasons.length) report.verdict = "RED";
  else if (missing.size > 0) {
    report.verdict = "NOT_CALIBRATED";
    report.reasons.push(`${missing.size} holdout results in the top ${k} have no owner or delegated grade and no calibrated judge`);
    for (const [cls, c] of Object.entries(report.classes)) {
      if (c.looksWorseIfUngradedAreJunk) report.reasons.push(`class ${cls} would be worse if its ${c.ungraded} ungraded results were junk (ΔnDCG@${k} ${fmt(c.ndcg10)})`);
    }
  } else if (weak.length) {
    report.verdict = "UNDERPOWERED";
    for (const cls of weak) report.reasons.push(`class ${cls}: not proven worse, nor proven not worse than −${DELTA} (ΔnDCG@${k} ${fmt(report.classes[cls].ndcg10)}, p ${report.classes[cls].p_not_worse.toFixed(4)})`);
  }
  return report;
}

const fmt = (d) => (d.mean === null ? "n/a" : `${d.mean.toFixed(3)} [${d.lo.toFixed(3)}, ${d.hi.toFixed(3)}]`);
