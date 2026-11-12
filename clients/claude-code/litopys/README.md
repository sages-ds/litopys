# Litopys — the Claude Code plugin

Litopys project memory in Claude Code: a session for this terminal in the folder's project, the start package from
the native API, checkpoint reminders, folder bindings.

## Install

1. **A token.** Issue one for the plugin on the server host (the command needs the database, as in the main README) —
   run it yourself in your own terminal, not through an assistant session — and keep it in the user environment
   variable `LITOPYS_TOKEN` (never in a file):

   ```
   litopys auth bootstrap --email <your e-mail> --name <your name> --role owner --profile core \
     --vendor anthropic --label claude-code-plugin --days 180
   ```

   The token is printed once.

2. **The server address.** The plugin's code holds no address: it belongs to the installation. Two ways:
   - build the installation's marketplace. The address becomes fixed in `.mcp.json`, and only then can claude.ai
     match the plugin's connector to the organisation's connector:

     ```
     node clients/claude-code/build-marketplace.mjs --url https://<server address> --out <new folder>
     ```

   - or install the plugin straight from `clients/claude-code` and set the environment variable `LITOPYS_URL`.

   A built plugin works with its own server only. If `LITOPYS_URL` points elsewhere, the hooks switch off, the Litopys
   tools are blocked (the plugin's and the connector's) and `doctor` names the reason. Another server (a staging one,
   say) needs its own build.

   The version of a built plugin carries a hash of the address, so after a rebuild for a new address `/plugin update`
   moves the clients to the new server.

3. **Marketplace and plugin:**

   ```
   /plugin marketplace add <marketplace folder>
   /plugin install litopys@litopys
   ```

   If a marketplace `litopys` is already added as a repository folder, run `/plugin marketplace remove litopys` first.

4. Restart Claude Code. Check with `/litopys:doctor`; advice for claude.ai, Desktop, Cowork and cloud sessions —
   `/litopys:doctor --advice`.

   A user environment variable is seen only by processes started after it was written: if doctor says "in your user
   environment but not in this process", close every Claude Code window and terminal and start again. Without a token
   the MCP connector `litopys` still registers (`.mcp.json` is static) and sends an empty Bearer — its tools answer
   `401 invalid token`; the hooks of such a session only say that memory is off.

**Connector mode (no `LITOPYS_TOKEN`, since 0.1.5).** In a claude.ai/code cloud session and in Cowork the plugin
comes without a token and the memory tools come from the Litopys connector. The start hook then calls nothing itself:
it asks the model to open the session with the connector's `project_resume` before answering the first message,
whatever it is. The project is chosen like this:
- the one named in the message;
- otherwise an exact match of the folder name;
- partial matches — the model lets the person choose;
- no match, the Cowork folder (`claude`) or the home folder — `global`.

Desktop's background probes (`claude-sandbox-status-*`) open no session. `post-tool-use` keeps the conversation's
marker from the `project_resume` answer, so the checkpoint reminder, `/litopys:checkpoint` and `/litopys:stop` work
here too. Switching — `/litopys:start <project>`: the new session first, then the old one is closed. Without the
connector's tools memory is off, as before.

Another memory plugin can run alongside: they do not conflict (different servers and tool names).

## How it works

| Event | What the plugin does |
|---|---|
| Claude Code session start (`startup`) | finds the folder's project: a binding (the deepest folder wins), otherwise the folder name = the slug of an existing project; opens the session **of this terminal** (`session_open`) and gives the model the `project_resume` package with `session_id` |
| `resume`, `compact` | continues the session from the terminal's marker |
| A write to Litopys (`statement_*`, `task_*`, `fact_*`…) | counts the writes since the last checkpoint (locally) |
| Stop | once per `LITOPYS_CHECKPOINT_MINUTES` (30; 0 — off), if there were writes since the last checkpoint or file edits (Edit / Write / MultiEdit / NotebookEdit) since the last write to memory, asks the model to record a finding and call `session_checkpoint` |
| `session_close` by the model | the marker forgets the session |
| Read / Edit / Write / Glob / Grep of a file in the folder of **another** bound project | once per session — that project's layer rules next to the result |
| Sub-agent start | the layer rules of the session's project; a sub-agent's touches of other projects are counted apart from the main agent's |
| Agents and skills of the layers | in both modes — the files `.claude/agents/litopys-*.md` and `.claude/skills/litopys-*/` of the bound folder (outside git), refreshed at every start |

A project's layer lives either in files (`files`, the default: the folder's CLAUDE.md is primary, a difference from
the copy in Litopys shows at the start) or in Litopys (`memory`: `/litopys:mode memory` — the file is switched off
through `claudeMdExcludes`, the rule comes with the start; `/litopys:mode files` brings the file back).

Commands: `/litopys:start <project> [--create]` (bind the folder and open a session), `/litopys:checkpoint`,
`/litopys:stop` (summary, the per-subject `outcomes` and `next_steps`, close), `/litopys:new-project <name>
[--path <folder>]` (a new project; the folder is optional), `/litopys:projects` (the registry: projects and folders on
this machine), `/litopys:layers` (the stack of layer rules global → spaces → project; add a space, a global layer or a
rule), `/litopys:doctor`.

The plugin's data — `~/.claude/litopys/`: `terminals/<conversation id>.json` (markers), `bindings.json` (the bindings
cache). The source of bindings is Litopys: a `binding` fact of the project per folder of a machine (key
`<machine>:<path hash>`). The cache refreshes once per `LITOPYS_SYNC_MINUTES` (60) and when a folder is unknown; a
binding made offline stays local until the next sync. No secrets in files.

## Tests

```
node --test clients/claude-code/litopys/test/plugin.test.mjs
```

A fake `/v1` server, no network and no Claude Code.
