#!/usr/bin/env node
// Validate the Litopys domain spec: meta-schema, naming conventions, the "razor"
// (every element must serve a read scenario), event/verb consistency, and that
// every slice-1 input/output/payload schema actually compiles with its cross-file refs.
import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";
import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const load = (name) => parse(readFileSync(join(root, name), "utf8"));

const files = {
  types: load("types.yaml"),
  envelope: load("envelope.yaml"),
  events: load("events.yaml"),
  verbs: load("verbs.yaml"),
  readModels: load("read-models.yaml"),
  linkTypes: load("link-types.yaml"),
  vocabularies: load("vocabularies.yaml"),
};
const meta = JSON.parse(readFileSync(join(root, "schema", "spec.schema.json"), "utf8"));

const errors = [];
const fail = (msg) => errors.push(msg);

// ---------- 1. meta-schema ----------
const metaAjv = new Ajv2020({ allErrors: true, strict: false });
addFormats(metaAjv);
metaAjv.addSchema(meta);
const check = (section, data, label) => {
  const validate = metaAjv.compile({ $ref: `litopys/spec-meta#/properties/${section}` });
  if (!validate(data)) for (const e of validate.errors) fail(`${label}: ${e.instancePath || "/"} ${e.message}`);
};
check("events_file", files.events, "events.yaml");
check("verbs_file", files.verbs, "verbs.yaml");
check("read_models_file", files.readModels, "read-models.yaml");
check("envelope_file", files.envelope, "envelope.yaml");
check("link_types_file", files.linkTypes, "link-types.yaml");

// ---------- 2. naming conventions ----------
const COMMAND_VERBS = new Set(["assert", "amend", "retract", "supersede", "reinstate", "fold", "unfold", "declare", "transition", "record", "retire", "register", "revoke", "open", "checkpoint", "close", "attest", "challenge", "publish", "withdraw", "purge", "quarantine", "release", "propose", "approve", "reject", "produce", "reopen", "attach", "resolve", "merge", "start", "finish", "grant", "set", "resume", "delegate", "retain"]);
const QUERY_VERBS = new Set(["read", "search", "list", "as_of", "timeline", "changes", "trace", "explain", "pack", "open_items", "capabilities", "health", "whoami", "guide", "discover", "impact"]);
const FORBIDDEN_DOMAINS = new Set(["memory"]);
const MCP_NAME = /^[a-zA-Z0-9_-]{1,64}$/;

const verbs = files.verbs.verbs;
const events = files.events.events;
const readModels = files.readModels.read_models;

for (const name of Object.keys(verbs)) {
  const [domain, verb] = name.split(".");
  if (FORBIDDEN_DOMAINS.has(domain)) fail(`verb ${name}: domain "${domain}" is reserved for the compatibility facade`);
  const v = verbs[name];
  if (v.kind === "command" && !COMMAND_VERBS.has(verb)) fail(`verb ${name}: "${verb}" is not in the command vocabulary`);
  if (v.kind === "query" && !QUERY_VERBS.has(verb)) fail(`verb ${name}: "${verb}" is not in the query vocabulary`);
  const mcp = `${domain}_${verb}`;
  if (!MCP_NAME.test(mcp)) fail(`verb ${name}: MCP projection "${mcp}" violates tool-name regex`);
}
for (const name of Object.keys(events)) {
  const [domain] = name.split(".");
  if (FORBIDDEN_DOMAINS.has(domain)) fail(`event ${name}: domain "${domain}" is reserved`);
}

