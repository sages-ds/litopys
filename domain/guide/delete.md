# Removing things

You never erase memory yourself. You propose; the person decides.

1. A record is wrong → `statement_retract` with a reason; a task not done → `task_close … dropped` or
   `task_retract`. These hide, they do not erase; they are everyday work.
2. Something must really go (a secret, personal data, a mistake that must not stay): `deletion_propose {ref,
   action, reason}` (`ref` like `R-N` or `<project>/T-N`). Show the person `deletion_impact {ref}` — what depends on
   it — and ask for a GO. Never call
   `deletion_approve` on your own initiative: the person approves, in their words, after seeing the impact.
3. A leaked secret: `journal_quarantine {ref, reason}` hides it at once (reversible with `journal_release`); then
   propose the deletion and tell the person to rotate the secret.
4. Retention labels that erase (`purge`) or cover every record of a project (`applies_to: record`) are proposed,
   not applied: the person confirms them in the Litopys viewer.
5. In a chat client without the `deletion_*` tools: say what should be removed and why, and ask the person to do it
   from a full client or the viewer.
6. Pending proposals appear in `project_resume` (`pending_deletions`): remind the person; do not decide for them.
