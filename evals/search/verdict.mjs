#!/usr/bin/env node
// Verdict of the search-quality gate: base vs candidate results on the same frozen base.
//
//   node evals/search/verdict.mjs --queries Q.json --base base.json --cand cand.json
//        [--incidents I.json] [--grades G.jsonl …] [--delegates D.json] [--experiment E.json] [--same-ranking]
//        [--exclude X.json --k N]   X — statements that quote the gate (searcheval -quoting), dropped from both sides
//
// Exit code: 0 GREEN, 1 RED, 2 NOT_CALIBRATED, 3 refused, 4 UNDERPOWERED (a changed ranking without a pre-registered
// experiment, or a query whose search failed on either side). The report (stdout) carries ids and numbers
// only — no query text, no memory content.

import { readFileSync } from "node:fs";
import { gate, calibrate, collectionErrors, excludeQuoting } from "./lib.mjs";

const args = process.argv.slice(2);
const all = (flag) => args.flatMap((a, i) => (a === flag ? [args[i + 1]] : []));
const one = (flag) => all(flag)[0];
const json = (f) => JSON.parse(readFileSync(f, "utf8"));
const jsonl = (f) => readFileSync(f, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));

const queries = json(one("--queries"));
const exclude = one("--exclude") ? json(one("--exclude")) : [];
// an excluded statement that an incident must find would make it unfindable on both sides: refuse, never measure
{
  const ex = new Set(exclude.flatMap((e) => [String(e.legacy_id), e.statement_id].filter(Boolean)));
  const clash = (one("--incidents") ? json(one("--incidents")) : []).flatMap((i) => (i.must_find_any ?? i.must_find ?? []).filter((m) => ex.has(String(m))).map((m) => `${i.id}:${m}`));
  if (clash.length) {
    console.error(`refused: exclude list holds incident targets (${clash.join(", ")})`);
    process.exit(3);
  }
}
const K = Number(one("--k") ?? 10);
// both sides lose the same statements before anything is measured (collected k + |exclude| deep by run.ps1)
const base = excludeQuoting(json(one("--base")), exclude, K), cand = excludeQuoting(json(one("--cand")), exclude, K);
const incidents = one("--incidents") ? json(one("--incidents")) : [];
const grades = all("--grades").flatMap(jsonl);
const same = args.includes("--same-ranking");

// a failed collection measures nothing (e.g. the frozen base behind the code's schema): refuse, never GREEN
const failed = collectionErrors(queries, base, cand);
if (failed.length) {
  const first = failed[0];
  console.error(`refused: ${failed.length} failed searches (first: ${first.side} ${first.id}: ${first.error})`);
  process.exit(3);
}
// delegates.json: [{ grader, granted_by: "owner", at, scope }] — grades the owner asked someone else to make
const delegation = one("--delegates") ? json(one("--delegates")) : [];
const delegates = new Set(delegation.filter((d) => d.granted_by === "owner").map((d) => d.grader));

// pre-registration: a changed ranking is judged only against a hypothesis written before
let experiment = null;
if (!same) {
  if (!one("--experiment")) {
    console.error("refused: a ranking change needs --experiment (id, hypothesis, prediction, registered_at) written before the run");
    process.exit(3);
  }
  experiment = json(one("--experiment"));
  for (const f of ["id", "hypothesis", "prediction", "registered_at"]) if (!experiment[f]) {
    console.error(`refused: experiment without ${f}`);
    process.exit(3);
  }
  if (new Date(experiment.registered_at) > new Date()) {
    console.error("refused: registered_at is in the future");
    process.exit(3);
  }
}

const judges = [...new Set(grades.map((g) => g.grader).filter((g) => g.startsWith("judge:")))];
const calibration = judges.map((j) => calibrate(grades, j, { delegates }));
const calibrated = new Set(calibration.filter((c) => c.calibrated).map((c) => c.judge));

const splitName = one("--split") ?? "holdout";
const report = gate({ queries, incidents, base, cand, grades, calibratedJudges: calibrated, delegates, splitName, k: K });
const out = {
  at: new Date().toISOString(),
  split: splitName,
  verdict: report.verdict,
  reasons: report.reasons,
  exploratory: report.exploratory,
  experiment,
  calibration,
  judge: calibrated.size ? [...calibrated] : "none (NOT_CALIBRATED until a judge passes calibration)",
  delegation,
  counts: { queries: queries.length, incidents: incidents.length, owner_grades: grades.filter((g) => g.grader === "owner").length,
    delegated_grades: grades.filter((g) => delegates.has(g.grader)).length },
  incidents: report.incidents,
  classes: report.classes,
  sections: report.sections,
  ungraded: report.ungraded,
};
console.log(JSON.stringify(out, null, 2));
process.exit({ GREEN: 0, RED: 1, NOT_CALIBRATED: 2, UNDERPOWERED: 4 }[report.verdict]);
