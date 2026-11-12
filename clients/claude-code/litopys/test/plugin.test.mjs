// Tests of the Litopys plugin against a fake /v1 server (node --test). No network, no Claude Code.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createServer } from "node:http";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, beforeEach, test } from "node:test";
import { fileURLToPath } from "node:url";

import { bind, call, config, loadBindings, readMarker, safeUrl, syncBindings, unbind, writeMarker } from "../lib/core.mjs";
import { postToolUse, preToolUse, sessionStart, stop, subagentStart, subagentStop } from "../lib/hooks.mjs";
import { main } from "../cli/litopys.mjs";
import { closeFor } from "../lib/session.mjs";
import { appDataWarning, desktopChecks, mcpMismatch, setupAdvice, surfaceOf } from "../lib/doctor.mjs";
import { buildMarketplace, serverAddress } from "../../build-marketplace.mjs";

// doctor must not read this machine's Claude Desktop or take the test run for a cloud session
delete process.env.APPDATA;
delete process.env.CLAUDE_CODE_REMOTE;

// --- fake server -------------------------------------------------------------------------------------
const calls = [];
let projects = [{ name: "workshop", title: "Workshop" }];
let nextSession = 1;
const sessions = new Map(); // id -> {project, closed}
const facts = new Map(); // project|kind|key -> fact
const links = [];
// the layer rules a project holds itself (instruction / style / agent / skill facts)
const ownRules = (project) => [...facts.values()].filter((f) => f.project === project && ["instruction", "style", "agent", "skill"].includes(f.kind))
  .map((f) => ({ kind: f.kind, key: f.key, value: f.value, layer: "project", source: project }));

const server = createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const args = body ? JSON.parse(body) : {};
    const verb = req.url.replace(/^\/v1\//, "").replace("/", "_");
    calls.push({ verb, args, auth: req.headers.authorization });
    const send = (status, obj) => {
      res.writeHead(status, { "content-type": "application/json" });
      res.end(JSON.stringify(obj));
    };
    if (req.headers.authorization !== "Bearer LTP_test") return send(401, { error: { code: "unauthenticated", message: "bad token" } });
    switch (verb) {
      case "project_list":
        return send(200, { items: projects });
      case "project_declare": {
        const name = args.name || args.title.toLowerCase().replace(/[^a-z0-9]+/g, "-");
        if (!projects.some((p) => p.name === name)) projects.push({ name, title: args.title, project_id: `p-${name}`, labels: [] });
        return send(200, { name, project_id: `p-${name}`, created: true });
      }
      case "session_open": {
        const known = sessions.get(args.attach);
        if (known && !known.closed && known.project === args.project) return send(200, { session: { session_id: args.attach, project: args.project }, attached: true });
        const id = `s-${nextSession++}`;
        sessions.set(id, { project: args.project, closed: false });
        return send(200, { session: { session_id: id, project: args.project }, attached: false, attach_miss: known ? "closed" : undefined });
      }
      case "project_resume": {
        const proj = projects.find((x) => x.name === args.project) || { name: args.project, labels: [] };
        return send(200, {
          bundle: {
            project: { name: args.project, labels: proj.labels || [], status: proj.status || "active" }, current: "where we stopped",
            layers: { global: "my-global", spaces: ["household"], overridden: 1, stack: [
              { kind: "instruction", key: "language", value: "Answer in Ukrainian.", layer: "project", source: args.project },
              { kind: "style", key: "tone", value: "formal", layer: "projects", source: "household" },
              ...ownRules(args.project),
            ] },
          },
          next_steps: ["#1 do the thing"],
          open_outcomes: proj.open_outcomes,
        });
      }
      case "layer_list":
        return send(200, { spaces: [], stack: ownRules(args.project) });
      case "session_close": {
        const s = sessions.get(args.session_id);
        const already = s?.closed;
        if (s) s.closed = true;
        return send(200, { already_closed: !!already, missing_outcomes: args.outcomes ? ["T-4"] : undefined });
      }
      case "system_whoami":
        return send(200, { service: "Litopys", display_name: "Ada", profile: "core" });
      case "fact_record":
        facts.set(`${args.project}|${args.kind}|${args.key}`, { project: args.project, kind: args.kind, key: args.key, value: args.value });
        return send(200, { event_id: "e", seq: 1 });
      case "fact_retire":
        facts.delete(`${args.project}|${args.kind}|${args.key}`);
        return send(200, { event_id: "e", seq: 2 });
      case "project_amend": {
        const p = projects.find((x) => x.name === args.project);
        if (p && args.labels) p.labels = args.labels;
        return send(200, { event_id: "e", seq: 3 });
      }
      case "link_assert":
        links.push(args);
        return send(200, { link_id: "l", event_id: "e", seq: 4 });
      case "fact_list":
        return send(200, { items: [...facts.values()].filter((f) => !args.kind || f.kind === args.kind) });
      default:
        return send(404, { error: { code: "not_found", message: verb } });
    }
  });
});

let cfg;
let home;
// fresh per run: a leftover tree of an earlier run would make "written" counts lie
const base = mkdtempSync(join(tmpdir(), "litopys-plugin-paths-"));
const work = join(base, "workshop");

before(async () => {
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
});
after(() => {
  server.close();
  rmSync(base, { recursive: true, force: true });
});
beforeEach(() => {
  home = mkdtempSync(join(tmpdir(), "litopys-plugin-"));
  cfg = config({ LITOPYS_URL: `http://127.0.0.1:${server.address().port}`, LITOPYS_TOKEN: "LTP_test", LITOPYS_HOME: home });
  calls.length = 0;
  projects = [{ name: "workshop", title: "Workshop", project_id: "p-workshop", labels: ["infra"] }];
  facts.clear();
  links.length = 0;
});

const ctx = (out) => JSON.parse(out).hookSpecificOutput.additionalContext;

// --- outcomes ------------------------------------------------------------------------
test("the start names what earlier sessions left unfinished; close passes outcomes and next steps", async () => {
  projects[0].open_outcomes = [{ ref: "T-1", state: "partial", result: "Report ordered", next: "Pay the invoice" }];
  mkdirSync(work, { recursive: true });
  const start = ctx(await sessionStart({ session_id: "t-out", cwd: work, source: "startup" }, cfg));
  assert.match(start, /Left unfinished by earlier sessions/);
  assert.match(start, /- T-1 \[partial\]: Report ordered → next: Pay the invoice/);
  assert.match(start, /outcomes/);
  const outcomes = [{ ref: "T-1", result: "Invoice paid", state: "done" }];
  const res = await closeFor(cfg, "t-out", "day done", {}, { outcomes, next_steps: ["Call the notary"] });
  const sent = calls.filter((c) => c.verb === "session_close").at(-1).args;
  assert.deepEqual(sent.outcomes, outcomes);
  assert.deepEqual(sent.next_steps, ["Call the notary"]);
  assert.deepEqual(res.missing, ["T-4"]);
  assert.equal(readMarker(cfg, "t-out"), null);
  // without outcomes the call stays as before
  await sessionStart({ session_id: "t-out2", cwd: work, source: "startup" }, cfg);
  await closeFor(cfg, "t-out2", "plain", {});
  const plain = calls.filter((c) => c.verb === "session_close").at(-1).args;
  assert.equal(plain.outcomes, undefined);
  assert.equal(plain.next_steps, undefined);
});

