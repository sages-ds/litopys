// What /litopys:doctor knows beyond the token and the server: which surface it runs on, what to set up there
// and the read-only checks of Claude Desktop on Windows (K10).
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { basename, join, resolve } from "node:path";

import { canonicalUrl, readJSON } from "./core.mjs";

const SURFACES = ["terminal", "cloud", "cowork"];

// surfaceOf: --surface wins; otherwise a cloud session says so in its environment and the Cowork working folder
// is named "claude" (the same rule as the start hook).
export function surfaceOf(flag, env, path) {
  if (flag) {
    if (!SURFACES.includes(flag)) throw new Error(`--surface: one of ${SURFACES.join(", ")}`);
    return flag;
  }
  if (env.CLAUDE_CODE_REMOTE === "true") return "cloud";
  return /^claude$/i.test(basename(resolve(path))) ? "cowork" : "terminal";
}

// mcpMismatch: a built plugin has a fixed MCP address, and config() reports a LITOPYS_URL that differs from install.json.
// This compares with .mcp.json itself, in case the two files disagree: returns the fixed address when it differs from
// the hooks' server. The source plugin (a template) has nothing to compare.
export function mcpMismatch(cfg, file = new URL("../.mcp.json", import.meta.url)) {
  const url = readJSON(file, null)?.mcpServers?.litopys?.url;
  if (!url || url.includes("${") || !cfg.url) return "";
  return canonicalUrl(url) === canonicalUrl(`${cfg.url}/v1/mcp`) ? "" : url;
}

export function setupAdvice(surface) {
  const common = [
    "Instructions for the model can live in several places (Claude's menu names change; see its help if one is missing):",
    "  - for the whole organization: the organization's instructions, set by the Owner;",
    "  - for one person: personal instructions in Settings (Cowork calls them global instructions);",
    "  - for a claude.ai project: that project's instructions;",
    "  - for a cloud session: CLAUDE.md of the repository.",
    "Text to put there:",
    "  Litopys is the memory of my projects. Before answering about them, search it (recall_search). Open a session with",
    "  project_resume at the first message and close it with session_close and a summary.",
    "To tie the text to one project, add: Litopys project: <name>",
    "Connector tool permissions are set by the Owner for everyone:",
    "  Always allow reading and ordinary writes; Needs approval for deletion_* and actor_*.",
  ];
  if (surface === "terminal") {
    return ["Terminal: the token comes from LITOPYS_TOKEN; the address comes from a built marketplace or from LITOPYS_URL. Hooks open the session.", ...common];
  }
  return [
    "Connector mode: no token here, and none is needed. The memory tools come from the Litopys connector; in a cloud session",
    "they appear a few seconds after the start. The model opens the session; the plugin keeps it for the conversation.",
    "Connect it once: Settings > Connectors > Litopys > Connect, then sign in.",
    ...common,
  ];
}

// a built plugin's version ends with the hash of its address (build-marketplace.mjs); Claude Code's cache folder
// spells the same "+" as "-"
const versionParts = (v) => String(v).split(/[+-]/)[0].split(".").map(Number);
const compareVersions = (a, b) => {
  const pa = versionParts(a);
  const pb = versionParts(b);
  for (let i = 0; i < 3; i++) {
    if ((pa[i] || 0) !== (pb[i] || 0)) return (pa[i] || 0) - (pb[i] || 0);
  }
  return 0;
};

// pluginCopies: versions of the Litopys plugin kept in the folder of local Cowork. The depth is limited: this runs
// on every doctor and the folder holds whole sessions.
function pluginCopies(dir, depth = 8) {
  const found = [];
  const walk = (d, left) => {
    let entries;
    try {
      entries = readdirSync(d, { withFileTypes: true });
    } catch {
      return;
    }
    for (const e of entries) {
      if (!e.isDirectory() || e.name === "node_modules") continue;
      const p = join(d, e.name);
      if (e.name === ".claude-plugin") {
        const m = readJSON(join(p, "plugin.json"), null);
        if (m?.name === "litopys" && m.version) found.push(m.version);
      } else if (left > 0) walk(p, left - 1);
    }
  };
  walk(dir, depth);
  return found;
}

// appDataWarning: only a UNC path is recognised, not a mapped network drive.
export function appDataWarning(appData) {
  return appData.startsWith("\\\\")
    ? "APPDATA is a network path. Desktop can fall behind there: an outdated Claude Code in its Code tab, outdated plugins in " +
      "local Cowork. IT can exempt AppData from folder redirection; the Claude Code CLI is not affected."
    : "";
}

// desktopChecks: read-only findings about Claude Desktop on Windows; each is advice, not a failure of the plugin.
export function desktopChecks({ platform = process.platform, env = process.env, url = "", version = pluginVersion() } = {}) {
  if (platform !== "win32" || !env.APPDATA) return [];
  const warnings = [];
  const network = appDataWarning(env.APPDATA);
  if (network) warnings.push(network);
  const claude = join(env.APPDATA, "Claude");
  const configFile = join(claude, "claude_desktop_config.json");
  if (existsSync(configFile)) {
    let servers = {};
    try {
      servers = JSON.parse(readFileSync(configFile, "utf8")).mcpServers || {};
    } catch {
      warnings.push(`${configFile} is not valid JSON, so local MCP bridges were not checked.`);
    }
    for (const [name, s] of Object.entries(servers)) {
      // only the command and args: env and headers may hold secrets and are never read
      const line = [s?.command, ...(Array.isArray(s?.args) ? s.args : [])].join(" ");
      const ours = /litopys/i.test(name) || /litopys/i.test(line) || (url && line.includes(url));
      if (/mcp-remote/.test(line) && ours) {
        warnings.push(`A local bridge "${name}" (mcp-remote) in Desktop's config runs beside the connector with the token's full rights. ` +
          "Delete it from Desktop's local MCP servers and restart Desktop.");
      }
    }
  }
  const old = pluginCopies(join(claude, "local-agent-mode-sessions")).filter((v) => compareVersions(v, version) < 0);
  if (old.length) {
    warnings.push(`Local Cowork keeps old copies of this plugin (${[...new Set(old)].join(", ")}; current ${version}), so outdated hooks can still run there. ` +
      "Update the plugin in Cowork; if APPDATA is a network path, see the advice for that.");
  }
  return warnings;
}

function pluginVersion() {
  return readJSON(new URL("../.claude-plugin/plugin.json", import.meta.url), {}).version || "0";
}
