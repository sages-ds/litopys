# Start or switch work

1. Call `project_resume` first in every conversation, before answering about the user's projects.
   - Without `project` it returns the catalog: slug, title, a short summary and open tasks of each project.
     Pick the project with the user; do not guess from the conversation.
   - With `project` (the slug or the human title — both work) it attaches your session in that project and returns
     its state: where work stopped, the last session summary, open tasks in "what next" order, recent decisions,
     facts, `next_steps`, `rules` and links to other projects.
2. The session belongs to this client. Later writes need no session id: they go to your open session in that
   project. Calling `project_resume` again reuses it.
3. The user names another project → call `project_resume` with it: that is the switch. Writing about another
   project does not need a switch — pass its name in `project` of the write.
4. A project that does not exist yet: ask the user, then `project_resume {project, auto_create: true}`.
5. A project labelled `global` holds the user's rules for every project; its facts reach you in `bundle.layers`.
6. Read `rules` in the answer and follow them; this guide explains the how.

Example: `project_resume {"project": "home network"}` → read `next_steps`, tell the user in a few lines where the
work stopped, ask what to do.
