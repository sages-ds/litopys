// Session logic of the plugin, shared by the hooks and the CLI.
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join, resolve } from "node:path";

import { ApiError, cacheAge, call, projectFor, readMarker, removeMarker, slugOf, syncBindings, writeMarker } from "./core.mjs";
import { claudeMdNote, layerText, modeOf } from "./layers.mjs";
import { materialize } from "./materialize.mjs";

// resolveProject: binding first (the cache, refreshed from Litopys when older than syncMinutes or when it
// does not know the folder), then a project whose slug is the folder name (only if it exists and is not
// archived: a folder name is a guess, an archived project needs a person's choice).
export async function resolveProject(cfg, cwd, opts) {
  let bound = projectFor(cfg, cwd);
  if (!bound || cacheAge(cfg) > cfg.syncMinutes) {
    try {
      await syncBindings(cfg, opts);
      bound = projectFor(cfg, cwd);
    } catch (e) {
      if (e.code === "no_server" || e.code === "address_conflict") throw e; // not offline: nowhere to connect
      // offline: the cache answers
    }
  }
  if (bound) return bound;
  const slug = slugOf(cwd);
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(slug)) return null;
  const list = await call(cfg, "project_list", {}, opts);
  const items = list.items || list.projects || [];
  return items.some((p) => p.name === slug && p.status !== "archived") ? { project: slug, path: cwd, source: "folder-name" } : null;
}

// startPackage: after openFor — the agents and skills of the layers become files in the project's bound folder
// (both modes, P5; never the home folder, its .claude holds the user-level ones) and the start text
// names the ones Claude Code has loaded. Shared by SessionStart and /litopys:start.
export function startPackage(cfg, terminal, cwd, where, opened) {
  // the folder of THIS project only: a terminal kept on another project must not get its files here
  const bound = projectFor(cfg, cwd);
  const here = where.source === "marker" ? bound : where;
  const folder = here?.project === where.project ? here.path : undefined;
  const bundle = opened.resume.bundle;
  const target = bound?.project === where.project && resolve(bound.path) !== resolve(homedir()) ? bound.path : undefined;
  let materialized = [];
  let note = "";
  if (target) {
    try {
      const m = materialize(target, bundle?.layers);
      materialized = m.present;
      if (m.written.length || m.removed.length) {
        note += `\n\nAgents and skills from Litopys: ${m.written.length} written, ${m.removed.length} removed in ${join(target, ".claude")}.`;
      }
      // Claude Code loads agents and skills when a session starts: a file written now works from the next start,
      // until then its rule comes as text (here and to sub-agents)
      if (m.written.length) note += " Files written now are loaded by Claude Code from the next session start; until then their rules are given as text.";
      if (m.skipped.length) note += `\nSkipped: ${m.skipped.join("; ")}`;
    } catch (e) {
      note += `\n\nAgents and skills were not written: ${e.message}`;
    }
  }
  if (materialized.length) {
    // sub-agents get the same: loaded files named, fresh ones as text
    const marker = readMarker(cfg, terminal);
    marker.layers_text = layerText(bundle?.layers, { mode: modeOf(bundle?.project), materialized });
    writeMarker(cfg, terminal, marker);
  }
  return startText({ project: where.project, source: where.source, folder, materialized, ...opened }) + note;
}

// openFor: opens (or continues) the session of this terminal in a project and returns the start package.
// The marker remembers the session per project, so coming back to a project continues it.
export async function openFor(cfg, terminal, project, opts) {
  const marker = readMarker(cfg, terminal) || { sessions: {} };
  const known = marker.sessions?.[project]?.session_id;
  const open = await call(cfg, "session_open", { project, attach: known || "none" }, opts);
  const session = open.session;
  const resume = await call(cfg, "project_resume", { project, budget_tokens: cfg.budgetTokens }, opts);
  marker.sessions = { ...(marker.sessions || {}), [project]: { session_id: session.session_id, opened_at: session.opened_at || new Date().toISOString() } };
  marker.project = project;
  marker.session_id = session.session_id;
  marker.last_checkpoint_at = marker.last_checkpoint_at || new Date().toISOString();
  // for sub-agents (SubagentStart) without a network call; touched projects start over with the context
  marker.layers_text = layerText(resume.bundle?.layers, { mode: modeOf(resume.bundle?.project) });
  marker.touched = [];
  marker.touched_by = {}; // per sub-agent (agent_id of the hook input)
  writeMarker(cfg, terminal, marker);
  return { session, attached: !!open.attached, attachMiss: open.attach_miss, resume };
}

