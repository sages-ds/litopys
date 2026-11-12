---
description: Diagnose the Litopys plugin — surface, token, server, folder binding, this terminal's session, setup advice
argument-hint: "[--advice]"
allowed-tools: Bash(node:*), PowerShell(node:*)
---

Run and show the output:

```
node "${CLAUDE_PLUGIN_ROOT}/cli/litopys.mjs" doctor --terminal ${CLAUDE_SESSION_ID} $ARGUMENTS
```

For every `FAIL` and `WARN` line say what to do in plain words. Never print the value of `LITOPYS_TOKEN`; check only
whether it is set. If everything is `OK`, say so in one line. Offer `--advice` when the person is setting up
claude.ai, Desktop, Cowork or a cloud session.

On `INFO connector mode`, call the connector's `system_whoami` tool as well:
- `service: Litopys` with the person's name means the connector works. There is no plugin version in this answer, and
  nothing to update.
- No such tool: the connector is not connected in this session, so give the steps from `--advice`.
