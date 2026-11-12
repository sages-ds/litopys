#!/usr/bin/env node
// CLI of the Litopys plugin: the deterministic part of its commands.
//
//   litopys.mjs status  --terminal <id>                  this terminal's project and session
//   litopys.mjs start   <project> --terminal <id> [--path <dir>] [--create]
//                                                        bind the folder, open/continue the session, print the start package
//   litopys.mjs bind    <project> [--path <dir>]         bind a folder (default: current) to a project (in Litopys)
//   litopys.mjs unbind  [--path <dir>]
//   litopys.mjs new     <title> [--path <dir>] [--summary <text>]
//                                                        a new project; the folder (created and bound) is optional
//   litopys.mjs projects                                 the registry: projects and their folders on this machine
//   litopys.mjs space   <project> <space>                the project becomes a member of the space (layer projects)
//   litopys.mjs global  <project>                        the project holds the owner's global layer
//   litopys.mjs mode    files|memory [--path <dir>]      where the layer of the bound project lives
//   litopys.mjs sync-claude-md [--path <dir>]            the folder's CLAUDE.md becomes the project's rule
//   litopys.mjs doctor  [--terminal <id>] [--path <dir>] [--surface terminal|cloud|cowork] [--advice]
//                                                        what works and what does not, with the fix; --advice adds setup advice
import { mkdirSync } from "node:fs";
import { resolve } from "node:path";

import { addressConflictText, ApiError, bind, call, config, loadBindings, noTokenText, projectFor, readMarker, safeUrl, syncBindings, unbind, userEnvHas } from "../lib/core.mjs";
import { desktopChecks, mcpMismatch, setupAdvice, surfaceOf } from "../lib/doctor.mjs";
import { setMode, syncClaudeMd } from "../lib/layers.mjs";
import { materialize } from "../lib/materialize.mjs";
import { openFor, resolveProject, startPackage } from "../lib/session.mjs";