// closeFor: closes the terminal's session of the current project with a summary and, when given, the outcomes
// per subject and next steps.
export async function closeFor(cfg, terminal, summary, opts, { outcomes, next_steps } = {}) {
  const marker = readMarker(cfg, terminal);
  if (!marker?.session_id) throw new ApiError("no_session", "this terminal has no open Litopys session");
  const args = { session_id: marker.session_id, summary };
  if (outcomes?.length) args.outcomes = outcomes;
  if (next_steps?.length) args.next_steps = next_steps;
  // a session closed already (idle sweep, another client) answers already_closed; its summary is kept
  const res = await call(cfg, "session_close", args, opts);
  const already = !!res.already_closed;
  const { [marker.project]: _, ...rest } = marker.sessions || {};
  if (Object.keys(rest).length) {
    writeMarker(cfg, terminal, { sessions: rest });
  } else {
    removeMarker(cfg, terminal);
  }
  return { project: marker.project, session_id: marker.session_id, already, missing: res.missing_outcomes || [], hints: res.hints || [] };
}

// openOutcomes: what earlier sessions left unfinished, one line per subject (project_resume open_outcomes).
function openOutcomes(list) {
  return list.map((o) => `- ${o.ref || o.topic || o.statement_id} [${o.state}]: ${o.result}` + (o.next ? ` → next: ${o.next}` : ""));
}

// startText: what the model reads at the start of a session (hook additionalContext).
export function startText({ project, source, session, attached, attachMiss, resume, folder, materialized = [] }) {
  const lines = [
    `# Litopys — project memory`,
    `Project: ${project} (${{ binding: "bound folder", marker: "kept by this terminal" }[source] || "folder name"}). Session of this terminal: ${session.session_id}` +
      (attached ? " (continued)." : attachMiss ? ` (new; the previous one: ${attachMiss}).` : " (new)."),
    `Pass session_id "${session.session_id}" to every write (statement_assert, task_*, session_checkpoint, session_close):` +
      ` other terminals of this client may have their own sessions in the same project.`,
    `Save decisions and findings as you go (statement_assert). Close with /litopys:stop: a summary plus outcomes — per task, record or topic moved, what came of it, its state and the next step.`,
  ];
  const bundle = resume.bundle ? { ...resume.bundle } : resume;
  const layers = bundle.layers;
  const mode = modeOf(bundle.project);
  if (bundle.project?.status === "archived") {
    lines.splice(1, 0, `ATTENTION: project ${project} is ARCHIVED. Tell the person before writing to it; ` +
      `project_transition to "active" brings it back, or /litopys:start <project> moves this terminal to another one.`);
  }
  // the resolved stack: already narrowed — one rule per kind/key, the narrower layer won
  const rules = layerText(layers, { mode, materialized });
  if (rules) lines.push("", rules);
  if (mode === "memory") lines.push("", `Layer source of ${project}: Litopys (memory mode) — the CLAUDE.md files of its folder are switched off.`);
  else if (folder) {
    const note = claudeMdNote(folder, layers, project);
    if (note) lines.push("", note);
    // read-only sub-agents (Explore) start without CLAUDE.md, general-purpose ones load it; the plugin
    // does not add it — whether a sub-agent needs it is the main model's call
    if (existsSync(join(folder, "CLAUDE.md"))) {
      lines.push("", "Sub-agents such as Explore start without the CLAUDE.md files (general-purpose ones load them): " +
        "when a rule from CLAUDE.md matters for a sub-agent's task, put it into the sub-agent's prompt.");
    }
  }
  delete bundle.layers;
  lines.push("", "## Where the project stands (project_resume)", JSON.stringify(bundle));
  if (resume.next_steps?.length) lines.push("", "Next steps: " + resume.next_steps.join(" · "));
  if (resume.open_outcomes?.length) {
    lines.push("", "## Left unfinished by earlier sessions (latest outcome per subject)", ...openOutcomes(resume.open_outcomes));
  }
  return lines.join("\n");
}
