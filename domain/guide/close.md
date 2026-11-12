# Checkpoints and closing a session

1. `session_checkpoint {summary, current}` after a meaningful step — a decision made, a PR opened, a fix verified:
   `summary` is what was done, `current` where the work stands. A long session without checkpoints loses its state
   when the conversation is cut or compacted.
2. Do not close the session while work goes on; the user ends it (in clients with commands — their stop command).
3. `session_close {summary, outcomes, next_steps}` when the work is finished:
   - `summary` — 2–5 sentences: what was done and decided;
   - `outcomes` — one per task, record or topic you moved: what came of it, its state (done, partly, blocked,
     dropped…) and the next step. The next session starts from them;
   - `next_steps` — what to do next outside those subjects.
4. A closed session stays closed; `project_resume` opens a new one next time. Closing twice is harmless.
5. A session idle for a day is closed by the server (auto-closed); its last checkpoint summary is kept.
6. Before closing: are the decisions recorded (topic `record`) and the tasks updated (topic `tasks`)?
