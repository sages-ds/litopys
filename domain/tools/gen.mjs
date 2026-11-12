#!/usr/bin/env node
// Generate the native API surface from the domain spec: core/api/gen/surface.json.
// One entry per exposed verb (slice 1, not planned): MCP name, HTTP path, kind, auth, profiles, the
// model-facing English text of tool{} and the input schema with every $ref inlined (MCP clients do not
// follow references across files). Human (Ukrainian) descriptions never reach the model: property
// descriptions are dropped unless tool.params gives an English one.
//
//   node domain/tools/gen.mjs           write the file
//   node domain/tools/gen.mjs --check   exit 1 when the committed file differs (CI drift check)
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const out = join(root, "..", "core", "api", "gen", "surface.json");
const API_VERSION = "1.0.0";

const docs = {
  "types.yaml": parse(readFileSync(join(root, "types.yaml"), "utf8")),
  "verbs.yaml": parse(readFileSync(join(root, "verbs.yaml"), "utf8")),
  "read-models.yaml": parse(readFileSync(join(root, "read-models.yaml"), "utf8")),
};

const pointer = (doc, path) =>
  path.split("/").filter(Boolean).reduce((node, raw) => {
    const key = raw.replace(/~1/g, "/").replace(/~0/g, "~");
    if (node == null || !(key in node)) throw new Error(`unresolvable pointer ${path} at ${key}`);
    return node[key];
  }, doc);

// inline every $ref; siblings of a $ref (e.g. default) are kept, descriptions are handled by strip()
function resolve(node, file, seen = []) {
  if (Array.isArray(node)) return node.map((n) => resolve(n, file, seen));
  if (node === null || typeof node !== "object") return node;
  if (typeof node.$ref === "string") {
    const [target, path = ""] = node.$ref.split("#");
    const tfile = target || file;
    const key = `${tfile}#${path}`;
    if (seen.includes(key)) throw new Error(`recursive $ref ${key}`);
    const base = resolve(pointer(docs[tfile], path), tfile, [...seen, key]);
    const { $ref, ...rest } = node;
    return { ...base, ...resolve(rest, file, seen) };
  }
  const outNode = {};
  for (const [k, v] of Object.entries(node)) outNode[k] = resolve(v, file, seen);
  return outNode;
}

// drop human descriptions (Ukrainian) everywhere; then put the English ones of tool.params
function strip(node) {
  if (Array.isArray(node)) return node.map(strip);
  if (node === null || typeof node !== "object") return node;
  const o = {};
  for (const [k, v] of Object.entries(node)) {
    if (k === "description" && typeof v === "string") continue;
    if (k === "text" && TEXT_CLASSES.includes(v)) continue; // the field class stays out of the schema clients see
    if (k === "properties") {
      o.properties = Object.fromEntries(Object.entries(v).map(([p, s]) => [p, strip(s)]));
      continue;
    }
    o[k] = strip(v);
  }
  return o;
}

// the text fields of a command by class (the owner's П1 б): every string or open object of a command's
// input carries text: line | free | json | id, and the generator refuses one without it. Ids are not listed in surface.json.
const TEXT_CLASSES = ["line", "free", "json", "id"];
function textFields(verb, schema) {
  const found = {};
  const need = (path, n) => {
    if (!TEXT_CLASSES.includes(n.text)) throw new Error(`verb ${verb}: the text field ${path} has no class (text: line | free | json | id)`);
    return n.text;
  };
  const walk = (n, path) => {
    if (n === null || typeof n !== "object") return;
    const types = [].concat(n.type ?? []);
    if (types.includes("string") && !n.enum && n.const === undefined && !n.format) {
      const c = need(path, n);
      if (c !== "id") found[path] = c;
    }
    if (types.includes("object") && !n.properties && !n.oneOf && !n.anyOf && (n.additionalProperties === undefined || n.additionalProperties === true)) {
      const c = need(path, n);
      if (c !== "json" && c !== "id") throw new Error(`verb ${verb}: the open object ${path} must be class json or id, not ${c}`);
      if (c === "json") found[path] = c;
    }
    for (const [p, c] of Object.entries(n.properties ?? {})) walk(c, path ? `${path}.${p}` : p);
    if (n.items) walk(n.items, `${path}[]`);
    if (n.additionalProperties && typeof n.additionalProperties === "object") walk(n.additionalProperties, `${path}{}`);
    for (const k of ["oneOf", "anyOf", "allOf"]) for (const a of n[k] ?? []) walk(a, path);
  };
  walk(schema, "");
  return found;
}

