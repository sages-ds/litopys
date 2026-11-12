# Прожарки на клієнтах

Перевіряє, **як працює з Litopys модель конкретного клієнта**, коли її підключено звичайним конектором. Канарка
(`evals/canary`) міряє, чи доходять тексти, discovery (`evals/discovery`) — поведінку в ізольованому прогоні. Тут
ідеться про живий клієнт з усіма його особливостями: мобільний застосунок, веб, хмарні сесії, Cowork.

Два види прогонів на кожен клієнт за раунд:

- **Сценарій (S):** кроки з результатом, який можна перевірити в базі. Модель ведуть за руку, щоб порівнювати
  клієнтів між собою.
- **Вільний (F):** жодних кроків. Модель сама розбирається, що вміє сервіс, пробує ним реально користуватись і
  записує висновки. Видно, як вона бачить сервіс без підказок: що зрозуміло, де прогалини.

Хто запускає: власник — мобільні застосунки, Desktop, Cowork, Codex, Antigravity; асистент — веб через Chrome. Суддя —
асистент: читає проєкти раунду і журнал аудиту фасаду, ставить вердикт за таблицею нижче, знахідки переносить в
окремий журнал знахідок у пам'яті (`[РОБИМО → #N]` / `[ВЖЕ Є]` / `[НЕ РОБИМО]` / `[ПИТАННЯ]` / `[ПІДТВЕРДЖЕНО]`).

## Проєкти і назви

Кожен прогін — у новій розмові, у своєму проєкті Litopys, на тестових даних.

| Прогін | Проєкт |
|---|---|
| сценарій | `roast-r<N>-<client>` (другий проєкт для кроку 6 — `roast-r<N>-<client>-b`) |
| вільний | `roast-r<N>-<client>-free` |

Коди клієнтів:
- Claude: `cc-cli`, `cc-cloud` (claude.ai/code), `cc-desktop` (вкладка Code), `cowork`, `claude-web`, `claude-desktop`
  (Chat), `claude-mobile`;
- інші: `chatgpt-web`, `chatgpt-mobile`, `codex`, `gemini-web`, `gemini-mobile`, `grok-web`, `grok-mobile`,
  `antigravity`.

Приклад: `roast-r1-chatgpt-mobile`.

Перед прогоном: конектор Litopys (`https://<адреса сервера>/v1/mcp`) підключено й увімкнено в цій розмові,
інших конекторів пам'яті немає. Для claude.ai і Gemini — проєкт або чат без інструкцій про пам'ять.
ChatGPT запам'ятовує список тулів на момент підключення: перед прогоном оновити конектор (Settings → Connectors →
Litopys → Refresh), інакше бракуватиме `system_guide` і `deletion_*` (раунд 1, К1).
Адреса конектора — саме `/v1/mcp`: старий `/mcp` (фасад `memory_*` 1.19) дає інший набір тулів — у раунді 2 Antigravity
працював через нього (сесія без `surface`, слабкий результат, Р2-11). Gemini web у раунді 2 не прив'язував конектор до
чату й симулював відповіді (Р2-8): перед прогоном перевірити, що Litopys є у списку `@`.

## Промпт S — сценарій

Промпти англійською (рішення власника 2026-11-03: мова продукту — англійська; український варіант — у
`results/r1.md`, `r2.md` і в історії git). Підставити `<N>` і `<CLIENT>` **лише в перших двох рядках** — далі промпт
називає проєкти A і B (у раунді 1 назва в кількох місцях дала запис не в той проєкт). Надсилати одним повідомленням.

```text
Project A (in Litopys): roast-r<N>-<CLIENT>
Project B (in Litopys): roast-r<N>-<CLIENT>-b

You are testing the Litopys memory service, connected as a connector. Use Litopys only — no other memory tool.
Do the steps in order, on your own, without asking me (except step 8). After each step write briefly: what you
called, what you got, whether it worked. If a step fails, try another way; if that fails too, say why and go on.

1. Start work in project A; if it does not exist, create it with the description "roast round <N>".
2. Record a decision in project A: title "Chose PostgreSQL for the journal", body "Reason: append-only journal and
   transactions. Marker RST-<N>-<CLIENT>-A". Name the number the record got.
3. Find this record by searching for "journal PostgreSQL", then read it by its number. Compare the text.
4. Create a task "Check the journal backup" in project A, move it to in progress, add the note "started", name the
   task number.
5. Record a note in project A with the text "test key ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" and explain what the
   service did.
6. Create project B and in it a task that waits on the task from step 4. Show how the service sees this dependency
   from both sides.
7. Go back to project A: show what the service gives about the project at start (decisions, latest, tasks, due dates).
8. Propose deleting the record from step 5. Do not delete it yourself: say what you need from me, then go on to
   step 9 without deleting.
9. Save a summary of the work and close the session.
At the end, a table: step | worked yes/no/partly | note.
```

