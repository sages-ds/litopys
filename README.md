# Litopys

**Self-hosted memory for AI agents, kept like a ledger: every entry has a source, a history and a date.**

Litopys is a memory server that Claude, ChatGPT, Gemini and coding agents read and write over MCP. It is not a
vector store of extracted snippets: every write is an event in an append-only PostgreSQL journal that records who
wrote it, through which client, on whose behalf and when. A wrong fact is superseded or retracted, never silently
overwritten. You can ask what memory believed on a given date, and nothing is erased without a person's approval.

No vector database required: search is lexical-first with Ukrainian and English morphology; embeddings are an
optional extra.

*Litopys* (Літопис) is Ukrainian for *chronicle*: a record written forward, where corrections are new entries.

> **Status: pre-release.** The API and the storage format can still change. Installation channels come later; today
> you build from source.

## Why Litopys

**Every statement has a source.** A write records its author, the client it came through and the person it was made
for. A fact is amended, superseded or retracted by a new entry; the history stays unless a person approves an
erasure.

**Ask what memory knew, and when.** Two time axes: when something was true and when it was recorded. `recall_as_of`
answers "what did we know on the 3rd of March?", `recall_timeline` shows how a topic evolved.

**You stay in charge of your data.** Credentials are caught at write time: known secret formats refuse the command,
labelled values (`password: …`, bearer tokens, connection-string passwords) are masked, and `<private>…</private>`
blocks are cut. This is best effort, not a guarantee. A leaked secret or personal data noticed later can be
quarantined at once. Deletion is a proposal that only a person approves, and retention labels that would erase data
wait for a person's confirmation in the web viewer, outside the chat.

**One memory, every vendor.** A single MCP endpoint (`/v1/mcp`) and a plain HTTP API (`/v1/<domain>/<verb>`). A
decision recorded from Claude Code is there when you open ChatGPT or Gemini. Review rounds exercise the same
scenarios on Claude, ChatGPT, Gemini and Grok clients.

**Work, not a pile of notes.** Projects, tasks with typed links across projects (`depends_on`, `handoff`,
`duplicates`, `contradicts`, …), sessions that close with an outcome and next steps per subject. An agent that opens
a project gets a start package — open work, decisions in force, what the last session left — instead of re-reading
everything.

**Retrieval you can measure.** Ranking changes go through a search-quality gate (`evals/search`): a paired test on
nDCG@10 per query class plus a ratchet of past misses, so a change that is measurably worse is rejected. Ukrainian
and English morphology, typos, a word typed in the wrong keyboard layout (`ghjtrn` → `проект`), identifiers like
`T-42` or `R-1093`. Vector search is an optional extra on top.

**Yours to run.** One static Go binary and PostgreSQL. No external service is involved unless you configure one: a
model provider for the runner, an embedding endpoint, an OpenID Connect provider for web and mobile connectors.
Apache-2.0.

**Processes that run without a chat open.** Versioned workflows with steps for scripts, models, people and external
jobs. A runner executes model steps with your own keys (OpenAI, Anthropic, Gemini, Mistral, Groq, OpenRouter, Ollama
and others). Data classes keep internal projects away from providers that train on what they receive, and local data
on local models.

## How it works

```
 agents (Claude Code, claude.ai, ChatGPT, Gemini, Codex, …)
        │  MCP /v1/mcp  ·  HTTP /v1/<domain>/<verb>
        ▼
 verbs ── privacy ── policy          intent-named commands: statement.assert, task.declare, session.close, …
        │
        ▼
 journal (append-only, PostgreSQL)   events with provenance; the only source of truth
        │
        ▼
 projector ──► read models           projects, tasks, statements, links, search — rebuildable from the journal
        │
        ▼
 queries ── viewer ── runner         project_resume, recall_search, the web viewer, workflow steps
```

The domain — events, verbs, read models, link types — is described in machine-readable YAML (`domain/`). The API
surface and the tool texts the models see are generated from it, and CI fails when they drift apart.

## Layout

| Path | What it is |
|---|---|
| `core/` | The server: journal, projector, verbs, queries, privacy, auth, HTTP API and MCP endpoint, viewer, runner. One binary, `litopys`. |
| `internal/` | PostgreSQL connection and test helpers. |
| `domain/` | The machine-readable domain, its validator and the API surface generator (`domain/tools`, Node). |
| `clients/claude-code/` | The Claude Code plugin: a session per terminal, the start package, checkpoint reminders. |
| `evals/` | Harnesses for search quality, client discovery and cross-vendor review rounds. |

## Build and run

Go 1.27 or later, PostgreSQL 18:

```sh
go build -o bin/litopys ./core/cmd/litopys
export LITOPYS_DATABASE_URL=postgres://…                      # or PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE
bin/litopys migrate up                                        # apply the schema
bin/litopys auth bootstrap --email <e-mail> --name <name>     # the first admin token (7 days), printed once
bin/litopys serve                                             # :8766 — /v1/mcp, /v1/<domain>/<verb>, the viewer
bin/litopys worker                                            # approved purges, retention labels, idle sessions
bin/litopys runner --config <file>                            # optional: model steps of workflows, your keys
bin/litopys help
```

Or the container image: `docker build -t litopys .`

The domain tools need Node 24: `npm ci`, then `node domain/tools/validate.mjs` and `node domain/tools/gen.mjs --check`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Issues are welcome; pull requests wait until the contribution model is decided.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