const tools = [];
for (const [name, v] of Object.entries(docs["verbs.yaml"].verbs)) {
  if (v.slice !== 1 || v.status) continue;
  const [domain, verb] = name.split(".");
  const t = v.tool;
  if (!t) throw new Error(`verb ${name}: no tool{} (run validate.mjs)`);
  const resolved = resolve(v.input ?? { type: "object" }, "verbs.yaml");
  const text = v.kind === "command" ? textFields(name, resolved) : {};
  const input = strip(resolved);
  for (const [p, text] of Object.entries(t.params ?? {})) {
    if (!input.properties?.[p]) throw new Error(`verb ${name}: tool.params.${p} names no input property`);
    input.properties[p].description = text;
  }
  // the executor's variant: the same verb with only tool.executor.params and a short description — what
  // an executor with a derived grant is listed; calling with the full schema still works
  let executor;
  if (t.executor) {
    const keep = t.executor.params ?? [];
    for (const p of keep) if (!input.properties?.[p]) throw new Error(`verb ${name}: tool.executor.params names no input property ${p}`);
    const desc = (t.executor.description ?? "").trim();
    if (desc.length < 20 || desc.length > 300) throw new Error(`verb ${name}: tool.executor.description must be 20..300 characters`);
    // a required property (also inside oneOf / anyOf / allOf) the variant hid could never be passed: refuse
    const requiredIn = (node) => [...(node.required ?? []), ...["oneOf", "anyOf", "allOf"].flatMap((k) => (node[k] ?? []).flatMap(requiredIn))];
    for (const r of requiredIn(input)) {
      if (!keep.includes(r)) throw new Error(`verb ${name}: tool.executor.params leaves out the required property ${r}`);
    }
    executor = {
      description: desc,
      inputSchema: { ...input, properties: Object.fromEntries(keep.map((p) => [p, input.properties[p]])) },
    };
  }
  const auth = v.auth ?? docs["verbs.yaml"].defaults?.auth ?? "owner";
  const profiles = auth === "owner" ? [...(t.chat ? ["chat"] : []), "core", "full"] : [];
  if (v.kind === "query" || verb === "resume") profiles.push("readonly");
  tools.push({
    name: `${domain}_${verb}`,
    verb: name,
    http: `/v1/${domain}/${verb}`,
    kind: v.kind,
    auth,
    profiles,
    title: t.title,
    description: t.description.trim(),
    annotations: {
      readOnlyHint: v.kind === "query",
      destructiveHint: !!t.destructive,
      idempotentHint: v.kind === "query" || !!t.idempotent,
      openWorldHint: false,
    },
    inputSchema: input,
    ...(Object.keys(text).length ? { text_fields: Object.fromEntries(Object.entries(text).sort()) } : {}),
    ...(executor ? { executor } : {}),
  });
}

// link types: active ones only, rules without the human text — core/linktypes embeds this table
const linkDoc = parse(readFileSync(join(root, "link-types.yaml"), "utf8"));
const linkTypes = {};
for (const [name, t] of Object.entries(linkDoc.link_types)) {
  if (t.status) continue;
  linkTypes[name] = {
    layer: t.layer, inverse: t.inverse, ends: t.ends, symmetric: !!t.symmetric, transitive: !!t.transitive,
    acyclic: !!t.acyclic, scope: t.scope, projects: t.projects ?? "any", on_end_closed: t.on_end_closed,
  };
}
const linkOut = join(root, "..", "core", "linktypes", "gen", "link_types.json");
const linkText = JSON.stringify({ spec_version: linkDoc.spec_version, link_types: linkTypes }, null, 2) + "\n";

