// Hook logic of the plugin. Each function takes the hook input and returns the stdout text;
// the files in hooks/ only read stdin and print. A hook never breaks the session: failures turn into
// one line of context or silence, always exit 0.
import { homedir } from "node:os";
import { resolve } from "node:path";

import { addressConflictText, call, config, contextOutput, noTokenText, projectFor, readMarker, slugOf, userEnvHas, writeMarker } from "./core.mjs";
import { layerText, touchPath } from "./layers.mjs";
import { openFor, resolveProject, startPackage } from "./session.mjs";

const FILE_TOOLS = /^(Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep)$/;

// touch (PostToolUse on file tools): work reached a file of ANOTHER bound project — the model
// gets that project's rules once per project per session, next to the tool result (as Claude Code does with a
// nested CLAUDE.md). A sub-agent has its own context, so it counts its own touches: the hook input carries its
// agent_id, the main agent has none (checked live on Claude Code 2.1.296). The binding cache answers,
// no sync here: the hook must stay fast.
export async function touch(input, cfg = config(), opts = {}) {
  const path = touchPath(input);
  if (!path) return "";
  const marker = readMarker(cfg, input.session_id);
  if (!marker?.session_id) return "";
  const bound = projectFor(cfg, path);
  if (!bound || bound.project === marker.project) return "";
  const agent = input.agent_id;
  const touched = (agent ? marker.touched_by?.[agent] : marker.touched) || [];
  if (touched.includes(bound.project)) return "";
  if (agent) marker.touched_by = { ...(marker.touched_by || {}), [agent]: [...touched, bound.project] };
  else marker.touched = [...touched, bound.project];
  writeMarker(cfg, input.session_id, marker); // once, even if the call below fails
  const layers = await call(cfg, "layer_list", { project: bound.project }, { timeoutMs: 4000, ...opts });
  const text = layerText(layers, { mode: "memory", heading: `## Rules of project ${bound.project} (this work touches its folder ${bound.path})` });
  return text ? contextOutput("PostToolUse", text + `\nThe session stays in ${marker.project}; write about ${bound.project} with project "${bound.project}".`) : "";
}

// SubagentStart: a sub-agent gets the rules of the session's project (kept in the marker at session start).
export function subagentStart(input, cfg = config()) {
  const marker = readMarker(cfg, input.session_id);
  if (!marker?.layers_text) return "";
  return contextOutput("SubagentStart", `${marker.layers_text}\n(Project ${marker.project}; you are a sub-agent: record proposals with agent set to your role.)`);
}

// SubagentStop: the sub-agent's context is gone, so are its touches
export function subagentStop(input, cfg = config()) {
  const marker = readMarker(cfg, input.session_id);
  if (!input.agent_id || !marker?.touched_by?.[input.agent_id]) return "";
  const { [input.agent_id]: _, ...rest } = marker.touched_by;
  writeMarker(cfg, input.session_id, { ...marker, touched_by: rest });
  return "";
}

// PreToolUse on the Litopys tools, the plugin's and a connector's: when LITOPYS_URL disagrees with the built address,
// memory is off, and a tool would reach a server the person does not expect, e.g. test records in the live memory.
export function preToolUse(input, cfg = config()) {
  if (!cfg.conflict) return "";
  return JSON.stringify({
    hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: `Litopys: ${addressConflictText(cfg)}.` },
  });
}

// SessionStart (startup | resume | compact): open or continue this terminal's session in the project
// of the folder and hand the model the start package (П4, П5).
export async function sessionStart(input, cfg = config(), opts = {}) {
  const terminal = input.session_id;
  const cwd = input.cwd || process.cwd();
  if (cfg.conflict) return contextOutput("SessionStart", `Litopys: ${addressConflictText(cfg)}. Project memory is off in this session (/litopys:doctor).`);
  if (!cfg.token) return connectorStart(input, cfg, (opts.userEnvHas || userEnvHas)("LITOPYS_TOKEN"));
  try {
    // after /compact (and on resume) the terminal keeps its project, whatever the folder
    const marker = input.source === "compact" || input.source === "resume" ? readMarker(cfg, terminal) : null;
    const where = marker?.project ? { project: marker.project, source: "marker" } : await resolveProject(cfg, cwd, opts);
    if (!where) {
      return contextOutput("SessionStart", `Litopys: the folder ${cwd} is not bound to a project. ` +
        "To work with memory: /litopys:start <project> (it also binds this folder); project_resume without arguments lists the projects.");
    }
    const opened = await openFor(cfg, terminal, where.project, opts);
    return contextOutput("SessionStart", startPackage(cfg, terminal, cwd, where, opened));
  } catch (e) {
    if (e.code === "no_server") return contextOutput("SessionStart", `Litopys: ${e.message}. Project memory is off in this session (/litopys:doctor).`);
    return contextOutput("SessionStart", `Litopys: could not open the session (${e.code || "error"}: ${e.message}). ` +
      "Memory tools still work; /litopys:doctor explains.");
  }
}