export async function main(argv, cfg = config(), opts = {}, out = (s) => process.stdout.write(s + "\n")) {
  const args = [];
  const flags = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--create" || a === "--advice") flags[a.slice(2)] = true;
    else if (a.startsWith("--")) flags[a.slice(2)] = argv[++i];
    else args.push(a);
  }
  const [cmd, ...rest] = args;
  const path = flags.path || process.cwd();
  const terminal = flags.terminal;

  switch (cmd) {
    case "status": {
      const m = terminal ? readMarker(cfg, terminal) : null;
      const b = projectFor(cfg, path);
      out(m?.session_id
        ? `Litopys: project ${m.project}, session ${m.session_id}; writes since the last checkpoint: ${m.writes_since_checkpoint || 0}.`
        : "Litopys: no open session in this terminal.");
      out(b ? `Folder ${path} is bound to ${b.project} (${b.path}).` : `Folder ${path} is not bound to a project.`);
      return 0;
    }
    case "start": {
      const project = rest.join(" ").trim();
      if (!project || !terminal) throw new ApiError("usage", "start <project> --terminal <Claude Code session id> [--path <dir>] [--create]");
      let slug = project;
      if (flags.create) {
        // project_declare derives the slug from a human title and returns the existing project on a repeat
        const r = await call(cfg, "project_declare", /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(project) ? { name: project } : { title: project }, opts);
        slug = r.name;
      } else {
        const list = await call(cfg, "project_list", {}, opts);
        const hit = (list.items || []).find((p) => p.name === project || (p.title || "").toLowerCase() === project.toLowerCase());
        if (!hit) throw new ApiError("project_not_found", `no project "${project}" — pass --create to create it, or see project_resume without arguments`);
        slug = hit.name;
      }
      await bind(cfg, path, slug, opts);
      const opened = await openFor(cfg, terminal, slug, opts);
      out(`Folder ${path} bound to ${slug}.`);
      // the same package as SessionStart: folder notes, sub-agent reminder, generated agents and skills
      out(startPackage(cfg, terminal, path, { project: slug, path: resolve(path), source: "binding" }, opened));
      return 0;
    }
    case "bind": {
      const project = rest.join(" ").trim();
      if (!project) throw new ApiError("usage", "bind <project> [--path <dir>]");
      const b = await bind(cfg, path, project, opts);
      out(`Folder ${path} bound to ${project}${b.pending ? " (offline: saved locally, goes to Litopys with the next sync)" : ""}.`);
      return 0;
    }
    case "unbind": {
      const n = await unbind(cfg, path, opts);
      out(n ? `Folder ${path} unbound.` : `Folder ${path} had no binding (a parent folder may still be bound).`);
      return 0;
    }
    case "new": {
      // a new project: Litopys first; a folder only when asked (a project may live in memory only)
      const title = rest.join(" ").trim();
      if (!title) throw new ApiError("usage", "new <title> [--path <dir>] [--summary <text>]");
      const args = /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(title) ? { name: title } : { title };
      if (flags.summary) args.summary = flags.summary;
      const r = await call(cfg, "project_declare", args, opts);
      out(r.created === false ? `Project ${r.name} exists already.` : `Project ${r.name} created.`);
      if (flags.path) {
        mkdirSync(flags.path, { recursive: true });
        await bind(cfg, flags.path, r.name, opts);
        out(`Folder ${resolve(flags.path)} bound to ${r.name}.`);
      } else {
        out("No folder: the project lives in Litopys only (bind one later with /litopys:start in that folder).");
      }
      return 0;
    }
    case "projects": {
      // the registry: projects of Litopys with the folders bound on this machine
      const [list, bindings] = [await call(cfg, "project_list", {}, opts), await syncBindings(cfg, opts).catch(() => loadBindings(cfg))];
      for (const p of list.items || []) {
        const here = bindings.filter((b) => b.project === p.name).map((b) => b.path);
        out(`${p.name}${p.title && p.title !== p.name ? ` — ${p.title}` : ""}  [${p.status || "active"}]  ${here.length ? here.join(", ") : "(no folder on this machine)"}`);
      }
      return 0;
    }
    case "space": {
      // space <project> <space>: the project becomes a member of the space (layer "projects")
      const [project, space] = rest;
      if (!project || !space) throw new ApiError("usage", "space <project> <space>");
      const list = (await call(cfg, "project_list", {}, opts)).items || [];
      const p = list.find((x) => x.name === project);
      if (!p) throw new ApiError("project_not_found", `no project "${project}"`);
      let s = list.find((x) => x.name === space);
      if (!s) {
        const r = await call(cfg, "project_declare", /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(space) ? { name: space } : { title: space }, opts);
        s = { name: r.name, project_id: r.project_id, labels: [] };
        out(`Space ${r.name} created.`);
      }
      await label(cfg, s, "space", opts);
      await call(cfg, "link_assert", { relation: "member_of",
        from: { entity_type: "project", entity_id: p.project_id }, to: { entity_type: "project", entity_id: s.project_id } }, opts);
      out(`${project} is a member of the space ${s.name}: its rules (instruction / style / agent / skill facts) now reach ${project}.`);
      return 0;
    }
    case "global": {
      // global <project>: the project holds the owner's global layer
      const [project] = rest;
      const p = ((await call(cfg, "project_list", {}, opts)).items || []).find((x) => x.name === project);
      if (!p) throw new ApiError("project_not_found", `no project "${project}"`);
      await label(cfg, p, "global", opts);
      out(`${project} holds the global layer: its rules reach every project (narrower layers win).`);
      return 0;
    }
    case "mode": {
      // mode files|memory [--path <dir>]: where the layer of the bound project lives
      const [mode] = rest;
      if (mode !== "files" && mode !== "memory") throw new ApiError("usage", "mode files|memory [--path <dir>]");
      const r = await setMode(cfg, mode, path, opts);
      out(`${r.project}: layer source = ${mode === "memory" ? "Litopys" : "CLAUDE.md files"} (${r.folder}).`);
      for (const n of r.notes) out(n);
      out("Restart the Claude Code session (or /clear) for Claude Code to reload its CLAUDE.md files.");
      return 0;
    }
    case "sync-claude-md": {
      // the CLAUDE.md of the bound folder becomes the project's instruction rule (files mode: the file wins)
      const bound = projectFor(cfg, path);
      if (!bound) throw new ApiError("not_bound", `${path} is not bound to a project`);
      const r = await syncClaudeMd(cfg, bound.project, bound.path, opts);
      out(`${r.file} (${r.chars} characters) is now the rule instruction/CLAUDE.md of ${bound.project}.`);
      return 0;
    }
    case "materialize": {
      // the agents and skills of the bound project's layers as files (P5); any mode, on demand
      const bound = projectFor(cfg, path);
      if (!bound) throw new ApiError("not_bound", `${path} is not bound to a project`);
      const layers = await call(cfg, "layer_list", { project: bound.project }, opts);
      const m = materialize(bound.path, layers);
      out(`${bound.project}: ${m.agents} agent(s), ${m.skills} skill(s) in the layers; written ${m.written.length}, removed ${m.removed.length}` +
        `${m.gitExcluded ? "; kept out of git (.git/info/exclude)" : ""}.`);
      for (const s of m.skipped) out(`skipped: ${s}`);
      return 0;
    }
    case "doctor":
      return doctor(cfg, terminal, path, flags, opts, out);
    default:
      out("usage: litopys.mjs status | start <project> | bind <project> | unbind | doctor   (see the header of this file)");
      return 2;
  }
}

