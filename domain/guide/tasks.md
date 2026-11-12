# Tasks

A task is the work: what to do, its state and the next step. What came of it — results, measurements, history — goes
to records that the task links to (`refs` with `R-N`), not into an ever-growing task body.

1. `task_declare {project, title, …}` — one task per piece of work. Fields: `body`, `kind` (task, bug, requirement,
   question, risk, test), `priority`, `role`, `owner` (who acts), `due`, `parent` (a task of the same project),
   `acceptance` (how to check it is done), `labels`, `refs`. An open task with the same title is not duplicated.
2. Numbers are per project: `<project>/T-N`; `task_list` shows them ("what next" order: in progress, review,
   testing, analysis, open, blocked, waiting, backlog).
3. `task_amend {project, num, …}` — change only what changed: status, priority, owner, a ref to the record of a
   result. Statuses: backlog, open, analysis, in_progress, review, testing, waiting, blocked.
4. `task_close {project, num, outcome, resolution}` — outcome `done` (achieved) or `dropped` (not doing it), with a
   one-line resolution. A closed task is history; reopen with `task_reopen` only if the work really resumes.
5. Waiting on someone or something: status `waiting` and a ref to what it waits for (a task, a record).
6. Ending a session: name the tasks you moved in the `outcomes` of `session_close` (topic `close`).

Work another project owns is not a task here — see topic `cross-project`.