// Folders of Claude Desktop and Cowork that are not a person's work: a background probe of Desktop
// starts the hook with nobody behind it, so it opens nothing; the Cowork working folder names no project.
const PROBE_FOLDER = /^claude-sandbox-status-/i;
const COWORK_FOLDER = /^claude$/i;

// connectorStart: SessionStart without LITOPYS_TOKEN — connector mode. In a cloud session
// (claude.ai/code) and in Cowork the plugin comes without a token while the memory tools come from the Litopys
// connector, so the hook cannot open the session itself: it tells the model to open it, now, with the connector's
// project_resume, and postToolUse keeps the session in the marker from that call's answer.
export function connectorStart(input, cfg, inUserEnv) {
  const cwd = input.cwd || process.cwd();
  const folder = slugOf(cwd);
  if (PROBE_FOLDER.test(folder)) return "";
  const kept = input.source === "resume" || input.source === "compact" ? readMarker(cfg, input.session_id) : null;
  writeMarker(cfg, input.session_id, { ...(kept || { sessions: {} }), connector: true });
  const lines = [
    "# Litopys — project memory (connector mode)",
    "There is no LITOPYS_TOKEN here, so the memory tools come from the Litopys connector (their names contain \"litopys\"). " +
      "In a cloud session or in Cowork this is expected: nothing to install, update or configure. In a cloud session the " +
      "connector's tools appear a few seconds after the start — look again before deciding there are none.",
    "",
    "Before you answer the person's first message — whatever it is, a greeting too — open the memory session:",
  ];
  if (kept?.project) {
    lines.push(`- this conversation continues project ${kept.project}: call project_resume with project "${kept.project}" ` +
      "(it continues this client's open session there).");
  } else {
    const home = resolve(cwd) === resolve(homedir());
    const named = COWORK_FOLDER.test(folder) || home ? "" : folder;
    lines.push("- if the first message names a project, call project_resume with that project;");
    lines.push(named
      ? `- otherwise call project_resume without arguments (the catalog of projects) and compare the names with the folder "${named}": ` +
        "an exact match → that project; partial matches → offer them and \"global\" and let the person choose; " +
        "no match → project \"global\" with auto_create true."
      : "- otherwise call project_resume with project \"global\" and auto_create true (this folder names no project).");
  }
  lines.push(
    "Pass the session_id from its answer to every write; the plugin keeps it for this conversation by itself.",
    "To move to another project later: project_resume of the new project first, then session_close of the old one with a summary.",
    "",
    `If there are no Litopys tools at all, project memory is off in this session. ${noTokenText(inUserEnv)} (/litopys:doctor)`,
  );
  return contextOutput("SessionStart", lines.join("\n"));
}

// followOpen: in connector mode the model opens the session (project_resume or session_open of the connector); the
// marker takes the session from that call's answer. The same session keeps its opened_at (resume of a conversation,
// K6); another project is a switch (K4).
function followOpen(input, cfg) {
  const marker = readMarker(cfg, input.session_id);
  if (!marker?.connector) return;
  const res = resultOf(input.tool_response);
  const session = res?.session;
  if (!session?.session_id) return; // the catalog (no project) opens nothing
  const project = res.bundle?.project?.name || input.tool_input?.project;
  if (!project) return;
  const known = marker.sessions?.[project];
  const openedAt = known?.session_id === session.session_id ? known.opened_at : session.opened_at || new Date().toISOString();
  marker.sessions = { ...(marker.sessions || {}), [project]: { session_id: session.session_id, opened_at: openedAt } };
  if (marker.session_id !== session.session_id) marker.last_checkpoint_at = new Date().toISOString();
  marker.project = project;
  marker.session_id = session.session_id;
  writeMarker(cfg, input.session_id, marker);
}

// resultOf: the JSON answer of an MCP tool as the hook input carries it — structured content, content blocks or text
function resultOf(r) {
  if (!r) return null;
  if (typeof r === "string") {
    try {
      return JSON.parse(r);
    } catch {
      return null;
    }
  }
  if (Array.isArray(r)) return resultOf(r.find((b) => b?.type === "text")?.text);
  if (r.structuredContent) return r.structuredContent;
  if (Array.isArray(r.content)) return resultOf(r.content);
  return r;
}