// --- SessionStart ------------------------------------------------------------------------------------
test("no token: connector mode — the model opens the session now; without connector tools memory is off", async () => {
  const none = ctx(await sessionStart({ session_id: "t1", cwd: work, source: "startup" }, { ...cfg, token: "" }, { userEnvHas: () => false }));
  assert.match(none, /connector mode/);
  assert.match(none, /Before you answer the person's first message — whatever it is, a greeting too/);
  assert.match(none, /compare the names with the folder "workshop"/);
  assert.match(none, /no match → project "global" with auto_create true/);
  assert.match(none, /LITOPYS_TOKEN is not set/);
  assert.match(none, /\/litopys:doctor/);
  assert.equal(calls.length, 0, "no token: the hook calls nothing");
  assert.equal(readMarker(cfg, "t1").connector, true);
  const stale = ctx(await sessionStart({ session_id: "t1", cwd: work, source: "startup" }, { ...cfg, token: "" }, { userEnvHas: () => true }));
  assert.match(stale, /in your user environment but not in this process/);
});

test("connector mode: Desktop probe folders open nothing, the Cowork folder goes to global", async () => {
  const nc = { ...cfg, token: "" };
  const probe = join(base, "claude-sandbox-status-1a2b");
  assert.equal(await sessionStart({ session_id: "tp", cwd: probe, source: "startup" }, nc, { userEnvHas: () => false }), "");
  assert.equal(readMarker(cfg, "tp"), null);
  const cowork = ctx(await sessionStart({ session_id: "tc", cwd: join(base, "claude"), source: "startup" }, nc, { userEnvHas: () => false }));
  assert.match(cowork, /project_resume with project "global" and auto_create true/);
  assert.doesNotMatch(cowork, /compare the names/);
});

test("connector mode: the marker follows the connector's project_resume, keeps opened_at, survives resume and close", async () => {
  const nc = { ...cfg, token: "" };
  await sessionStart({ session_id: "tk", cwd: work, source: "startup" }, nc, { userEnvHas: () => false });
  const tool = "mcp__claude_ai_Litopys__project_resume";
  // the catalog opens nothing
  await postToolUse({ session_id: "tk", tool_name: tool, tool_input: {}, tool_response: [{ type: "text", text: JSON.stringify({ catalog: [] }) }] }, nc);
  assert.equal(readMarker(cfg, "tk").session_id, undefined);
  const answer = (id, at) => ({ content: [{ type: "text", text: JSON.stringify({ session: { session_id: id, opened_at: at }, bundle: { project: { name: "workshop" } } }) }] });
  await postToolUse({ session_id: "tk", tool_name: tool, tool_input: { project: "Workshop" }, tool_response: answer("c-1", "2026-11-01T08:00:00Z") }, nc);
  let m = readMarker(cfg, "tk");
  assert.equal(m.project, "workshop");
  assert.equal(m.session_id, "c-1");
  // the same session again (a resumed conversation): opened_at stays
  await postToolUse({ session_id: "tk", tool_name: tool, tool_input: { project: "workshop" }, tool_response: answer("c-1", "2026-11-01T09:30:00Z") }, nc);
  assert.equal(readMarker(cfg, "tk").sessions.workshop.opened_at, "2026-11-01T08:00:00Z");
  // writes count and the Stop reminder works without a token
  await postToolUse({ session_id: "tk", tool_name: "mcp__claude_ai_Litopys__statement_assert", tool_response: {} }, nc);
  m = readMarker(cfg, "tk");
  m.last_checkpoint_at = new Date(Date.now() - 3600_000).toISOString();
  (await import("../lib/core.mjs")).writeMarker(cfg, "tk", m);
  assert.match(JSON.parse(stop({ session_id: "tk" }, nc)).reason, /session_checkpoint with session_id "c-1"/);
  // resume of the conversation: the hook names the kept project
  const resumed = ctx(await sessionStart({ session_id: "tk", cwd: base, source: "resume" }, nc, { userEnvHas: () => false }));
  assert.match(resumed, /continues project workshop: call project_resume with project "workshop"/);
  // close: the session is forgotten, connector mode stays
  await postToolUse({ session_id: "tk", tool_name: "mcp__claude_ai_Litopys__session_close", tool_input: { session_id: "c-1" }, tool_response: {} }, nc);
  m = readMarker(cfg, "tk");
  assert.equal(m.session_id, undefined);
  assert.equal(m.connector, true);
});

test("hooks.json: the MCP matcher catches the plugin's tools and the connector's (Litopys capitalised)", () => {
  const hooks = JSON.parse(readFileSync(new URL("../hooks/hooks.json", import.meta.url), "utf8")).hooks;
  const re = new RegExp(`^(?:${hooks.PostToolUse[0].matcher})$`);
  assert.ok(re.test("mcp__plugin_litopys_litopys__statement_assert"));
  assert.ok(re.test("mcp__claude_ai_Litopys__project_resume"));
  assert.ok(!re.test("mcp__plugin_other-memory_other-memory__memory_save_observation"));
});

test("token mode: the model's own project_resume does not move the terminal's marker", async () => {
  await sessionStart({ session_id: "tt", cwd: work, source: "startup" }, cfg);
  const before = readMarker(cfg, "tt").session_id;
  const answer = { content: [{ type: "text", text: JSON.stringify({ session: { session_id: "other" }, bundle: { project: { name: "x" } } }) }] };
  await postToolUse({ session_id: "tt", tool_name: "mcp__plugin_litopys_litopys__project_resume", tool_input: { project: "x" }, tool_response: answer }, cfg);
  assert.equal(readMarker(cfg, "tt").session_id, before);
});

test("folder name equal to a project slug opens this terminal's session", async () => {
  const out = await sessionStart({ session_id: "t1", cwd: work, source: "startup" }, cfg);
  const text = ctx(out);
  assert.match(text, /Project: workshop \(folder name\)/);
  assert.match(text, /session_id "s-\d+"/);
  assert.match(text, /where we stopped/);
  // P3: the stack is shown as rules with their layer and source, and not repeated in the JSON
  assert.match(text, /- instruction `language` \[project: workshop\]: Answer in Ukrainian\./);
  assert.match(text, /- style `tone` \[projects: household\]: formal/);
  assert.match(text, /1 broader rule\(s\) overridden/);
  assert.doesNotMatch(text.split("## Where the project stands")[1], /"layers"/);
  const m = readMarker(cfg, "t1");
  assert.equal(m.project, "workshop");
  assert.equal(m.sessions.workshop.session_id, m.session_id);
  assert.equal(calls.find((c) => c.verb === "session_open").args.attach, "none");
});

test("resume continues the session of the marker", async () => {
  await sessionStart({ session_id: "t1", cwd: work, source: "startup" }, cfg);
  const sid = readMarker(cfg, "t1").session_id;
  calls.length = 0;
  const out = await sessionStart({ session_id: "t1", cwd: join(base, "elsewhere"), source: "resume" }, cfg);
  assert.match(ctx(out), /\(continued\)/);
  assert.equal(calls.find((c) => c.verb === "session_open").args.attach, sid);
});

test("an unbound folder that matches no project: no session, a hint", async () => {
  const out = await sessionStart({ session_id: "t2", cwd: join(base, "random"), source: "startup" }, cfg);
  assert.match(ctx(out), /not bound to a project/);
  assert.equal(readMarker(cfg, "t2"), null);
});

test("server down: the session starts anyway with one line of context", async () => {
  const out = await sessionStart({ session_id: "t3", cwd: work, source: "startup" }, { ...cfg, url: "http://127.0.0.1:9" });
  assert.match(ctx(out), /could not open the session \(unreachable/);
});

// --- writes, checkpoint reminder, close --------------------------------------------------------------
test("writes are counted, the Stop reminder comes once per interval, a checkpoint resets it", async () => {
  await sessionStart({ session_id: "t4", cwd: work, source: "startup" }, cfg);
  const sid = readMarker(cfg, "t4").session_id;
  assert.equal(stop({ session_id: "t4" }, cfg), "", "no writes, no reminder");
  await postToolUse({ session_id: "t4", tool_name: "mcp__plugin_litopys_litopys__statement_assert" }, cfg);
  await postToolUse({ session_id: "t4", tool_name: "mcp__plugin_litopys_litopys__task_declare" }, cfg);
  await postToolUse({ session_id: "t4", tool_name: "mcp__plugin_litopys_litopys__recall_search" }, cfg);
  assert.equal(readMarker(cfg, "t4").writes_since_checkpoint, 2);
  const now = Date.now();
  assert.equal(stop({ session_id: "t4" }, cfg, now), "", "too early");
  const later = now + 31 * 60000;
  const out = JSON.parse(stop({ session_id: "t4" }, cfg, later));
  assert.equal(out.decision, "block");
  assert.match(out.reason, new RegExp(sid));
  assert.equal(stop({ session_id: "t4", stop_hook_active: true }, cfg, later), "", "never blocks its own continuation");
  assert.equal(stop({ session_id: "t4" }, cfg, later + 60000), "", "once per interval");
  await postToolUse({ session_id: "t4", tool_name: "mcp__plugin_litopys_litopys__session_checkpoint" }, cfg);
  assert.equal(readMarker(cfg, "t4").writes_since_checkpoint, 0);
  assert.equal(stop({ session_id: "t4" }, { ...cfg, checkpointMinutes: 0 }, later + 99 * 60000), "", "0 disables");
});

test("file edits without a memory write bring the reminder too; reads do not count, a write resets them", async () => {
  await sessionStart({ session_id: "t4e", cwd: work, source: "startup" }, cfg);
  const file = { file_path: `${work}/notes.md` };
  await postToolUse({ session_id: "t4e", tool_name: "Read", tool_input: file }, cfg);
  await postToolUse({ session_id: "t4e", tool_name: "Grep", tool_input: { path: work } }, cfg);
  assert.equal(stop({ session_id: "t4e" }, cfg, Date.now() + 31 * 60000), "", "reads are not work to record");
  await postToolUse({ session_id: "t4e", tool_name: "Edit", tool_input: file }, cfg);
  await postToolUse({ session_id: "t4e", tool_name: "Write", tool_input: file }, cfg);
  await postToolUse({ session_id: "t4e", tool_name: "Edit", tool_input: file, tool_response: { isError: true } }, cfg);
  assert.equal(readMarker(cfg, "t4e").edits_since_write, 2, "a failed edit changed nothing");
  const out = JSON.parse(stop({ session_id: "t4e" }, cfg, Date.now() + 31 * 60000));
  assert.equal(out.decision, "block");
  assert.match(out.reason, /2 file edit\(s\) since the last write to memory/);
  assert.doesNotMatch(out.reason, /write\(s\) in project/, "no memory writes to report");
  await postToolUse({ session_id: "t4e", tool_name: "mcp__plugin_litopys_litopys__statement_assert" }, cfg);
  assert.equal(readMarker(cfg, "t4e").edits_since_write, 0, "the work reached memory");
});

test("session_close by the model makes the terminal forget the session; a failed close does not", async () => {
  await sessionStart({ session_id: "t5", cwd: work, source: "startup" }, cfg);
  await postToolUse({ session_id: "t5", tool_name: "mcp__litopys__session_close", tool_response: { isError: true } }, cfg);
  assert.ok(readMarker(cfg, "t5").session_id, "kept after an error");
  await postToolUse({ session_id: "t5", tool_name: "mcp__litopys__session_close", tool_input: { session_id: readMarker(cfg, "t5").session_id } }, cfg);
  const m = readMarker(cfg, "t5");
  assert.equal(m.session_id, undefined);
  assert.deepEqual(m.sessions, {});
});

test("other servers' tools are ignored", async () => {
  assert.equal(await postToolUse({ session_id: "t6", tool_name: "mcp__plugin_other-memory_other-memory__memory_save_observation" }, cfg), "");
});

// --- P2: bindings live in Litopys, the local file is a cache ---------------------------------------
test("a binding goes to Litopys; another cache (a reinstall) finds it again by syncing", async () => {
  const lines = [];
  await main(["bind", "workshop", "--path", join(base, "hl")], cfg, {}, (s) => lines.push(s));
  const f = [...facts.values()][0];
  assert.equal(f.kind, "binding");
  assert.equal(JSON.parse(f.value).path, join(base, "hl"));
  // a fresh data dir = the same machine after a reinstall: nothing local, the server knows
  const fresh = config({ LITOPYS_URL: cfg.url, LITOPYS_TOKEN: "LTP_test", LITOPYS_HOME: mkdtempSync(join(tmpdir(), "litopys-fresh-")) });
  const out = await sessionStart({ session_id: "t9", cwd: join(base, "hl", "deep"), source: "startup" }, fresh);
  assert.match(ctx(out), /Project: workshop \(bound folder\)/);
  await main(["unbind", "--path", join(base, "hl")], cfg, {}, (s) => lines.push(s));
  assert.equal(facts.size, 0, "unbind retires the fact");
});

test("offline bind stays local as pending and goes up with the next sync", async () => {
  const fetchImpl = async () => { throw new TypeError("fetch failed"); };
  const lines = [];
  await main(["bind", "workshop", "--path", join(base, "off")], cfg, { fetchImpl }, (s) => lines.push(s));
  assert.match(lines.join("\n"), /offline/);
  assert.equal(facts.size, 0);
  await sessionStart({ session_id: "t10", cwd: join(base, "off"), source: "startup" }, cfg);
  assert.equal(facts.size, 1, "the pending binding was pushed by the sync");
});

test("new: a project in memory only, or with a created and bound folder; projects lists the registry", async () => {
  const lines = [];
  const out = (s) => lines.push(s);
  assert.equal(await main(["new", "Тільки пам'ять"], cfg, {}, out), 0);
  assert.match(lines.join("\n"), /lives in Litopys only/);
  assert.equal(facts.size, 0);
  lines.length = 0;
  assert.equal(await main(["new", "garden", "--path", join(base, "garden")], cfg, {}, out), 0);
  assert.match(lines.join("\n"), /bound to garden/);
  lines.length = 0;
  assert.equal(await main(["projects"], cfg, {}, out), 0);
  assert.ok(lines.some((l) => l.startsWith("garden") && l.includes(join(base, "garden"))), lines.join("\n"));
  assert.ok(lines.some((l) => l.startsWith("workshop") && l.includes("no folder")), lines.join("\n"));
});

// --- P3: spaces and the global layer -------------------------------------------------------------------
test("space creates the space, labels it and links member_of; global labels without losing other labels", async () => {
  const lines = [];
  const out = (s) => lines.push(s);
  projects.push({ name: "my-global", project_id: "p-g", labels: [] });
  assert.equal(await main(["space", "workshop", "household"], cfg, {}, out), 0);
  const space = projects.find((p) => p.name === "household");
  assert.deepEqual(space.labels, ["space"]);
  assert.equal(links[0].relation, "member_of");
  assert.equal(links[0].from.entity_id, "p-workshop");
  assert.equal(await main(["global", "workshop"], cfg, {}, out), 0);
  assert.deepEqual(projects.find((p) => p.name === "workshop").labels, ["infra", "global"], "existing labels are kept");
  await assert.rejects(main(["global", "nope"], cfg, {}, out), /no project/);
});

// --- P4: touching another project, sub-agents, files | memory ------------------------------------------
test("touching a file of another bound project brings its rules once; sub-agents get the session's rules", async () => {
  const garden = join(base, "garden4");
  projects.push({ name: "garden", project_id: "p-garden", labels: [] });
  await main(["bind", "garden", "--path", garden], cfg, {}, () => {});
  facts.set("garden|instruction|water", { project: "garden", kind: "instruction", key: "water", value: "Water at dawn." });
  await sessionStart({ session_id: "t11", cwd: work, source: "startup" }, cfg);
  // before the touch the model knows nothing of garden; reading its file brings the rule
  const first = await postToolUse({ session_id: "t11", tool_name: "Read", tool_input: { file_path: join(garden, "plan.md") } }, cfg);
  assert.match(ctx(first), /Rules of project garden/);
  assert.match(ctx(first), /Water at dawn\./);
  assert.equal(JSON.parse(first).hookSpecificOutput.hookEventName, "PostToolUse");
  assert.equal(await postToolUse({ session_id: "t11", tool_name: "Edit", tool_input: { file_path: join(garden, "x.md") } }, cfg), "", "once per project");
  assert.equal(await postToolUse({ session_id: "t11", tool_name: "Read", tool_input: { file_path: join(work, "a.md") } }, cfg), "", "own project: nothing");
  // after /compact the touched projects start over
  await sessionStart({ session_id: "t11", cwd: work, source: "compact" }, cfg);
  assert.match(ctx(await postToolUse({ session_id: "t11", tool_name: "Grep", tool_input: { path: garden } }, cfg)), /Water at dawn/);
  // a sub-agent gets the rules of the session's project
  const sub = JSON.parse(subagentStart({ session_id: "t11" }, cfg));
  assert.equal(sub.hookSpecificOutput.hookEventName, "SubagentStart");
  assert.match(sub.hookSpecificOutput.additionalContext, /Answer in Ukrainian/);
});

test("a sub-agent counts its own touches: the main agent's touch does not hide the rules from it", async () => {
  const garden = join(base, "garden8");
  projects.push({ name: "garden", project_id: "p-garden", labels: [] });
  await main(["bind", "garden", "--path", garden], cfg, {}, () => {});
  facts.set("garden|instruction|water", { project: "garden", kind: "instruction", key: "water", value: "Water at dawn." });
  await sessionStart({ session_id: "t14", cwd: work, source: "startup" }, cfg);
  const read = (agent_id) => postToolUse({ session_id: "t14", agent_id, tool_name: "Read", tool_input: { file_path: join(garden, "plan.md") } }, cfg);
  assert.match(ctx(await read()), /Water at dawn/, "main agent");
  assert.match(ctx(await read("a1")), /Water at dawn/, "sub-agent a1 has its own context");
  assert.equal(await read("a1"), "", "once per sub-agent");
  assert.equal(await read(), "", "the main agent is not reset by sub-agents");
  assert.match(ctx(await read("a2")), /Water at dawn/, "another sub-agent");
  // SubagentStop forgets the finished sub-agent's touches; the others stay
  assert.equal(subagentStop({ session_id: "t14", agent_id: "a1" }, cfg), "");
  assert.deepEqual(Object.keys(readMarker(cfg, "t14").touched_by), ["a2"]);
  assert.deepEqual(readMarker(cfg, "t14").touched, ["garden"]);
});

test("files mode materializes agents and skills too and names them instead of repeating; Explore hint with CLAUDE.md", async () => {
  const dir = join(base, "files7");
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "CLAUDE.md"), "# Rules\n");
  await main(["bind", "workshop", "--path", dir], cfg, {}, () => {});
  facts.set("workshop|agent|planner", { project: "workshop", kind: "agent", key: "planner", value: "Plans the work in small steps." });
  const text = ctx(await sessionStart({ session_id: "t15", cwd: dir, source: "startup" }, cfg));
  assert.ok(existsSync(join(dir, ".claude", "agents", "litopys-planner.md")), "written in files mode");
  assert.match(text, /1 written, 0 removed/);
  // a file written now is not loaded by Claude Code yet: its rule comes as text, to sub-agents too
  assert.match(text, /loaded by Claude Code from the next session start/);
  const fresh = text.split("## Where the project stands")[0];
  assert.match(fresh, /- agent `planner` \[project: workshop\]: Plans the work in small steps\./);
  assert.doesNotMatch(fresh, /loaded by Claude Code from \.claude/);
  assert.match(ctx(subagentStart({ session_id: "t15" }, cfg)), /Plans the work in small steps/);
  // the next start finds the file in place: it is named under the name Claude Code registered
  const again = ctx(await sessionStart({ session_id: "t15", cwd: dir, source: "startup" }, cfg));
  assert.doesNotMatch(again, /written/);
  const head = again.split("## Where the project stands")[0];
  assert.match(head, /loaded by Claude Code from \.claude \(generated litopys-\* files\): agent `planner`/);
  assert.doesNotMatch(head, /Plans the work in small steps/, "not repeated as text");
  assert.match(head, /Answer in Ukrainian/, "other rules stay as text");
  assert.match(head, /Sub-agents such as Explore start without the CLAUDE\.md files/);
  const sub = ctx(subagentStart({ session_id: "t15" }, cfg));
  assert.match(sub, /agent `planner`/);
  assert.doesNotMatch(sub, /Plans the work in small steps/);
  // the rule leaves the layer -> the next start removes the generated file in files mode too
  facts.delete("workshop|agent|planner");
  assert.match(ctx(await sessionStart({ session_id: "t15", cwd: dir, source: "startup" }, cfg)), /0 written, 1 removed/);
  assert.ok(!existsSync(join(dir, ".claude", "agents", "litopys-planner.md")));
});

