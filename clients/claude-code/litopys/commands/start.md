---
description: Start or switch this terminal's Litopys session to a project (and bind the current folder to it)
argument-hint: "<project slug or title> [--create]"
allowed-tools: Bash(node:*), PowerShell(node:*)
---

Run exactly this command in the current folder and show its output to the person:

```
node "${CLAUDE_PLUGIN_ROOT}/cli/litopys.mjs" start $ARGUMENTS --terminal ${CLAUDE_SESSION_ID}
```

- It binds the current folder to the project, opens (or continues) this terminal's session there and prints
  the start package. From now on pass the printed `session_id` to every Litopys write.
- `project_not_found`: the project does not exist. Ask the person whether to create it; if yes, repeat
  with `--create` (a human title is fine, the slug is derived).
- `no_token` (connector mode: a cloud session or Cowork, the memory tools come from the Litopys connector): do not
  bind the folder. First note the current session: `node "${CLAUDE_PLUGIN_ROOT}/cli/litopys.mjs" status --terminal ${CLAUDE_SESSION_ID}`.
  Then call the connector's `project_resume` with the project (`auto_create: true` only if the person agreed to
  create it). Only after it answered, close the noted session of the other project, if there was one, with
  `session_close` and a short summary — open first, close after. The plugin keeps the new session by itself.
- Any other error: show it and suggest `/litopys:doctor`.

Then, in two or three lines, tell the person where the project stands and what is next (from the package).