// ---------- 3. the razor: every element serves a scenario (meta-schema enforces minItems=1; here we cross-check R-ids exist in the scenario catalog, when present) ----------
let scenarioDoc = "";
try { scenarioDoc = readFileSync(join(root, "..", "docs", "01-read-scenarios-and-evals.md"), "utf8"); } catch { /* optional */ }
const declared = new Set([...scenarioDoc.matchAll(/^### (R\d+)\./gm)].map((m) => m[1]));
const usedScenarios = new Set();
const collect = (kind, obj) => {
  for (const [name, el] of Object.entries(obj)) for (const s of el.scenarios ?? []) {
    usedScenarios.add(s);
    if (declared.size && !declared.has(s)) fail(`${kind} ${name}: scenario ${s} is not defined in the scenario catalog`);
  }
};
collect("event", events); collect("verb", verbs); collect("read-model", readModels); collect("link type", files.linkTypes.link_types);
for (const s of files.envelope.scenarios) usedScenarios.add(s);
for (const s of declared) if (!usedScenarios.has(s)) fail(`scenario ${s} from the scenario catalog is served by no element (dead scenario or missing verb)`);

// ---------- 4. events <-> verbs consistency ----------
const emitted = new Set();
for (const [name, v] of Object.entries(verbs)) {
  for (const e of v.emits ?? []) {
    if (!events[e]) fail(`verb ${name} emits unknown event ${e}`);
    emitted.add(e);
  }
  if (v.kind === "command" && v.slice === 1 && !v.status) {
    const [domain, verb] = name.split(".");
    // the session verbs keep the journal's context.* events; resume (any domain) attaches a session
    const owners = verb === "resume" ? ["context", "run"] : domain === "session" ? ["context"] : [domain];
    const own = (v.emits ?? []).some((e) => owners.some((d) => e.startsWith(`${d}.`)));
    if (!own) fail(`verb ${name}: slice-1 command emits no event of its own domain`);
  }
}
for (const [name, e] of Object.entries(events)) {
  if (e.origin.includes("command") && !emitted.has(name)) fail(`event ${name}: origin includes "command" but no verb emits it`);
  if (e.slice === 1 && e.status) fail(`event ${name}: slice-1 element cannot be "planned"`);
}
for (const [name, rm] of Object.entries(readModels)) {
  for (const e of rm.fed_by) if (!events[e]) fail(`read-model ${name}: fed_by unknown event ${e}`);
  if (rm.slice === 1 && rm.status) fail(`read-model ${name}: slice-1 element cannot be "planned"`);
}
// the native API: every exposed verb has a model-facing tool text
for (const [name, v] of Object.entries(verbs)) if (v.slice === 1 && !v.status && !v.tool) fail(`verb ${name}: slice-1 verb without tool{} (title, description for the model)`);

// ---------- 4b. link types: one table drives the enum, the ends and the rules in core ----------
const linkTypes = files.linkTypes.link_types;
const entityTypes = new Set(files.types.$defs.EntityType.enum);
const relations = new Set(files.types.$defs.LinkRelation.enum);
const active = Object.entries(linkTypes).filter(([, t]) => !t.status).map(([n]) => n);
for (const n of active) if (!relations.has(n)) fail(`link type ${n}: active but missing from types.yaml LinkRelation`);
for (const r of relations) if (!active.includes(r)) fail(`LinkRelation ${r}: no active entry in link-types.yaml`);
const inverses = new Map();
for (const [name, t] of Object.entries(linkTypes)) {
  if (t.symmetric && t.inverse !== name) fail(`link type ${name}: symmetric, so inverse must be its own name`);
  if (!t.symmetric && t.inverse === name) fail(`link type ${name}: inverse equals the name but the type is not symmetric`);
  if (!t.symmetric && linkTypes[t.inverse]) fail(`link type ${name}: inverse ${t.inverse} collides with a link type`);
  if (inverses.has(t.inverse) && !t.symmetric) fail(`link type ${name}: inverse ${t.inverse} already used by ${inverses.get(t.inverse)}`);
  inverses.set(t.inverse, name);
  if (t.symmetric && t.acyclic) fail(`link type ${name}: symmetric and acyclic contradict each other`);
  const hasTask = t.ends.some((e) => e.from === "task" || e.to === "task" || e.from === "*");
  if (t.projects && !hasTask) fail(`link type ${name}: projects applies to task ends only`);
  if (t.on_end_closed === "satisfied" && !t.ends.some((e) => e.to === "task")) fail(`link type ${name}: satisfied needs a task at the "to" end`);
  if (!t.status) for (const e of t.ends) for (const end of [e.from, e.to]) {
    if (end !== "*" && !entityTypes.has(end)) fail(`link type ${name}: end ${end} is not an EntityType`);
  }
}

// ---------- 4c. usage vocabularies: the built-in catalog of record types ----------
const vocabs = files.vocabularies.vocabularies ?? {};
const families = files.types.$defs.Family?.enum ?? [];
const VOCAB_KEY = /^[a-z][a-z0-9-]{1,30}$/;
const TYPE_FIELDS = new Set(["family", "title", "description", "template", "tags", "data_class_hint", "reserved", "retired", "replaced_by", "fields"]);
const general = vocabs.general?.types ?? {};
if (!vocabs.general) fail("vocabularies: no general vocabulary");
for (const f of families) if (general[f]?.family !== f) fail(`vocabularies: general must have type ${f} of family ${f}`);
for (const k of Object.keys(general)) if (!families.includes(k)) fail(`vocabularies: general type ${k} is not a family`);
const typeOf = (full) => { const [v, t] = full.split("."); return t ? vocabs[v]?.types?.[t] : undefined; };
for (const [vk, v] of Object.entries(vocabs)) {
  if (!VOCAB_KEY.test(vk)) fail(`vocabulary ${vk}: key must match ${VOCAB_KEY}`);
  if (!Number.isInteger(v.version) || v.version < 1) fail(`vocabulary ${vk}: version must be an integer >= 1`);
  if (!v.title?.uk) fail(`vocabulary ${vk}: title.uk required`);
  if (!v.types || !Object.keys(v.types).length) fail(`vocabulary ${vk}: no types`);
  for (const [tk, t] of Object.entries(v.types ?? {})) {
    const n = `${vk}.${tk}`;
    if (!VOCAB_KEY.test(tk)) fail(`vocabulary type ${n}: key must match ${VOCAB_KEY}`);
    if (vk !== "general" && general[tk]) fail(`vocabulary type ${n}: the key of a general type is reserved (a stored "${tk}" means general)`);
    for (const k of Object.keys(t)) if (!TYPE_FIELDS.has(k)) fail(`vocabulary type ${n}: unknown field ${k}`);
    if (!families.includes(t.family)) fail(`vocabulary type ${n}: family ${t.family} is not a Family`);
    if (!t.title?.uk) fail(`vocabulary type ${n}: title.uk required`);
    if (typeof t.description !== "string" || !t.description) fail(`vocabulary type ${n}: description (English, for the model) required`);
    if (t.reserved && vk !== "general") fail(`vocabulary type ${n}: only general types are reserved`);
    if (t.family === "outcome" && n !== "general.outcome") fail(`vocabulary type ${n}: family outcome belongs to general.outcome only`);
    if (t.replaced_by && !t.retired) fail(`vocabulary type ${n}: replaced_by without retired`);
    if (t.replaced_by) { const r = typeOf(t.replaced_by) ?? general[t.replaced_by]; if (!r || r.retired) fail(`vocabulary type ${n}: replaced_by ${t.replaced_by} is not a live type`); }
    if (t.data_class_hint && !["public", "internal", "local"].includes(t.data_class_hint)) fail(`vocabulary type ${n}: data_class_hint ${t.data_class_hint}`);
  }
  for (const inc of v.includes ?? []) {
    const [iv] = inc.split(".");
    const t = typeOf(inc);
    if (!t || iv === "general" || iv === vk || t.retired) fail(`vocabulary ${vk}: includes ${inc} — not a live type of another (non-general) vocabulary`);
    if (t && v.types?.[inc.split(".")[1]]) fail(`vocabulary ${vk}: includes ${inc} and has its own type of the same short name`);
  }
}
for (const [old, to] of Object.entries(files.vocabularies.legacy ?? {})) {
  const t = to.includes(".") ? typeOf(to) : general[to];
  if (!t || t.retired) fail(`vocabularies legacy ${old}: ${to} is not a live type`);
  if (general[old]) fail(`vocabularies legacy ${old}: already a general type, nothing to map`);
}
// against the committed catalog: a type is never removed (retired instead), its family never changes, a changed
// vocabulary raises its version — old records keep their meaning
let committed = null;
try { committed = JSON.parse(readFileSync(join(root, "..", "core", "vocab", "gen", "vocabularies.json"), "utf8")); } catch { /* first generation */ }
for (const [vk, cv] of Object.entries(committed?.vocabularies ?? {})) {
  const v = vocabs[vk];
  if (!v) { fail(`vocabulary ${vk}: removed (built-in vocabularies are never removed)`); continue; }
  for (const [tk, ct] of Object.entries(cv.types)) {
    const t = v.types?.[tk];
    if (!t) fail(`vocabulary type ${vk}.${tk}: removed — mark it retired instead`);
    else if (t.family !== ct.family) fail(`vocabulary type ${vk}.${tk}: family ${ct.family} -> ${t.family} (a family never changes)`);
  }
  const same = JSON.stringify(Object.keys(cv.types).sort()) === JSON.stringify(Object.keys(v.types ?? {}).sort()) &&
    Object.entries(v.types ?? {}).every(([tk, t]) => !!t.retired === !!cv.types[tk]?.retired) &&
    JSON.stringify(cv.includes ?? []) === JSON.stringify(v.includes ?? []);
  if (!same && v.version <= cv.version) fail(`vocabulary ${vk}: types changed, version must rise above ${cv.version}`);
  if (v.version < cv.version) fail(`vocabulary ${vk}: version went down (${cv.version} -> ${v.version})`);
}

// ---------- 5. slice-1 schemas compile with cross-file refs ----------
const ajv = new Ajv2020({ strict: false, allErrors: true, validateSchema: false });
addFormats(ajv);
ajv.addSchema(files.types, "types.yaml");
ajv.addSchema(files.envelope, "envelope.yaml");
ajv.addSchema(files.events, "events.yaml");
ajv.addSchema(files.verbs, "verbs.yaml");
ajv.addSchema(files.readModels, "read-models.yaml");
const compileRef = (ref, label) => {
  try { ajv.compile({ $ref: ref }); } catch (err) { fail(`${label}: schema does not compile — ${err.message}`); }
};
compileRef("envelope.yaml#/schema", "envelope");
for (const [name, e] of Object.entries(events)) if (e.payload) compileRef(`events.yaml#/events/${name}/payload`, `event ${name} payload`);
for (const [name, v] of Object.entries(verbs)) {
  if (v.input) compileRef(`verbs.yaml#/verbs/${name}/input`, `verb ${name} input`);
  if (v.output) compileRef(`verbs.yaml#/verbs/${name}/output`, `verb ${name} output`);
}
for (const [name, rm] of Object.entries(readModels)) {
  for (const key of Object.keys(rm)) if (!["slice", "status", "scenarios", "description", "fed_by"].includes(key)) compileRef(`read-models.yaml#/read_models/${name}/${key}`, `read-model ${name}.${key}`);
}

// ---------- 6. examples: real processes of the golden set must fit the schemas ----------
let examples = [];
try { examples = readdirSync(join(root, "examples")).filter((f) => f.endsWith(".yaml")); } catch { /* none */ }
for (const f of examples) {
  const doc = load(join("examples", f));
  try {
    const validate = ajv.compile({ $ref: doc.schema });
    if (!validate(doc.example)) for (const e of validate.errors) fail(`example ${f}: ${e.instancePath || "/"} ${e.message}`);
  } catch (err) { fail(`example ${f}: ${err.message}`); }
}

// ---------- report ----------
const count = (obj, pred) => Object.values(obj).filter(pred).length;
console.log(`events: ${Object.keys(events).length} (slice 1: ${count(events, (e) => e.slice === 1)})`);
console.log(`verbs: ${Object.keys(verbs).length} (slice 1: ${count(verbs, (v) => v.slice === 1)}, commands: ${count(verbs, (v) => v.kind === "command")}, queries: ${count(verbs, (v) => v.kind === "query")})`);
console.log(`read-models: ${Object.keys(readModels).length}`);
console.log(`link types: ${Object.keys(linkTypes).length} (active: ${active.length}); examples: ${examples.length}`);
console.log(`vocabularies: ${Object.keys(vocabs).length}, types: ${Object.values(vocabs).reduce((n, v) => n + Object.keys(v.types ?? {}).length, 0)}`);
console.log(`scenarios served: ${[...usedScenarios].sort((a, b) => +a.slice(1) - +b.slice(1)).join(" ")}`);
if (errors.length) {
  console.error(`\n${errors.length} problem(s):`);
  for (const e of errors) console.error(" - " + e);
  process.exit(1);
}
console.log("\nOK — spec is consistent");