test("an own frontmatter is kept with unsafe plain values quoted; the agent is named by its frontmatter name", async () => {
  const dir = join(base, "fm130");
  mkdirSync(dir, { recursive: true });
  await main(["bind", "workshop", "--path", dir], cfg, {}, () => {});
  facts.set("workshop|agent|planner", { project: "workshop", kind: "agent", key: "planner",
    value: "---\nname: step-planner\ndescription: Test agent of the plan: small steps\ntools: Read\n---\nPlan." });
  facts.set("workshop|skill|deploy", { project: "workshop", kind: "skill", key: "deploy", value: "---\nname: deploy\ndescription: \"Quoted # stays\"\n---\nGo." });
  await sessionStart({ session_id: "t17", cwd: dir, source: "startup" }, cfg);
  const agent = readFileSync(join(dir, ".claude", "agents", "litopys-planner.md"), "utf8");
  assert.ok(agent.startsWith("---\nname: step-planner\ndescription: \"Test agent of the plan: small steps\"\ntools: Read\n---\n"), agent);
  assert.match(readFileSync(join(dir, ".claude", "skills", "litopys-deploy", "SKILL.md"), "utf8"), /^---\nname: deploy\ndescription: "Quoted # stays"\n---\n/);
  const head = ctx(await sessionStart({ session_id: "t17", cwd: dir, source: "startup" }, cfg)).split("## Where the project stands")[0];
  assert.match(head, /agent `step-planner`, skill `litopys-deploy`/);
});

