---
description: Switch where the layer of this folder's project lives — CLAUDE.md files (files) or Litopys (memory)
argument-hint: "files | memory"
allowed-tools: Bash(node:*), PowerShell(node:*)
---

- **files** (default): the CLAUDE.md of the folder is the source; Litopys keeps a copy and the session start says
  when they differ.
- **memory**: Litopys is the source; the folder's CLAUDE.md is switched off for Claude Code (`claudeMdExcludes` in
  `.claude/settings.local.json`) and the rule comes with the session start. The file itself is not deleted.

If `$ARGUMENTS` is empty, ask which mode. Before `memory`, tell the person that the CLAUDE.md content goes into
Litopys as the rule `instruction/CLAUDE.md` (it must hold no secrets). Then run and show the output:

```
node "${CLAUDE_PLUGIN_ROOT}/cli/litopys.mjs" mode $ARGUMENTS
```

Switching back to `files` restores a missing CLAUDE.md from Litopys, so nothing is lost. Remind the person to
start a new Claude Code session so the file is (re)loaded or skipped.
