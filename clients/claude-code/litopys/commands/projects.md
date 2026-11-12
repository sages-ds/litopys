---
description: The registry of Litopys projects with the folders bound on this machine
allowed-tools: Bash(node:*), PowerShell(node:*)
---

Run and show the output as a compact table (project, title, status, folder on this machine):

```
node "${CLAUDE_PLUGIN_ROOT}/cli/litopys.mjs" projects
```

Projects without a folder live in Litopys only; `/litopys:start <project>` in a folder binds it.
