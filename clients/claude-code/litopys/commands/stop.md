---
description: Close this terminal's Litopys session with a summary and the outcome of each subject it moved
allowed-tools: Bash(node:*), PowerShell(node:*)
---

1. Find this terminal's session:

   ```
   node "${CLAUDE_PLUGIN_ROOT}/cli/litopys.mjs" status --terminal ${CLAUDE_SESSION_ID}
   ```

   No open session → say so and stop here.

2. Save what is not saved yet: decisions and findings of this session that are not in Litopys go in with
   `statement_assert` (with the `session_id`), finished or new tasks with `task_*`. A finished task is closed
   with `task_close` — an outcome does not close it.

3. Call `session_close` with that `session_id` and:
   - `summary`: what was achieved, what was decided, what is left open. Write it for the person who resumes
     tomorrow, in the language of the conversation;
   - `outcomes`: one per subject the session moved — a task (`T-12`, or `<project>/T-12` for another project's),
     a record (`R-498` or its id) or a topic without either (`topic:<name>`). Each: `result` (what came of it,
     one sentence), `state` (`done`, `partial`, `blocked` or `failed`), `next` (the next step — required unless
     done), `evidence` (commits, PRs, reports) when there are any;
   - `next_steps`: what the next session should start with, beyond these subjects.

   The plugin forgets the session of this terminal by itself when the call succeeds.

4. Read the answer: `missing_outcomes` names tasks the session changed and left open without an outcome, `hints`
   say e.g. that a task called done is still open. Say them in one line and offer to fix (an outcome cannot be
   added to a closed session — record it with `statement_assert` or in the next session).

5. Confirm in one line: project, what was saved, the outcomes written, that the session is closed
   (`already_closed` means it had been closed before — the summary is kept, outcomes are not written).