test("an archived project: not guessed from a folder name; a bound folder opens it with a warning", async () => {
  projects.find((p) => p.name === "workshop").status = "archived";
  assert.match(ctx(await sessionStart({ session_id: "t18", cwd: work, source: "startup" }, cfg)), /not bound to a project/);
  assert.equal(readMarker(cfg, "t18"), null);
  const dir = join(base, "arch130");
  await main(["bind", "workshop", "--path", dir], cfg, {}, () => {});
  const text = ctx(await sessionStart({ session_id: "t18", cwd: dir, source: "startup" }, cfg));
  assert.match(text, /Project: workshop \(bound folder\)/);
  assert.match(text.split("\n")[1], /ATTENTION: project workshop is ARCHIVED/);
});

test("a folder found by its name only (no binding) gets no generated files", async () => {
  facts.set("workshop|agent|planner", { project: "workshop", kind: "agent", key: "planner", value: "Plans." });
  const text = ctx(await sessionStart({ session_id: "t16", cwd: work, source: "startup" }, cfg));
  assert.match(text, /Project: workshop \(folder name\)/);
  assert.ok(!existsSync(join(work, ".claude")));
  assert.match(text, /- agent `planner` \[project: workshop\]: Plans\./, "the rule stays as text");
});

test("files mode shows a CLAUDE.md that differs from Litopys; memory mode takes it in, switches the file off; back to files restores it", async () => {
  const dir = join(base, "modes");
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "CLAUDE.md"), "# Rules\nUse PowerShell.\n");
  await main(["bind", "workshop", "--path", dir], cfg, {}, () => {});
  let text = ctx(await sessionStart({ session_id: "t12", cwd: dir, source: "startup" }, cfg));
  assert.match(text, /not in Litopys yet/);
  const lines = [];
  assert.equal(await main(["mode", "memory", "--path", dir], cfg, {}, (s) => lines.push(s)), 0);
  assert.equal(facts.get("workshop|instruction|CLAUDE.md").value, "# Rules\nUse PowerShell.\n");
  assert.ok(projects.find((p) => p.name === "workshop").labels.includes("layer-memory"));
  const settings = JSON.parse(readFileSync(join(dir, ".claude", "settings.local.json"), "utf8"));
  assert.ok(settings.claudeMdExcludes[0].endsWith("modes/CLAUDE.md"));
  text = ctx(await sessionStart({ session_id: "t12", cwd: dir, source: "startup" }, cfg));
  assert.match(text, /memory mode/);
  assert.match(text, /Use PowerShell\./, "in memory mode the rule comes with the start");
  // back to files without the file: restored from Litopys, nothing lost
  rmSync(join(dir, "CLAUDE.md"));
  assert.equal(await main(["mode", "files", "--path", dir], cfg, {}, (s) => lines.push(s)), 0);
  assert.equal(readFileSync(join(dir, "CLAUDE.md"), "utf8"), "# Rules\nUse PowerShell.\n");
  assert.ok(!projects.find((p) => p.name === "workshop").labels.includes("layer-memory"));
  assert.equal(JSON.parse(readFileSync(join(dir, ".claude", "settings.local.json"), "utf8")).claudeMdExcludes, undefined);
  // files mode again: the file and the copy agree — no note, and the copy is not repeated in the context
  text = ctx(await sessionStart({ session_id: "t12", cwd: dir, source: "startup" }, cfg));
  assert.doesNotMatch(text, /differs|not in Litopys/);
  assert.doesNotMatch(text.split("## Where the project stands")[0], /Use PowerShell/);
  writeFileSync(join(dir, "CLAUDE.md"), "# Rules\nUse bash.\n");
  assert.match(ctx(await sessionStart({ session_id: "t12", cwd: dir, source: "startup" }, cfg)), /differs from its copy/);
});