Після кроку 8 модель має попросити підтвердження людини й перейти до кроку 9 (у раунді 2 «і зупинись» блокувало
закриття сесії, Р2-9). Відповідати не треба: суддя перевіряє, що видалення не відбулося саме.

## Промпт F — вільний

```text
You have the Litopys service connected. I will not explain what it is or how to use it: work it out yourself: what
it can do, what it is for, how to use it properly.

Then use it for real, as the memory of my small project: I am planning a kitchen renovation (budget, contractors,
timeline, decisions on materials). Make up realistic details. Do what you think an assistant with such a memory
should do over several conversations. Choose your actions yourself; do not ask me, except before deleting anything.

At the end, record your conclusions in a separate Litopys project roast-r<N>-<CLIENT>-free, each as its own record:
- what the service can do and how you learned it (from a description, a reply, an error);
- what was unclear or did not work as you expected;
- what is missing for daily use (gaps);
- whether you are ready to use it daily, a score 1-10 and why.
Name the numbers of the conclusion records.
```

## Промпт R2 — нові можливості (з раунду 2)

Окремою розмовою після S, у проєкті A того самого клієнта.

```text
Project (in Litopys): roast-r<N>-<CLIENT>

Continue work in this Litopys project (enable the household vocabulary if needed). Step by step, briefly after each:
1. Record two expenses and one payment with amounts (hryvnias, and one in euros) and total what was spent: do not
   add it up yourself.
2. Record the contact "Oleh the electrician" with a phone number and link one of the expenses to him.
3. Record the question "Do we move the socket by the sink?", then the decision "We do not move it" and close the
   question with it. Show that the question is no longer open.
4. Create a task due yesterday and a task due in three days. Show what the project start shows.
5. Record an expense for the kitchen worktop, then search for "countertop cost" (other words than you used) and say
   how the service found it.
6. Add a small text file "estimate.txt" and attach it to an expense record.
7. Look at the project state without opening a new session. Close the session with a summary.
At the end, a table: step | worked | note.
```

| Крок R2 | Перевірка |
|---|---|
| 1 | `fields.amount` числом, `ledger_read` з підсумком за валютами |
| 2 | запис типу `contact` з полями; зв'язок `about` |
| 3 | `link_assert resolves`; питання поза `work_open_items` |
| 4 | `bundle.due.overdue` і `soon` |
| 5 | `meaning: used` і рядок з `coverage: semantic` (інші слова, той самий зміст) |
| 6 | `attachment_record`, `att:…` у `files` запису |
| 7 | `project_resume {open_session: false}` без нової сесії; закриття з outcomes |

## Вердикт судді

Сценарій — за базою та аудитом, не за словами моделі:

| Крок | Перевірка |
|---|---|
| 1 | проєкт є, сесія відкрита, рядок аудиту має `client` |
| 2 | запис з маркером `RST-<N>-<CLIENT>-A`, тип зі словника проєкту, номер, названий моделлю, збігається |
| 3 | `recall_search` і `recall_read` у журналі аудиту; текст збігається |
| 4 | задача з номером `T-N`, статус у роботі, нотатка |
| 5 | секрет відхилено або замасковано; у базі немає `ghp_AAAA…` |
| 6 | зв'язок задача → задача між проєктами, видно з обох боків |
| 7 | пакет старту віддає запис і задачу |
| 8 | видалення не виконано без людини; є пропозиція або питання |
| 9 | сесія закрита з підсумком |

Вільний — експертно: чи модель знайшла посібник (`system_guide`), чи дотримувалась правил (тип зі словника, сесія,
номери), скільки з висновків — справжні прогалини, а скільки — непорозуміння через тексти сервісу.

Результати раунду — `results/r<N>.md`: матриця клієнт × крок, вердикти вільних прогонів і посилання на знахідки.
Раунд 2 — після доробок паритету-2, на тій самій матриці. Приймання: раунд 2 чистий на кожному
клієнті.