// write tools of the native API (MCP names) that count as work in the session
const WRITES = /__(statement_(assert|amend|retract|reinstate|supersede)|task_(declare|amend|close|reopen|retract)|fact_(record|retire)|link_(assert|retire)|project_(declare|amend|transition))$/;
const CHECKPOINT = /__session_(checkpoint|close)$/;

// file tools that change files: work worth a record even when nothing was written to memory
const EDIT_TOOLS = /^(Edit|Write|MultiEdit|NotebookEdit)$/;

// PostToolUse on the Litopys MCP tools: count writes since the last checkpoint (П6); on file tools: count edits since
// the last write to memory (owner decision 2026-10-30). Local only.
export async function postToolUse(input, cfg = config(), opts = {}) {
  const name = input.tool_name || "";
  if (FILE_TOOLS.test(name)) {
    if (EDIT_TOOLS.test(name) && !isError(input)) countEdit(input, cfg);
    return touch(input, cfg, opts);
  }
  if (!name.startsWith("mcp__") || !/litopys/i.test(name)) return "";
  if (/__(project_resume|session_open)$/.test(name) && !isError(input)) {
    followOpen(input, cfg);
    return "";
  }
  const marker = readMarker(cfg, input.session_id);
  if (!marker?.session_id) return "";
  if (/__session_close$/.test(name) && !isError(input)) {
    // the terminal's session of this project is over: the marker forgets it (a later start opens anew)
    const closed = input.tool_input?.session_id || marker.session_id;
    const sessions = Object.fromEntries(Object.entries(marker.sessions || {}).filter(([, s]) => s.session_id !== closed));
    const left = marker.connector ? { sessions, connector: true } : { sessions };
    writeMarker(cfg, input.session_id, closed === marker.session_id ? left : { ...marker, sessions });
    return "";
  }
  if (CHECKPOINT.test(name)) {
    marker.writes_since_checkpoint = 0;
    marker.last_checkpoint_at = new Date().toISOString();
    delete marker.reminded_at;
  } else if (WRITES.test(name)) {
    marker.writes_since_checkpoint = (marker.writes_since_checkpoint || 0) + 1;
  } else {
    return "";
  }
  marker.edits_since_write = 0; // the work reached memory
  writeMarker(cfg, input.session_id, marker);
  return "";
}

// countEdit: one more file edit in this terminal's session (a sub-agent's edit is the session's work too)
function countEdit(input, cfg) {
  const marker = readMarker(cfg, input.session_id);
  if (!marker?.session_id) return;
  marker.edits_since_write = (marker.edits_since_write || 0) + 1;
  writeMarker(cfg, input.session_id, marker);
}

// isError: the MCP tool answered with an error (nothing happened on the server)
function isError(input) {
  const r = input.tool_response;
  return !!(r && (r.isError || r.is_error || (typeof r === "object" && r.error)));
}

const minutesSince =(iso, now) => (iso ? (now - Date.parse(iso)) / 60000 : Infinity);

// Stop: once per interval, when the session has writes since its last checkpoint or file edits since its last write
// to memory, ask the model to save a checkpoint. stop_hook_active (a stop that this hook
// already continued) is never blocked again.
export function stop(input, cfg = config(), now = Date.now()) {
  if (input.stop_hook_active || cfg.checkpointMinutes === 0 || cfg.conflict) return ""; // the checkpoint tool is denied
  const marker = readMarker(cfg, input.session_id);
  const writes = marker?.writes_since_checkpoint || 0, edits = marker?.edits_since_write || 0;
  if (!marker?.session_id || (writes === 0 && edits === 0)) return "";
  if (minutesSince(marker.last_checkpoint_at, now) < cfg.checkpointMinutes) return "";
  if (minutesSince(marker.reminded_at, now) < cfg.checkpointMinutes) return "";
  marker.reminded_at = new Date(now).toISOString();
  writeMarker(cfg, input.session_id, marker);
  const what = [writes && `${writes} write(s) in project ${marker.project} since the last checkpoint`,
    edits && `${edits} file edit(s) since the last write to memory`].filter(Boolean).join(" and ");
  return JSON.stringify({
    decision: "block",
    reason: `Litopys: ${what}. ` +
      (edits ? "Record what the edits decided or found (statement_assert) if it is worth keeping; " : "") +
      `call session_checkpoint with session_id "${marker.session_id}", a summary of what was done and "current" (where the work stands), then finish your answer. ` +
      "Do not close the session: the person closes it with /litopys:stop.",
  });
}
