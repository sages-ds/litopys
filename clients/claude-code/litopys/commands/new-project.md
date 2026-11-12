---
description: Create a Litopys project — in memory only, or with a folder that is created and bound to it
argument-hint: "<title> [--path <folder>] [--summary <text>]"
allowed-tools: Bash(node:*), PowerShell(node:*)
---

1. If the person gave no title, ask for it (any language: the slug is derived). Ask whether the project needs a
   folder on this machine; without one it lives in Litopys only.
2. Run and show the output:

   ```
   node "${CLAUDE_PLUGIN_ROOT}/cli/litopys.mjs" new $ARGUMENTS
   ```

3. Offer the next step: `/litopys:start <slug>` in the folder to open a session there. Creating a git
   repository or project files is not part of this command — say so if the person expects it.
