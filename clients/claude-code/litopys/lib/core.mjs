// Shared code of the Litopys plugin for Claude Code: configuration, the native API client,
// terminal markers and folder bindings. No dependencies: Node >= 20 only.
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { homedir, hostname } from "node:os";
import { basename, dirname, join, resolve, sep } from "node:path";

// --- configuration (environment only: secrets never live in files) -----------------------

// The server address is the installation's, never the code's: LITOPYS_URL, or install.json written
// by build-marketplace.mjs. A built plugin's MCP tools stay at its fixed address whatever the environment says, so
// conflict names that address when LITOPYS_URL points elsewhere: the hooks and the tools would use different servers.
export function config(env = process.env, installFile = new URL("../install.json", import.meta.url)) {
  const home = env.LITOPYS_HOME || join(env.CLAUDE_CONFIG_DIR || join(homedir(), ".claude"), "litopys");
  const minutes = Number.parseInt(env.LITOPYS_CHECKPOINT_MINUTES ?? "30", 10);
  const installed = readJSON(installFile, {})?.url;
  const fixed = (typeof installed === "string" ? installed : "").replace(/\/+$/, "");
  const url = (env.LITOPYS_URL || fixed).replace(/\/+$/, "");
  return {
    url,
    conflict: fixed && canonicalUrl(url) !== canonicalUrl(fixed) ? fixed : "",
    token: env.LITOPYS_TOKEN || "",
    home,
    checkpointMinutes: Number.isFinite(minutes) && minutes >= 0 ? minutes : 30,
    syncMinutes: Number.parseInt(env.LITOPYS_SYNC_MINUTES ?? "60", 10) || 60,
    budgetTokens: Number.parseInt(env.LITOPYS_BUDGET_TOKENS ?? "2500", 10) || 2500,
  };
}

// canonicalUrl: one spelling of an address (host case, the default port), to compare two of them
export function canonicalUrl(url) {
  try {
    return new URL(url).href.replace(/\/+$/, "");
  } catch {
    return url;
  }
}

// safeUrl: an address for the output, without credentials someone may have put into LITOPYS_URL
export function safeUrl(url) {
  try {
    const u = new URL(url);
    u.username = "";
    u.password = "";
    return u.href.replace(/\/+$/, "");
  } catch {
    return url ? "(not a valid address)" : "";
  }
}

// addressConflictText: why this plugin refuses to work (see config). Refusing keeps test data out of the live memory.
export function addressConflictText(cfg) {
  return `LITOPYS_URL (${safeUrl(cfg.url)}) differs from the server this plugin was built for (${cfg.conflict}), so the hooks and the ` +
    "memory tools would use different servers. Remove LITOPYS_URL, or install a marketplace built for that server";
}

// userEnvHas: whether a variable is set in the Windows USER environment (HKCU\Environment), which a process
// started before setx does not see. Only the exit status is used: the value is never read.
export function userEnvHas(name, platform = process.platform) {
  if (platform !== "win32") return false;
  return spawnSync("reg", ["query", "HKCU\\Environment", "/v", name], { stdio: "ignore", windowsHide: true }).status === 0;
}

// noTokenText: what an empty LITOPYS_TOKEN means for this session, and how to fix it
export function noTokenText(inUserEnv) {
  return inUserEnv
    ? "LITOPYS_TOKEN is in your user environment but not in this process: Claude Code was started before it was set. " +
      "Close every Claude Code window and terminal, then start again."
    : "LITOPYS_TOKEN is not set: issue a token for this client (scripts/issue-plugin-token.ps1, profile core) — it goes into " +
      "the user environment — then restart Claude Code. Until then the litopys MCP tools answer 401 invalid token (empty Bearer).";
}

// --- native API (/v1/<domain>/<verb>) -------------------------------------------------------

export class ApiError extends Error {
  constructor(code, message, hint) {
    super(message);
    this.code = code;
    this.hint = hint;
  }
}