// label adds a label to a project, keeping the ones it has (project_amend replaces the whole set)
async function label(cfg, p, name, opts) {
  const labels = p.labels || [];
  if (labels.includes(name)) return;
  await call(cfg, "project_amend", { project: p.name, labels: [...labels, name] }, opts);
}

async function doctor(cfg, terminal, path, flags, opts, out) {
  const surface = surfaceOf(flags.surface, opts.env || process.env, path);
  const connector = !cfg.token && surface !== "terminal";
  let bad = 0;
  const ok = (cond, good, fix) => {
    out(`${cond ? "OK  " : "FAIL"} ${cond ? good : fix}`);
    if (!cond) bad++;
    return cond;
  };
  out(`Litopys ${safeUrl(cfg.url) || "(no server address)"}  (surface: ${surface}, data: ${cfg.home})`);
  ok(Number(process.versions.node.split(".")[0]) >= 20, `Node ${process.versions.node}`, `Node ${process.versions.node}: the plugin needs Node 20 or later`);
  if (cfg.conflict) {
    ok(false, "", addressConflictText(cfg));
  } else if (connector) {
    out("INFO connector mode: no LITOPYS_TOKEN, and none is needed here. Check the connector with its system_whoami tool. " +
      "If this is a terminal, run doctor with --surface terminal.");
  } else if ([
    ok(!!cfg.url, "server address is set", "the server address is not set: set LITOPYS_URL or install the plugin from a built marketplace"),
    ok(!!cfg.token, "LITOPYS_TOKEN is set", cfg.token ? "" : noTokenText((opts.userEnvHas || userEnvHas)("LITOPYS_TOKEN"))),
  ].every(Boolean)) {
    const fixed = mcpMismatch(cfg, opts.mcpConfigFile);
    ok(!fixed, "the MCP tools and the hooks use one server", `the MCP tools use ${fixed} but the hooks use ${safeUrl(cfg.url)}: remove LITOPYS_URL or point it at that server`);
    try {
      const w = await call(cfg, "system_whoami", {}, opts);
      ok(w.service === "Litopys", `signed in as ${w.display_name || w.email} (profile ${w.profile})`, `unexpected service: ${w.service}`);
    } catch (e) {
      const why = /unauthenticated|invalid_token|http_401/.test(e.code || "")
        ? " — the token is wrong, expired or revoked: issue a new one (scripts/issue-plugin-token.ps1) and restart Claude Code" : "";
      ok(false, "", `whoami failed — ${e.code}: ${e.message}${e.hint ? ` (${e.hint})` : ""}${why}`);
    }
    try {
      const where = await resolveProject(cfg, path, opts);
      ok(!!where, `folder → project ${where?.project} (${where?.source})`, `folder ${path} is not bound: /litopys:start <project> binds it`);
    } catch (e) {
      ok(false, "", `project lookup failed — ${e.code}: ${e.message}`);
    }
  }
  const m = terminal ? readMarker(cfg, terminal) : null;
  out(m?.session_id ? `OK   this terminal: project ${m.project}, session ${m.session_id}` : "INFO this terminal has no open session");
  out(`INFO bindings: ${loadBindings(cfg).length}`);
  for (const w of desktopChecks({ url: cfg.url })) out(`WARN ${w}`);
  if (flags.advice) for (const line of setupAdvice(surface)) out(line);
  return bad ? 1 : 0;
}

if (process.argv[1] && /litopys\.mjs$/.test(process.argv[1].replace(/\\/g, "/"))) {
  // exitCode, not process.exit: exiting while fetch still closes its sockets trips a libuv assertion on
  // Windows (src\win\async.c:94); the loop ends by itself once they are closed
  main(process.argv.slice(2)).then(
    (code) => (process.exitCode = code),
    (e) => {
      process.stderr.write(`litopys: ${e.code ? e.code + ": " : ""}${e.message}${e.hint ? `\nhint: ${e.hint}` : ""}\n`);
      process.exitCode = 1;
    },
  );
}
