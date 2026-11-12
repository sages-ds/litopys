---
description: Save a Litopys checkpoint of this terminal's session without closing it
allowed-tools: Bash(node:*), PowerShell(node:*)
---

1. Get this terminal's session:

   ```
   node "${CLAUDE_PLUGIN_ROOT}/cli/litopys.mjs" status --terminal ${CLAUDE_SESSION_ID}
   ```

   No open session → suggest `/litopys:start <project>` and stop.

2. Save what is not saved yet (`statement_assert` for decisions and findings, `task_*` for tasks), each
   with the `session_id`.

3. Call `session_checkpoint` with the `session_id`, a `summary` of the work so far and `current` — one or
   two lines on where the work stands right now. Do not close the session.

4. Confirm in one line what was saved.