// call POSTs one verb ("session_open" -> /v1/session/open) and returns its JSON result.
export async function call(cfg, tool, args = {}, { timeoutMs = 8000, fetchImpl = globalThis.fetch } = {}) {
  // first: no_token and unreachable leave a binding pending, and that would reach the built server later
  if (cfg.conflict) throw new ApiError("address_conflict", addressConflictText(cfg));
  if (!cfg.token) throw new ApiError("no_token", "LITOPYS_TOKEN is not set");
  if (!cfg.url) throw new ApiError("no_server", "the Litopys server address is not set: set LITOPYS_URL or install the plugin from a built marketplace");
  const i = tool.indexOf("_");
  const path = `/v1/${tool.slice(0, i)}/${tool.slice(i + 1)}`;
  let res;
  try {
    res = await fetchImpl(cfg.url + path, {
      method: "POST",
      headers: { authorization: `Bearer ${cfg.token}`, "content-type": "application/json", "x-litopys-client": "claude-code-plugin" },
      body: JSON.stringify(args),
      signal: AbortSignal.timeout(timeoutMs),
    });
  } catch (e) {
    throw new ApiError("unreachable", `${safeUrl(cfg.url)}: ${e.cause?.code || e.name || e.message}`);
  }
  const text = await res.text();
  let body;
  try {
    body = text ? JSON.parse(text) : {};
  } catch {
    throw new ApiError("bad_response", `${path}: HTTP ${res.status}, not JSON`);
  }
  if (!res.ok || body.error) {
    const e = body.error || {};
    throw new ApiError(e.code || `http_${res.status}`, e.message || `HTTP ${res.status}`, e.hint);
  }
  return body;
}

// --- small file helpers --------------------------------------------------------------------------

export function readJSON(file, fallback) {
  try {
    return JSON.parse(readFileSync(file, "utf8"));
  } catch {
    return fallback;
  }
}

// writeJSON replaces the file atomically (a crash never leaves half a marker).
export function writeJSON(file, value) {
  mkdirSync(dirname(file), { recursive: true });
  const tmp = `${file}.${process.pid}.tmp`;
  writeFileSync(tmp, JSON.stringify(value, null, 2) + "\n", { mode: 0o600 });
  renameSync(tmp, file);
}

export async function readStdin(stream = process.stdin) {
  if (stream.isTTY) return {};
  let data = "";
  for await (const chunk of stream) data += chunk;
  try {
    return data.trim() ? JSON.parse(data) : {};
  } catch {
    return {};
  }
}

// --- terminal markers: a session belongs to the terminal --------------------------------

const safeId = (id) => String(id || "").replace(/[^A-Za-z0-9_-]/g, "");

export function markerPath(cfg, terminal) {
  const id = safeId(terminal);
  if (!id) throw new Error("no terminal id (Claude Code session_id)");
  return join(cfg.home, "terminals", `${id}.json`);
}

export const readMarker = (cfg, terminal) => readJSON(markerPath(cfg, terminal), null);
export const writeMarker = (cfg, terminal, m) => writeJSON(markerPath(cfg, terminal), { ...m, updated_at: new Date().toISOString() });
export const removeMarker = (cfg, terminal) => rmSync(markerPath(cfg, terminal), { force: true });

// --- folder bindings: path -> project, the deepest prefix wins --------------------------
// P2: the source of truth is Litopys (facts of kind binding, one per folder of a machine); bindings.json is
// this machine's cache, refreshed by syncBindings.

const bindingsFile = (cfg) => join(cfg.home, "bindings.json");

export const machine = () => hostname().toLowerCase();

// bindingKey: the fact key of a folder of this machine (a path may be longer than a key may be)
export function bindingKey(path, host = machine()) {
  return `${host}:${createHash("sha1").update(norm(path)).digest("hex").slice(0, 16)}`;
}

