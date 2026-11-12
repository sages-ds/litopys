# Litopys — машиночитний опис домену

Статус: v0.1, 2026-09-21. Єдине джерело, з якого генеруються MCP-тули, HTTP API, SDK, CLI і документація. Рукописних поверхонь не існує.

## Файли

| Файл | Що описує |
|---|---|
| `types.yaml` | спільні типи (ідентифікатори, часи, перелічення) — JSON Schema `$defs` |
| `envelope.yaml` | конверт події — однаковий для всіх подій, версіонується окремо від payload |
| `events.yaml` | типи подій з payload-схемами |
| `verbs.yaml` | дієслова API: вхід, вихід, що емітять, проєкції |
| `read-models.yaml` | read-моделі — похідні від журналу, будь-яку можна викинути й перебудувати |
| `link-types.yaml` | типи зв'язків: кінці, зворотна назва, симетричність, цикли, межі, життєвий цикл → `core/linktypes/gen/link_types.json` |
| `vocabularies.yaml` | словники використання: типи записів з родинами, каталог особистих і відомчих словників, мапа спадщини старих типів → `core/vocab/gen/vocabularies.json` |
| `examples/` | приклади реальних процесів (золотий набір), валідатор звіряє їх зі схемами |
| `schema/spec.schema.json` | мета-схема цих файлів |
| `tools/validate.mjs` | валідатор: мета-схема + бритва + конвенції назв |

## Конвенції

**Назви.** Канонічна форма — `<domain>.<verb>` для дієслів і `<entity>.<past_participle>` для подій. Кожна **команда** емітить принаймні одну подію; для більшості — рівно одну, що є дієприкметником того ж дієслова: `statement.assert` → `statement.asserted`, `task.close` → `task.closed`. Запити подій не емітять (лише audit).

**Дієслова.** Уточнення правила стилю `domain.verb` (словники зашиті у валідатор):
- команди — дієслова наміру: `assert, amend, retract, supersede, reinstate, fold, unfold, declare, transition, record, retire, register, revoke, open, checkpoint, close, reopen, attest, challenge, attach, resolve, merge, start, finish, publish, withdraw, grant, purge, produce, set, resume, delegate` (`set` — лише для `policy.set`, де політика і є значенням). CRUD-слова (`create/update/delete`) для команд заборонені;
- запити — обмежений словник: `read, search, list, as_of, timeline, changes, trace, explain, pack, open_items, capabilities, health, whoami, discover`;
- одне дієслово має одне значення в усіх доменах;
- **команда зрізу 1 емітить принаймні одну подію свого домену** — валідатор це перевіряє. Через це два імені першого опису подій змінено: `checkpoint.recorded` → `context.checkpointed`, `purge.executed` → `journal.purged`; також `fact.set` → `fact.recorded`, `project.status_set` → `project.transitioned`, `task.updated` → `task.amended`. Спека первинна щодо першого опису в назвах; семантика та сама.

**Домен `session` і `resume`.** Поверхня каже `session.*`, журнал лишає події `context.*` — валідатор знає псевдонім `session → context`. `resume` в будь-якому домені — команда, що може відкрити сесію (`context.opened`, `run.started`).

**Домени.** `session, context, statement, summary, recall, journal, project, task, fact, consumer, referent, subject, edge, link, policy, space, system`; зріз 2 — `workflow, execution, step, approval`. Домен **`memory` заборонений**: так звалися тули попередника, і назва лишається поза поверхнею, щоб не плутати їх.

**Проєкції** (обчислюються, не пишуться руками):

| Поверхня | Правило | Приклад |
|---|---|---|
| MCP tool | `<domain>_<verb>` (регекс назв тулів `^[a-zA-Z0-9_-]{1,64}$` не допускає крапки) | `statement_assert` |
| HTTP | `POST /v1/<domain>/<verb>`, JSON in/out; усі виклики RPC-стилем, і запити теж — модель командна, не ресурсна | `POST /v1/recall/search` |
| SDK | `client.<domain>.<verb>(input)` | `client.recall.search({...})` |
| CLI | `litopys <domain> <verb> [--flags]` | `litopys recall search --query ...` |

**Зрізи.** Кожен елемент має `slice: 1 | 2 | 3`: 1 — персональний контур (R1, R2, R3, R5, R6, R7, R8, R14); 2 — довіра (R4, R9, R10, R11); 3 — політика, забування, спільний простір (R12, R13, R15, спільний простір). Елементи зрізу 1 мають повні схеми; для 2–3 допускаються заглушки (`status: planned`) зі сценаріями, але без схем — модель проєктується цілою, реалізується зрізами.

**Бритва.** Валідатор відхиляє елемент без `scenarios: [R#]`. Це і є механізм проти «змоделювати надто багато».

## Запуск валідатора

```bash
npm install          # ajv, ajv-formats, yaml
npm run validate     # domain/tools/validate.mjs
```

## Що генерується

`tools/gen.mjs` → `core/api/gen/surface.json` — поверхня власного API v1: по запису на дієслово зрізу 1 з англійським текстом для моделі (поле `tool{}` у verbs.yaml: `title`, `description`, `chat`, `destructive`, `idempotent`, `params`), профілями й вхідною схемою з розгорнутими `$ref`. Українські `description` у моделі не потрапляють. `node domain/tools/gen.mjs --check` — перевірка дрейфу в CI. Далі з того самого файла: OpenAPI 3.1, SDK-TS, CLI-довідка.
