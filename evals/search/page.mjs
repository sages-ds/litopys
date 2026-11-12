#!/usr/bin/env node
// The owner's grading page of the search-quality gate: ~30 queries × the top 10 of the base
// and the candidate, each result graded 0..3; "Download" saves owner grades as JSONL for the private dataset
// (<dataset>/search/grades/). The page carries memory text, so it is written to bin/ and never committed.
//
//   node evals/search/page.mjs --queries Q.json --shown base.json [--shown cand.json] [--grades G.jsonl …]
//        [--limit 30] > bin/search-gate/grade.html

import { readFileSync } from "node:fs";
import { split, excludeQuoting } from "./lib.mjs";

const args = process.argv.slice(2);
const all = (flag) => args.flatMap((a, i) => (a === flag ? [args[i + 1]] : []));
const one = (flag, def) => all(flag)[0] ?? def;

const queries = JSON.parse(readFileSync(one("--queries"), "utf8"));
// --exclude/--k: the page shows what the gate measures — the exclusion list dropped, cut to k (#276)
const exclude = all("--exclude").flatMap((f) => JSON.parse(readFileSync(f, "utf8")));
const K = Number(all("--k")[0] ?? 10);
const shown = all("--shown").map((f) => new Map(excludeQuoting(JSON.parse(readFileSync(f, "utf8")), exclude, K).map((r) => [r.id, r])));
const graded = new Set(all("--grades").flatMap((f) => readFileSync(f, "utf8").split("\n").filter(Boolean))
  .map((l) => JSON.parse(l)).filter((g) => g.grader === "owner").map((g) => `${g.query_id}\u0000${g.statement_id}`));
const limit = Number(one("--limit", "30"));

// holdout first (the verdict reads it), then the rest; queries with something left to grade only
const sp = (q) => q.split ?? split(q.id);
const order = [...queries].sort((a, b) => (sp(a) === "holdout" ? 0 : 1) - (sp(b) === "holdout" ? 0 : 1));
const items = [];
for (const q of order) {
  const seen = new Map();
  for (const m of shown) for (const it of m.get(q.id)?.items ?? []) if (!seen.has(it.statement_id)) seen.set(it.statement_id, it);
  const todo = [...seen.values()].filter((it) => !graded.has(`${q.id}\u0000${it.statement_id}`));
  if (todo.length) items.push({ q, todo });
  if (items.length === limit) break;
}

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
const blocks = items.map(({ q, todo }, qi) => `
<section class="q"><h2><span class="n">${qi + 1}</span> ${esc(q.text)} <small>${esc(q.class)} · ${sp(q)}</small></h2>
${todo.map((it) => `<div class="r" data-q="${esc(q.id)}" data-s="${esc(it.statement_id)}">
  <div class="t">${esc(it.title)} <small>${esc(it.type)}${it.legacy_id ? " · #" + it.legacy_id : ""}</small></div>
  <div class="h">${esc(it.head)}</div>
  <div class="g">${[0, 1, 2, 3].map((g) => `<label><input type="radio" name="${esc(q.id)}|${esc(it.statement_id)}" value="${g}">${g}</label>`).join("")}</div>
</div>`).join("")}
</section>`).join("");

process.stdout.write(`<!doctype html><html lang="uk"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Оцінки пошуку</title><style>
:root{--bg:#fbfaf7;--fg:#1d1d1b;--mut:#6b6a66;--line:#e3e0d8;--acc:#2f5d8a}
@media (prefers-color-scheme:dark){:root{--bg:#161615;--fg:#ecebe6;--mut:#9b9a94;--line:#2c2b28;--acc:#8fb4dc}}
body{background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif;margin:0 auto;max-width:900px;padding:16px}
h1{font-size:20px}h2{font-size:16px;margin:28px 0 8px}small{color:var(--mut);font-weight:normal}
.n{display:inline-block;min-width:1.6em;color:var(--acc)}.r{border-top:1px solid var(--line);padding:8px 0}
.t{font-weight:600}.h{color:var(--mut);white-space:pre-wrap;max-height:7.5em;overflow:hidden}
.g label{margin-right:14px}button{font:inherit;padding:8px 14px;margin:16px 0}
.legend{color:var(--mut)}</style></head><body>
<h1>Оцінки пошуку Litopys</h1>
<p class="legend">3 — саме те, що шукали · 2 — релевантне, допомагає · 1 — дотичне, не допомагає · 0 — не те / сміття.
Оцінки зберігаються в цьому браузері; «Завантажити» дає файл для приватного набору даних (search/grades/).</p>
<p id="progress"></p>${blocks}
<button id="dl">Завантажити оцінки (JSONL)</button>
<script>
const KEY="litopys-search-grades";let saved={};try{saved=JSON.parse(localStorage.getItem(KEY)||"{}")}catch{}
const radios=[...document.querySelectorAll("input[type=radio]")];
for(const r of radios){if(saved[r.name]===r.value)r.checked=true;r.addEventListener("change",()=>{saved[r.name]=r.value;try{localStorage.setItem(KEY,JSON.stringify(saved))}catch{}progress()})}
function progress(){const total=new Set(radios.map(r=>r.name)).size;const done=radios.filter(r=>r.checked).length;document.getElementById("progress").textContent=done+" з "+total+" оцінено"}
progress();
document.getElementById("dl").onclick=()=>{const at=new Date().toISOString();const lines=radios.filter(r=>r.checked).map(r=>{const [q,s]=r.name.split("|");return JSON.stringify({query_id:q,statement_id:s,grade:Number(r.value),grader:"owner",at})});
const a=document.createElement("a");a.href=URL.createObjectURL(new Blob([lines.join("\\n")+"\\n"],{type:"application/x-ndjson"}));a.download="owner-"+at.slice(0,10)+".jsonl";a.click()};
</script></body></html>
`);