// usage vocabularies: the built-in catalog with the families and the legacy map — core/vocab embeds it
const vocabDoc = parse(readFileSync(join(root, "vocabularies.yaml"), "utf8"));
const vocabularies = {};
for (const [vk, v] of Object.entries(vocabDoc.vocabularies)) {
  const types = {};
  for (const [tk, t] of Object.entries(v.types)) {
    types[tk] = {
      family: t.family, title: t.title, description: t.description,
      ...(t.template ? { template: t.template } : {}), ...(t.tags ? { tags: t.tags } : {}),
      ...(t.data_class_hint ? { data_class_hint: t.data_class_hint } : {}), ...(t.reserved ? { reserved: true } : {}),
      ...(t.retired ? { retired: true } : {}), ...(t.replaced_by ? { replaced_by: t.replaced_by } : {}),
      ...(t.fields ? { fields: t.fields } : {}),
    };
    for (const [fk, f] of Object.entries(t.fields ?? {})) {
      if (!["number", "string", "date", "currency", "email", "phone"].includes(f.type)) throw new Error(`vocabulary ${vk}.${tk}: field ${fk} type ${f.type}`);
    }
  }
  vocabularies[vk] = { version: v.version, title: v.title, purpose: v.purpose, ...(v.includes ? { includes: v.includes } : {}), types };
}
const vocabOut = join(root, "..", "core", "vocab", "gen", "vocabularies.json");
const vocabText = JSON.stringify({ spec_version: vocabDoc.spec_version, families: docs["types.yaml"].$defs.Family.enum,
  legacy: vocabDoc.legacy, vocabularies }, null, 2) + "\n";

// the any-client guide (owner decision 2026-10-30): one source for instructions, the rules block of
// project_resume / session.open and the topics of system.guide — core/api embeds core/api/gen/guide.json
const guideDir = join(root, "guide");
const guideDoc = parse(readFileSync(join(guideDir, "rules.yaml"), "utf8"));
const flat = (s) => s.replace(/\s+/g, " ").trim();
const guideCore = flat(guideDoc.core);
const topicNames = Object.keys(guideDoc.topics);
const topics = {};
for (const name of topicNames) {
  const text = readFileSync(join(guideDir, `${name}.md`), "utf8").replace(/\r\n/g, "\n").trim();
  if (text.length > 2000) throw new Error(`guide topic ${name}: ${text.length} characters, the limit is 2000`);
  const see = guideDoc.topics[name].see_also ?? [];
  for (const s of see) if (!topicNames.includes(s)) throw new Error(`guide topic ${name}: see_also ${s} is no topic`);
  topics[name] = { summary: guideDoc.topics[name].summary, text, see_also: see };
}
const guideIndex = "# Litopys guide\n\nCall system_guide with one of these topics:\n\n" +
  topicNames.map((n) => `- ${n} — ${topics[n].summary}`).join("\n");
const ruleLines = guideDoc.lines.map(flat);
const guideLink = `system_guide — topics: ${topicNames.join(", ")}`;
const instructions = `${guideCore}\n\n${flat(guideDoc.lifecycle)}\n\nRules:\n` +
  [...ruleLines, ...guideDoc.instructions_only.map(flat)].map((l) => `- ${l}`).join("\n") + `\n\nDetails: ${guideLink}.`;
if (guideCore.length > 512) throw new Error(`guide core: ${guideCore.length} characters, the limit is 512`);
const linesLen = ruleLines.join("").length;
if (linesLen > 1000) throw new Error(`guide rules lines: ${linesLen} characters, the limit is 1000`);
if (instructions.length > 2048) throw new Error(`instructions: ${instructions.length} characters, the limit is 2048`);
const guideBody = { core: guideCore, instructions, rules: { lines: ruleLines, guide: guideLink }, index: guideIndex,
  order: topicNames, topics };
const guideVersion = createHash("sha256").update(JSON.stringify(guideBody)).digest("hex").slice(0, 16);
const guideOut = join(root, "..", "core", "api", "gen", "guide.json");
const guideText = JSON.stringify({ version: guideVersion, ...guideBody }, null, 2) + "\n";

