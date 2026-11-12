# Work that crosses projects

One rule: **a task lives open in one place — the project within which the action is done** (the owner). A project
that needs the result keeps a mirror.

1. Who owns it: the project whose boundaries the action falls in (see each project's summary). Unclear → ask the
   user; do not guess.
2. In the owner project: `task_declare` the work there (pass that project's name; your session stays where it is).
3. In the consumer project: a mirror — `task_declare` with status `waiting`, `refs` `["<owner>/T-N"]`, label
   `mirror`, and `acceptance` saying how the consumer checks the result. Then
   `link_assert {from: mirror, to: owner task, relation: handoff}`.
4. Never two open copies of the same work. Found a duplicate: keep it open at the owner, turn the other into a
   waiting mirror.
5. Closing: the owner closes its task (`done` + resolution). The consumer closes the mirror only after checking the
   result against its acceptance.
6. A task that waits on another project's task without a handoff: `link_assert … relation: depends_on`.
7. Check: `work_open_items` of a project lists in `cross_project` the mirrors whose owner task is closed or gone,
   mirrors without a handoff link, the same Jira key open in two projects, and the tasks other projects wait on
   (`waiting_on_us`). Fix what it names.
8. Lasting dependency between projects: `link_assert` between the projects (`depends_on`, `predecessor_of`,
   `parallel_line_of`); `project_resume` shows them in `links`.
