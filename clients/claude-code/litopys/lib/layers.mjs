// Layers in Claude Code: the source switch files | memory, CLAUDE.md as a layer rule, touching
// another project, sub-agents.
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

import { ApiError, call, projectFor, readJSON, writeJSON } from "./core.mjs";

export const MEMORY_LABEL = "layer-memory"; // a project whose layer lives in Litopys, not in CLAUDE.md files
export const CLAUDE_MD_KEY = "CLAUDE.md"; // the instruction fact that holds the folder's CLAUDE.md
const MAX_RULE = 8192; // fact value limit

export const sha = (text) => createHash("sha256").update(text.replace(/\r\n/g, "\n")).digest("hex");

export const modeOf = (project) => ((project?.labels || []).includes(MEMORY_LABEL) ? "memory" : "files");

// layerText renders a resolved stack as rules. In files mode the CLAUDE.md rule is left out: Claude Code loads
// the file itself, the copy would only double it. Agents and skills Claude Code has loaded from generated files
// (materialize, not fresh) are named under the name it registered them by, not repeated.
export function layerText(layers, { mode = "files", materialized = [], heading = "## Rules of the layers (global → projects → project; follow them, the narrower one already won)" } = {}) {
  const all = (layers?.stack || []).filter((r) => !(mode === "files" && r.kind === "instruction" && r.key === CLAUDE_MD_KEY));
  const loaded = (r) => materialized.find((m) => !m.fresh && m.kind === r.kind && m.key === r.key);
  const rules = all.filter((r) => !loaded(r));
  const files = all.filter(loaded);
  if (!rules.length && !files.length) return "";
  const lines = [heading];
  for (const r of rules) lines.push(`- ${r.kind} \`${r.key}\` [${r.layer}: ${r.source}]: ${r.value ?? ""}`);
  if (files.length) lines.push(`- loaded by Claude Code from .claude (generated litopys-* files): ${files.map((r) => `${r.kind} \`${loaded(r).name}\``).join(", ")}`);
  if (layers.overridden) lines.push(`(${layers.overridden} broader rule(s) overridden by narrower ones)`);
  return lines.join("\n");
}

// claudeMdNote: in files mode, whether the folder's CLAUDE.md and its copy in Litopys agree (the
// divergence must be visible). Returns one line or "".
export function claudeMdNote(folder, layers, project) {
  const file = join(folder, "CLAUDE.md");
  const copy = (layers?.stack || []).find((r) => r.kind === "instruction" && r.key === CLAUDE_MD_KEY && r.source === project);
  if (!existsSync(file)) return copy ? `Litopys keeps a CLAUDE.md of ${project} that this folder does not have (/litopys:mode files restores it).` : "";
  const text = readFileSync(file, "utf8");
  if (!copy) return `CLAUDE.md of this folder is not in Litopys yet (/litopys:layers sync).`;
  return sha(text) === sha(copy.value || "") ? "" : `CLAUDE.md of this folder differs from its copy in Litopys (/litopys:layers sync takes the file).`;
}

// syncClaudeMd: the folder's CLAUDE.md becomes the project's instruction rule (files mode: the file wins).
export async function syncClaudeMd(cfg, project, folder, opts) {
  const file = join(folder, "CLAUDE.md");
  if (!existsSync(file)) throw new ApiError("no_file", `${file} does not exist`);
  const text = readFileSync(file, "utf8").replace(/\r\n/g, "\n");
  if (text.length > MAX_RULE) throw new ApiError("too_long", `CLAUDE.md has ${text.length} characters, a rule holds ${MAX_RULE}: split it into several rules (/litopys:layers rule)`);
  await call(cfg, "fact_record", { project, kind: "instruction", key: CLAUDE_MD_KEY, value: text }, opts);
  return { file, chars: text.length };
}

// settings.local.json of the folder: claudeMdExcludes switches the file off in memory mode
function settingsFile(folder) {
  return join(folder, ".claude", "settings.local.json");
}

export function setExclude(folder, on) {
  const file = settingsFile(folder);
  const s = readJSON(file, {});
  const pattern = resolve(folder, "CLAUDE.md").replace(/\\/g, "/");
  const list = (s.claudeMdExcludes || []).filter((p) => p !== pattern);
  if (on) list.push(pattern);
  if (list.length) s.claudeMdExcludes = list;
  else delete s.claudeMdExcludes;
  writeJSON(file, s);
  return pattern;
}

// setMode switches a bound project between files and memory: memory = the rule in Litopys is the source,
// the file is excluded; files = the file is the source again and is restored from Litopys when it is missing.
export async function setMode(cfg, mode, folder, opts) {
  const bound = projectFor(cfg, folder);
  if (!bound) throw new ApiError("not_bound", `${folder} is not bound to a project (/litopys:start <project>)`);
  const list = (await call(cfg, "project_list", {}, opts)).items || [];
  const p = list.find((x) => x.name === bound.project);
  if (!p) throw new ApiError("project_not_found", `no project "${bound.project}"`);
  const labels = (p.labels || []).filter((l) => l !== MEMORY_LABEL);
  const root = bound.path;
  const notes = [];
  if (mode === "memory") {
    if (existsSync(join(root, "CLAUDE.md"))) {
      const r = await syncClaudeMd(cfg, p.name, root, opts);
      notes.push(`CLAUDE.md (${r.chars} characters) is now the rule instruction/${CLAUDE_MD_KEY} of ${p.name}.`);
    }
    await call(cfg, "project_amend", { project: p.name, labels: [...labels, MEMORY_LABEL] }, opts);
    const pattern = setExclude(root, true);
    notes.push(`Claude Code skips ${pattern} (claudeMdExcludes in .claude/settings.local.json); the rule comes from Litopys at session start.`);
  } else {
    const layers = await call(cfg, "layer_list", { project: p.name }, opts);
    const copy = (layers.stack || []).find((r) => r.kind === "instruction" && r.key === CLAUDE_MD_KEY && r.source === p.name);
    const file = join(root, "CLAUDE.md");
    if (!existsSync(file) && copy?.value) {
      mkdirSync(dirname(file), { recursive: true });
      writeFileSync(file, copy.value);
      notes.push(`CLAUDE.md restored from Litopys (${copy.value.length} characters).`);
    }
    if (labels.length !== (p.labels || []).length) await call(cfg, "project_amend", { project: p.name, labels }, opts);
    setExclude(root, false);
    notes.push("The file is the source again; Litopys keeps its copy for the divergence check.");
  }
  return { project: p.name, mode, folder: root, notes };
}

// touchPath: the file a tool worked on (Read / Edit / Write / MultiEdit / NotebookEdit / Glob / Grep)
export function touchPath(input) {
  const t = input.tool_input || {};
  return t.file_path || t.notebook_path || t.path || "";
}