// vendor presets of executors with the help of each vendor from domain/executor-keys.md (heading
// "### … (`key`)") — core/executors embeds core/executors/gen/providers.json; the settings page shows that text
const provDoc = parse(readFileSync(join(root, "providers.yaml"), "utf8"));
const keysDoc = readFileSync(join(root, "executor-keys.md"), "utf8").replace(/\r\n/g, "\n");
const help = {};
for (const sec of keysDoc.split(/^### /m).slice(1)) {
  const nl = sec.indexOf("\n");
  for (const m of sec.slice(0, nl).matchAll(/\(`([a-z0-9-]+)`\)/g)) help[m[1]] = "### " + sec.trim();
}
const providers = {};
for (const [k, v] of Object.entries(provDoc.providers)) {
  if (!["openai", "anthropic"].includes(v.adapter)) throw new Error(`provider ${k}: adapter ${v.adapter}`);
  if (!["local", "free", "paid", "plan_credits"].includes(v.key_kind)) throw new Error(`provider ${k}: key_kind ${v.key_kind}`);
  if (!["public", "internal", "local"].includes(v.data_class)) throw new Error(`provider ${k}: data_class ${v.data_class}`);
  if (v.data_policy?.training && v.data_class !== "public") throw new Error(`provider ${k}: trains on data, the preset class must be public`);
  if (!help[k]) throw new Error(`provider ${k}: no section "### … (\`${k}\`)" in domain/executor-keys.md`);
  const terms = v.terms_checked_at instanceof Date ? v.terms_checked_at.toISOString().slice(0, 10) : v.terms_checked_at;
  providers[k] = { ...v, ...(terms ? { terms_checked_at: terms } : {}), fields: v.fields ?? [], help: help[k] };
}
const provOut = join(root, "..", "core", "executors", "gen", "providers.json");
const provText = JSON.stringify({ spec_version: provDoc.spec_version, order: Object.keys(providers), providers }, null, 2) + "\n";

const body = { api_version: API_VERSION, spec_version: docs["verbs.yaml"].spec_version, tools };
const hash = createHash("sha256").update(JSON.stringify(body)).digest("hex").slice(0, 16);
const text = JSON.stringify({ ...body, surface_hash: hash }, null, 2) + "\n";

if (process.argv.includes("--check")) {
  let have = "";
  try { have = readFileSync(out, "utf8").replace(/\r\n/g, "\n"); } catch { /* missing */ }
  if (have !== text) {
    console.error("core/api/gen/surface.json is stale: run node domain/tools/gen.mjs and commit it");
    process.exit(1);
  }
  let haveLinks = "";
  try { haveLinks = readFileSync(linkOut, "utf8").replace(/\r\n/g, "\n"); } catch { /* missing */ }
  if (haveLinks !== linkText) {
    console.error("core/linktypes/gen/link_types.json is stale: run node domain/tools/gen.mjs and commit it");
    process.exit(1);
  }
  let haveVocab = "";
  try { haveVocab = readFileSync(vocabOut, "utf8").replace(/\r\n/g, "\n"); } catch { /* missing */ }
  if (haveVocab !== vocabText) {
    console.error("core/vocab/gen/vocabularies.json is stale: run node domain/tools/gen.mjs and commit it");
    process.exit(1);
  }
  let haveProv = "";
  try { haveProv = readFileSync(provOut, "utf8").replace(/\r\n/g, "\n"); } catch { /* missing */ }
  if (haveProv !== provText) {
    console.error("core/executors/gen/providers.json is stale: run node domain/tools/gen.mjs and commit it");
    process.exit(1);
  }
  let haveGuide = "";
  try { haveGuide = readFileSync(guideOut, "utf8").replace(/\r\n/g, "\n"); } catch { /* missing */ }
  if (haveGuide !== guideText) {
    console.error("core/api/gen/guide.json is stale: run node domain/tools/gen.mjs and commit it");
    process.exit(1);
  }
  console.log(`surface up to date: ${tools.length} tools, hash ${hash}`);
} else {
  mkdirSync(dirname(out), { recursive: true });
  writeFileSync(out, text);
  console.log(`wrote ${out}: ${tools.length} tools, hash ${hash}`);
  mkdirSync(dirname(linkOut), { recursive: true });
  writeFileSync(linkOut, linkText);
  console.log(`wrote ${linkOut}: ${Object.keys(linkTypes).length} link types`);
  mkdirSync(dirname(vocabOut), { recursive: true });
  writeFileSync(vocabOut, vocabText);
  console.log(`wrote ${vocabOut}: ${Object.keys(vocabularies).length} vocabularies`);
  writeFileSync(guideOut, guideText);
  console.log(`wrote ${guideOut}: ${topicNames.length} topics, instructions ${instructions.length} characters, version ${guideVersion}`);
  mkdirSync(dirname(provOut), { recursive: true });
  writeFileSync(provOut, provText);
  console.log(`wrote ${provOut}: ${Object.keys(providers).length} providers`);
}