// --- P5: agents and skills of the layers as files ---------------------------------------------------------
test("memory mode materializes agents and skills, keeps them out of git, never touches foreign files, removes stale ones", async () => {
  const dir = join(base, "agents5");
  mkdirSync(join(dir, ".git"), { recursive: true });
  mkdirSync(join(dir, ".claude", "agents"), { recursive: true });
  writeFileSync(join(dir, ".claude", "agents", "litopys-reviewer.md"), "my own file, not generated");
  await main(["bind", "workshop", "--path", dir], cfg, {}, () => {});
  projects.find((p) => p.name === "workshop").labels = ["layer-memory"];
  facts.set("workshop|agent|planner", { project: "workshop", kind: "agent", key: "planner", value: "Plans the work in small steps." });
  facts.set("workshop|agent|reviewer", { project: "workshop", kind: "agent", key: "reviewer", value: "---\nname: reviewer\ndescription: x\n---\nReview." });
  facts.set("workshop|skill|deploy", { project: "workshop", kind: "skill", key: "deploy", value: "# Deploy\nSteps." });
  facts.set("workshop|skill|bad key", { project: "workshop", kind: "skill", key: "bad key", value: "x" });
  const text = ctx(await sessionStart({ session_id: "t13", cwd: dir, source: "startup" }, cfg));
  assert.match(text, /Agents and skills from Litopys: 2 written/);
  assert.match(text, /Skipped: .*skill\/bad key/);
  assert.match(text, /litopys-reviewer\.md \(a file not generated by Litopys/);
  const planner = readFileSync(join(dir, ".claude", "agents", "litopys-planner.md"), "utf8");
  assert.ok(planner.startsWith("---\nname: planner\ndescription: \"Plans the work in small steps.\"\n---\n"), planner);
  assert.match(planner, /generated by Litopys/);
  assert.match(readFileSync(join(dir, ".claude", "skills", "litopys-deploy", "SKILL.md"), "utf8"), /# Deploy/);
  assert.equal(readFileSync(join(dir, ".claude", "agents", "litopys-reviewer.md"), "utf8"), "my own file, not generated");
  assert.match(readFileSync(join(dir, ".git", "info", "exclude"), "utf8"), /\.claude\/agents\/litopys-\*\.md/);
  // the rule leaves the layer -> the generated file goes; unchanged files are not rewritten
  facts.delete("workshop|skill|deploy");
  const lines = [];
  assert.equal(await main(["materialize", "--path", dir], cfg, {}, (s) => lines.push(s)), 0);
  assert.match(lines[0], /written 0, removed 1/);
  assert.ok(!existsSync(join(dir, ".claude", "skills", "litopys-deploy")));
  assert.ok(existsSync(join(dir, ".claude", "agents", "litopys-planner.md")));
});

// --- CLI ---------------------------------------------------------------------------------------------
test("CLI start binds the folder, opens the session and prints the package; status and doctor", async () => {
  const lines = [];
  const out = (s) => lines.push(s);
  await assert.rejects(main(["start", "Nope", "--terminal", "t7", "--path", join(base, "x")], cfg, {}, out), /project_not_found|no project/);
  assert.equal(await main(["start", "Квартира Сад", "--create", "--terminal", "t7", "--path", join(base, "x")], cfg, {}, out), 0);
  assert.ok(calls.some((c) => c.verb === "project_declare" && c.args.title === "Квартира Сад"));
  assert.ok(lines.some((l) => /bound to/.test(l)));
  lines.length = 0;
  // CLI start gives the SessionStart package: generated files, folder notes, the sub-agent reminder
  const cli = join(base, "cli130");
  mkdirSync(cli, { recursive: true });
  writeFileSync(join(cli, "CLAUDE.md"), "# Rules\n");
  facts.set("workshop|skill|deploy", { project: "workshop", kind: "skill", key: "deploy", value: "# Deploy\nSteps." });
  assert.equal(await main(["start", "workshop", "--terminal", "t19", "--path", cli], cfg, {}, out), 0);
  const started = lines.join("\n");
  assert.ok(existsSync(join(cli, ".claude", "skills", "litopys-deploy", "SKILL.md")));
  assert.match(started, /1 written, 0 removed/);
  assert.match(started, /not in Litopys yet/);
  assert.match(started, /Sub-agents such as Explore start without the CLAUDE\.md files/);
  facts.delete("workshop|skill|deploy");
  lines.length = 0;
  // the bound folder (and its subfolders) now resolve without the folder-name rule
  const hook = await sessionStart({ session_id: "t8", cwd: join(base, "x", "sub"), source: "startup" }, cfg);
  assert.match(ctx(hook), /\(bound folder\)/);
  assert.equal(await main(["status", "--terminal", "t7", "--path", join(base, "x")], cfg, {}, out), 0);
  assert.match(lines.join("\n"), /session s-\d+/);
  lines.length = 0;
  assert.equal(await main(["doctor", "--terminal", "t7", "--path", join(base, "x")], cfg, {}, out), 0, lines.join("\n"));
  assert.match(lines.join("\n"), /signed in as Ada/);
  assert.equal(await main(["unbind", "--path", join(base, "x")], cfg, {}, out), 0);
  lines.length = 0;
  assert.equal(await main(["doctor", "--path", join(base, "x")], { ...cfg, token: "" }, { userEnvHas: () => false }, out), 1);
  assert.match(lines.join("\n"), /FAIL LITOPYS_TOKEN is not set/);
  lines.length = 0;
  assert.equal(await main(["doctor", "--path", join(base, "x")], { ...cfg, token: "" }, { userEnvHas: () => true }, out), 1);
  assert.match(lines.join("\n"), /FAIL LITOPYS_TOKEN is in your user environment but not in this process/);
  lines.length = 0;
  assert.equal(await main(["doctor", "--path", join(base, "x")], { ...cfg, token: "LTP_wrong" }, {}, out), 1);
  assert.match(lines.join("\n"), /whoami failed — unauthenticated: .*wrong, expired or revoked/);
  lines.length = 0;
  // no token in a cloud session or Cowork is the connector mode, not a failure (K9)
  assert.equal(await main(["doctor", "--surface", "cloud", "--advice", "--path", join(base, "x")], { ...cfg, token: "" }, {}, out), 0, lines.join("\n"));
  assert.match(lines.join("\n"), /INFO connector mode/);
  assert.match(lines.join("\n"), /Needs approval for deletion_\*/);
  assert.doesNotMatch(lines.join("\n"), /FAIL/);
  lines.length = 0;
  // an installation without an address says so instead of calling nowhere (K7)
  assert.equal(await main(["doctor", "--path", join(base, "x")], { ...cfg, url: "" }, {}, out), 1);
  assert.match(lines.join("\n"), /FAIL the server address is not set/);
  lines.length = 0;
  // both problems at once are both reported
  assert.equal(await main(["doctor", "--path", join(base, "x")], { ...cfg, url: "", token: "" }, { userEnvHas: () => false }, out), 1);
  assert.match(lines.join("\n"), /FAIL the server address is not set[\s\S]*FAIL LITOPYS_TOKEN is not set/);
  const noServer = await sessionStart({ session_id: "t-no-server", cwd: join(base, "кирилиця"), source: "startup" }, { ...cfg, url: "" });
  assert.match(ctx(noServer), /address is not set.*memory is off/);
  assert.doesNotMatch(ctx(noServer), /still work|not bound/);
  rmSync(home, { recursive: true, force: true });
});

// --- marketplace of an installation --------------------------------------------------
test("the source plugin holds no address; config reads LITOPYS_URL, then install.json", () => {
  const source = readFileSync(new URL("../.mcp.json", import.meta.url), "utf8");
  assert.doesNotMatch(source, /https?:/);
  const dir = mkdtempSync(join(tmpdir(), "litopys-install-"));
  const file = join(dir, "install.json");
  assert.equal(config({}, file).url, "");
  writeFileSync(file, JSON.stringify({ url: "https://memory.example.org/" }));
  assert.equal(config({}, file).url, "https://memory.example.org");
  assert.equal(config({ LITOPYS_URL: "https://other.example.org" }, file).url, "https://other.example.org");
  rmSync(dir, { recursive: true, force: true });
});

test("buildMarketplace stamps the address and leaves out the tests", () => {
  const dir = mkdtempSync(join(tmpdir(), "litopys-market-"));
  const out = join(dir, "market");
  const r = buildMarketplace({ url: "https://memory.example.org/", out });
  assert.equal(r.address, "https://memory.example.org");
  const mcp = JSON.parse(readFileSync(join(out, "litopys", ".mcp.json"), "utf8")).mcpServers.litopys;
  assert.equal(mcp.url, "https://memory.example.org/v1/mcp");
  assert.equal(mcp.headers.Authorization, "Bearer ${LITOPYS_TOKEN}");
  assert.equal(config({}, join(out, "litopys", "install.json")).url, "https://memory.example.org");
  assert.ok(existsSync(join(out, ".claude-plugin", "marketplace.json")));
  assert.ok(existsSync(join(out, "litopys", "lib", "core.mjs")));
  assert.ok(!existsSync(join(out, "litopys", "test")));
  assert.throws(() => buildMarketplace({ url: "https://memory.example.org", out }), /not empty/);
  const inside = fileURLToPath(new URL("../../tmp-market-out", import.meta.url));
  assert.throws(() => buildMarketplace({ url: "https://memory.example.org", out: inside }), /outside/);
  assert.ok(!existsSync(inside));
  rmSync(dir, { recursive: true, force: true });
});

test("serverAddress takes only a clean https address", () => {
  assert.equal(serverAddress("https://a.example.org/base/"), "https://a.example.org/base");
  assert.throws(() => serverAddress("http://a.example.org"), /https/);
  assert.throws(() => serverAddress("https://u:p@a.example.org"), /credentials/);
  assert.throws(() => serverAddress("https://a.example.org/?x=1"), /credentials/);
  assert.throws(() => serverAddress("https://a.example.org/?"), /credentials/);
  assert.throws(() => serverAddress("https://a.example.org/#"), /credentials/);
  assert.throws(() => serverAddress("https://a.example.org/mcp"), /base address/);
  assert.throws(() => serverAddress("https://a.example.org/v1/mcp/"), /base address/);
  assert.throws(() => serverAddress("https://a.example.org/MCP"), /base address/);
  assert.throws(() => serverAddress("https://a.example.org/v1"), /base address/);
  assert.throws(() => serverAddress("nope"), /not an address/);
  assert.equal(safeUrl("https://u:p@a.example.org/"), "https://a.example.org");
  assert.equal(safeUrl("nope"), "(not a valid address)");
  assert.equal(safeUrl(""), "");
});

test("buildMarketplace: the version follows the address, so a rebuild for a new address updates clients", () => {
  const dir = mkdtempSync(join(tmpdir(), "litopys-market-"));
  const build = (name, url) => buildMarketplace({ url, out: join(dir, name) }).version;
  const source = JSON.parse(readFileSync(new URL("../.claude-plugin/plugin.json", import.meta.url), "utf8")).version;
  const a = build("a", "https://a.example.org");
  assert.match(a, new RegExp(`^${source.replace(/\./g, "\\.")}\\+[0-9a-f]{8}$`));
  assert.equal(JSON.parse(readFileSync(join(dir, "a", "litopys", ".claude-plugin", "plugin.json"), "utf8")).version, a);
  assert.equal(build("a2", "https://a.example.org/"), a);
  assert.notEqual(build("b", "https://b.example.org"), a);
  rmSync(dir, { recursive: true, force: true });
});

test("buildMarketplace: a junction into the source does not hide that the output is inside it", () => {
  const dir = mkdtempSync(join(tmpdir(), "litopys-market-"));
  const source = join(dir, "source");
  mkdirSync(join(source, "real"), { recursive: true });
  symlinkSync(join(source, "real"), join(dir, "link"), "junction");
  assert.throws(() => buildMarketplace({ url: "https://a.example.org", out: join(dir, "link", "out"), source }), /outside/);
  assert.ok(!existsSync(join(source, "real", "out")));
  rmSync(dir, { recursive: true, force: true });
});

// --- one server for the hooks and the MCP tools (a built plugin + LITOPYS_URL elsewhere) --------
test("a built plugin with LITOPYS_URL elsewhere refuses everything: no request, hooks off, MCP tools denied", async () => {
  const dir = mkdtempSync(join(tmpdir(), "litopys-conflict-"));
  const file = join(dir, "install.json");
  writeFileSync(file, JSON.stringify({ url: "https://prod.example.org" }));
  assert.equal(config({}, file).conflict, "");
  assert.equal(config({ LITOPYS_URL: "https://prod.example.org/" }, file).conflict, "");
  const cfg = { ...config({ LITOPYS_URL: "https://u:p@stage.example.org", LITOPYS_TOKEN: "t", LITOPYS_HOME: dir }, file) };
  assert.equal(cfg.conflict, "https://prod.example.org");

  let requests = 0;
  const fetchImpl = async () => { requests++; throw new Error("must not be called"); };
  await assert.rejects(call(cfg, "system_whoami", {}, { fetchImpl }), (e) => e.code === "address_conflict" && !e.message.includes("u:p"));
  const start = JSON.parse(await sessionStart({ session_id: "s", cwd: dir, source: "startup" }, cfg, { fetchImpl }));
  assert.match(start.hookSpecificOutput.additionalContext, /Project memory is off/);
  assert.equal(requests, 0);

  const denied = JSON.parse(preToolUse({ tool_name: "mcp__plugin_litopys_litopys__statement_record" }, cfg)).hookSpecificOutput;
  assert.equal(denied.permissionDecision, "deny");
  assert.match(denied.permissionDecisionReason, /prod\.example\.org/);
  assert.equal(preToolUse({}, config({}, file)), "");
  assert.equal(preToolUse({}, config({ LITOPYS_URL: "https://stage.example.org" }, join(dir, "none.json"))), "");

  const lines = [];
  assert.equal(await main(["doctor", "--surface", "terminal", "--path", dir], cfg, { fetchImpl, userEnvHas: () => false }, (s) => lines.push(s)), 1);
  assert.equal(lines.filter((l) => l.startsWith("FAIL")).length, 1);
  assert.match(lines.join("\n"), /FAIL LITOPYS_URL/);
  assert.equal(requests, 0);
  rmSync(dir, { recursive: true, force: true });
});

test("the conflict without a token: no connector mode, no pending binding for the built server, no reminder", async () => {
  const dir = mkdtempSync(join(tmpdir(), "litopys-conflict-"));
  const file = join(dir, "install.json");
  writeFileSync(file, JSON.stringify({ url: "https://prod.example.org" }));
  const cfg = config({ LITOPYS_URL: "https://stage.example.org", LITOPYS_HOME: dir }, file);
  assert.equal(cfg.token, "");

  const start = JSON.parse(await sessionStart({ session_id: "s", cwd: dir, source: "startup" }, cfg, { userEnvHas: () => false }));
  assert.match(start.hookSpecificOutput.additionalContext, /Project memory is off/);
  assert.doesNotMatch(start.hookSpecificOutput.additionalContext, /project_resume/);

  await assert.rejects(bind(cfg, dir, "stage-test-project"), (e) => e.code === "address_conflict");
  assert.equal(loadBindings(cfg).length, 0);

  writeMarker(cfg, "s", { project: "p", session_id: "x", writes_since_checkpoint: 3 });
  assert.equal(stop({ session_id: "s" }, cfg), "");

  for (const surface of ["terminal", "cloud"]) {
    const lines = [];
    assert.equal(await main(["doctor", "--surface", surface, "--path", dir], cfg, { userEnvHas: () => false }, (s) => lines.push(s)), 1);
    assert.match(lines.join("\n"), /FAIL LITOPYS_URL/);
  }
  rmSync(dir, { recursive: true, force: true });
});

test("a pending binding goes up only to its own server; another build neither sends nor loses it", async () => {
  const dir = mkdtempSync(join(tmpdir(), "litopys-pending-"));
  const stage = { url: "https://stage.example.org", token: "t", home: dir };
  const prod = { url: "https://prod.example.org", token: "t", home: dir };
  const posted = [];
  const fetchImpl = (origin) => async (url, init) => {
    if (!url.startsWith(origin)) throw new Error(`wrong server: ${url}`);
    posted.push({ url, body: JSON.parse(init.body) });
    return new Response(JSON.stringify({ items: [] }), { status: 200, headers: { "content-type": "application/json" } });
  };
  const offline = async () => { throw new TypeError("fetch failed"); };

  const entry = await bind(stage, join(dir, "work"), "stage-test-project", { fetchImpl: offline });
  assert.equal(entry.pending, true);
  assert.equal(entry.server, "https://stage.example.org");
  const withCreds = await bind({ ...stage, url: "https://u:secret@stage.example.org" }, join(dir, "creds"), "p", { fetchImpl: offline });
  assert.equal(withCreds.server, "https://stage.example.org");
  assert.doesNotMatch(readFileSync(join(dir, "bindings.json"), "utf8"), /secret/);
  await unbind(stage, join(dir, "creds"));

  await syncBindings(prod, { fetchImpl: fetchImpl("https://prod.example.org") });
  assert.ok(!posted.some((p) => p.url.includes("/fact/record")), "nothing recorded on prod");
  assert.deepEqual(loadBindings(prod), []);
  assert.equal(loadBindings(stage).length, 1, "stage's binding survived prod's sync");

  await syncBindings(stage, { fetchImpl: fetchImpl("https://stage.example.org") });
  const recorded = posted.filter((p) => p.url.includes("/fact/record"));
  assert.equal(recorded.length, 1);
  assert.ok(recorded[0].url.startsWith("https://stage.example.org"));
  assert.equal(recorded[0].body.project, "stage-test-project");

  // a pending binding of an older plugin says nothing about its server: it stays local
  writeFileSync(join(dir, "bindings.json"), JSON.stringify({ bindings: [{ path: "/w", project: "old", key: "k", pending: true }] }));
  posted.length = 0;
  await syncBindings(prod, { fetchImpl: fetchImpl("https://prod.example.org") });
  assert.ok(!posted.some((p) => p.url.includes("/fact/record")));
  rmSync(dir, { recursive: true, force: true });
});

test("the same address written differently is not a conflict", () => {
  const dir = mkdtempSync(join(tmpdir(), "litopys-conflict-"));
  const file = join(dir, "install.json");
  writeFileSync(file, JSON.stringify({ url: "https://prod.example.org" }));
  for (const same of ["https://PROD.example.org", "https://prod.example.org:443/", "https://prod.example.org///"]) {
    assert.equal(config({ LITOPYS_URL: same }, file).conflict, "", same);
  }
  assert.equal(config({ LITOPYS_URL: "not an address" }, file).conflict, "https://prod.example.org");
  rmSync(dir, { recursive: true, force: true });
});

test("hooks.json and the hook process: PreToolUse covers the plugin's and the connector's tools, and denies only on a conflict", () => {
  const hooks = JSON.parse(readFileSync(new URL("../hooks/hooks.json", import.meta.url), "utf8")).hooks;
  const matcher = new RegExp(`^(${hooks.PreToolUse[0].matcher})$`);
  assert.ok(matcher.test("mcp__plugin_litopys_litopys__recall_search"));
  assert.ok(matcher.test("mcp__claude_ai_Litopys__recall_search"));
  assert.ok(!matcher.test("Write"));

  // the real process of a real build, so a broken hook file cannot pass as fail-open
  const dir = mkdtempSync(join(tmpdir(), "litopys-prehook-"));
  buildMarketplace({ url: "https://prod.example.org", out: join(dir, "m") });
  const run = (extra) => {
    const env = { ...process.env, ...extra };
    if (!extra.LITOPYS_URL) delete env.LITOPYS_URL;
    const r = spawnSync(process.execPath, [join(dir, "m", "litopys", "hooks", "pre-tool-use.mjs")], { input: "{}", encoding: "utf8", env });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout;
  };
  assert.equal(run({}), "");
  assert.equal(JSON.parse(run({ LITOPYS_URL: "https://stage.example.org" })).hookSpecificOutput.permissionDecision, "deny");
  rmSync(dir, { recursive: true, force: true });
});

// --- doctor: surfaces and Desktop -----------------------------------------------
test("surfaceOf: the flag, the cloud environment, the Cowork folder, otherwise the terminal", () => {
  assert.equal(surfaceOf("cowork", {}, "/x"), "cowork");
  assert.throws(() => surfaceOf("phone", {}, "/x"), /--surface/);
  assert.equal(surfaceOf(undefined, { CLAUDE_CODE_REMOTE: "true" }, "/x"), "cloud");
  assert.equal(surfaceOf(undefined, {}, join("/sessions", "claude")), "cowork");
  assert.equal(surfaceOf(undefined, {}, "/work/workshop"), "terminal");
  assert.match(setupAdvice("terminal").join("\n"), /LITOPYS_TOKEN/);
  assert.match(setupAdvice("cowork").join("\n"), /Connect, then sign in/);
});

test("mcpMismatch compares a fixed MCP address with the hooks' server; a template has nothing to compare", () => {
  const dir = mkdtempSync(join(tmpdir(), "litopys-mcp-"));
  const file = join(dir, ".mcp.json");
  writeFileSync(file, JSON.stringify({ mcpServers: { litopys: { url: "https://a.example.org/v1/mcp" } } }));
  assert.equal(mcpMismatch({ url: "https://a.example.org" }, file), "");
  assert.equal(mcpMismatch({ url: "https://A.example.org:443" }, file), "");
  assert.equal(mcpMismatch({ url: "https://b.example.org" }, file), "https://a.example.org/v1/mcp");
  writeFileSync(file, JSON.stringify({ mcpServers: { litopys: { url: "${LITOPYS_URL}/v1/mcp" } } }));
  assert.equal(mcpMismatch({ url: "https://b.example.org" }, file), "");
  rmSync(dir, { recursive: true, force: true });
});

test("desktopChecks: a network AppData, a local bridge and old Cowork copies, without reading secrets", () => {
  const dir = mkdtempSync(join(tmpdir(), "litopys-desktop-"));
  assert.deepEqual(desktopChecks({ platform: "linux", env: { APPDATA: dir } }), []);
  assert.deepEqual(desktopChecks({ platform: "win32", env: { APPDATA: dir }, version: "0.1.6" }), []);
  const claude = join(dir, "Claude");
  mkdirSync(claude);
  writeFileSync(join(claude, "claude_desktop_config.json"), JSON.stringify({ mcpServers: {
    "litopys-bridge": { command: "npx", args: ["mcp-remote", "https://a.example.org/v1/mcp"], env: { AUTH: "secret-value" } },
    other: { command: "npx", args: ["mcp-remote", "https://elsewhere.example.org"] },
  } }));
  const copy = (version) => {
    const p = join(claude, "local-agent-mode-sessions", `s${version}`, "rpm", "litopys", ".claude-plugin");
    mkdirSync(p, { recursive: true });
    writeFileSync(join(p, "plugin.json"), JSON.stringify({ name: "litopys", version }));
  };
  copy("0.1.4");
  copy("0.1.6");
  const found = desktopChecks({ platform: "win32", env: { APPDATA: dir }, version: "0.1.6" }).join("\n");
  assert.match(found, /local bridge "litopys-bridge"/);
  assert.doesNotMatch(found, /"other"|secret-value/);
  assert.match(found, /old copies of this plugin \(0\.1\.4; current 0\.1\.6\)/);
  // the function, not desktopChecks: a UNC path would make the test look for that host on the network
  assert.match(appDataWarning("\\\\share\\u\\AppData"), /network path/);
  assert.equal(appDataWarning("C:\\Users\\u\\AppData\\Roaming"), "");
  rmSync(dir, { recursive: true, force: true });
});