function norm(p) {
  let n = resolve(p).replace(/[\\/]+$/, "");
  if (sep === "\\" || /^[A-Za-z]:/.test(n)) n = n.replace(/\//g, "\\").toLowerCase(); // Windows paths are case-insensitive
  return n;
}

const loadCache = (cfg) => readJSON(bindingsFile(cfg), { bindings: [] });

// A pending binding belongs to the server it was made for: several builds of the plugin share this cache, and a
// binding of a stage project must not go up to prod. One without a server (older plugin) stays local too.
const othersPending = (cfg, b) => b.pending && b.server !== safeUrl(cfg.url);

export function loadBindings(cfg) {
  return (loadCache(cfg).bindings || []).filter((b) => !othersPending(cfg, b));
}

function saveBindings(cfg, list, syncedAt) {
  const prev = loadCache(cfg);
  const kept = (prev.bindings || []).filter((b) => othersPending(cfg, b));
  writeJSON(bindingsFile(cfg), { bindings: [...list, ...kept], synced_at: syncedAt ?? prev.synced_at ?? null });
}

// bind records the folder in Litopys (fact binding) and in the cache; offline, it stays local with
// pending: true and goes up with the next sync to the same server.
export async function bind(cfg, path, project, opts) {
  const abs = resolve(path);
  const entry = { path: abs, project, key: bindingKey(abs), bound_at: new Date().toISOString() };
  try {
    await call(cfg, "fact_record", { project, kind: "binding", key: entry.key, value: JSON.stringify({ host: machine(), path: abs }) }, opts);
  } catch (e) {
    if (e.code !== "unreachable" && e.code !== "no_token") throw e;
    entry.pending = true;
    entry.server = safeUrl(cfg.url); // never the credentials someone may have put into LITOPYS_URL
  }
  saveBindings(cfg, [...loadBindings(cfg).filter((b) => norm(b.path) !== norm(abs)), entry]);
  return entry;
}

// unbind retires the binding fact of the folder and drops it from the cache; returns how many were removed.
export async function unbind(cfg, path, opts) {
  const before = loadBindings(cfg);
  const gone = before.filter((b) => norm(b.path) === norm(path));
  for (const b of gone) {
    if (!b.pending) await call(cfg, "fact_retire", { project: b.project, kind: "binding", key: b.key || bindingKey(b.path), reason: "folder unbound" }, opts);
  }
  saveBindings(cfg, before.filter((b) => norm(b.path) !== norm(path)));
  return gone.length;
}

// syncBindings: pending local bindings go up, then the cache becomes what Litopys holds for this machine.
export async function syncBindings(cfg, opts) {
  for (const b of loadBindings(cfg).filter((x) => x.pending)) {
    await call(cfg, "fact_record", { project: b.project, kind: "binding", key: b.key, value: JSON.stringify({ host: machine(), path: b.path }) }, opts);
  }
  const res = await call(cfg, "fact_list", { kind: "binding" }, opts);
  const host = machine();
  const list = [];
  for (const f of res.items || []) {
    let v;
    try {
      v = JSON.parse(f.value || "{}");
    } catch {
      continue;
    }
    if (v.host === host && v.path) list.push({ path: v.path, project: f.project, key: f.key, bound_at: f.provenance?.recorded_at });
  }
  saveBindings(cfg, list, new Date().toISOString());
  return list;
}

// cacheAge: minutes since the last successful sync (Infinity if never)
export function cacheAge(cfg, now = Date.now()) {
  const at = loadCache(cfg).synced_at;
  return at ? (now - Date.parse(at)) / 60000 : Infinity;
}

// projectFor: the binding of cwd or of its nearest bound ancestor; null when none.
export function projectFor(cfg, cwd) {
  const here = norm(cwd);
  let best = null;
  for (const b of loadBindings(cfg)) {
    const p = norm(b.path);
    const inside = here === p || here.startsWith(p + (p.includes("\\") ? "\\" : "/"));
    if (inside && (!best || p.length > norm(best.path).length)) best = b;
  }
  return best ? { project: best.project, path: best.path, source: "binding" } : null;
}

// slugOf: the folder name as a project slug candidate (fallback rule).
export function slugOf(cwd) {
  return basename(resolve(cwd)).trim();
}

// --- hook output -------------------------------------------------------------------------------------

const LIMIT = 9500; // additionalContext is capped at 10 000 characters by Claude Code

export function clip(text, limit = LIMIT) {
  return text.length <= limit ? text : text.slice(0, limit - 40) + "\n… (cut to fit the hook limit)";
}

export function contextOutput(event, text) {
  return JSON.stringify({ hookSpecificOutput: { hookEventName: event, additionalContext: clip(text) } });
}
