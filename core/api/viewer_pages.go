package api

import (
	"html/template"
	"net/http"
	"strings"
	"time"
)

// Pages of /viewer: one layout, no external resources (the viewer works offline in the LAN and leaks nothing
// to third parties), light and dark by prefers-color-scheme.
const viewerLayout = `<!doctype html>
<html lang="uk"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{block "title" .}}Litopys{{end}}</title>
<style>
:root{--bg:#fbfaf7;--fg:#1f2328;--muted:#656d76;--line:#d8dee4;--accent:#0b5cad;--chip:#eef1f4}
@media (prefers-color-scheme:dark){:root{--bg:#14171b;--fg:#e6e8eb;--muted:#9aa4af;--line:#2d333b;--accent:#6cb6ff;--chip:#22272e}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.55 system-ui,-apple-system,"Segoe UI",sans-serif}
header{display:flex;gap:16px;align-items:center;padding:10px 16px;border-bottom:1px solid var(--line);flex-wrap:wrap}
header a{color:var(--fg);text-decoration:none;font-weight:600}header form{margin-left:auto;display:flex;gap:6px}
main{max-width:920px;margin:0 auto;padding:16px}a{color:var(--accent)}h1{font-size:1.5rem;margin:.2em 0 .6em}
h2{font-size:1.1rem;margin:1.6em 0 .5em;border-bottom:1px solid var(--line);padding-bottom:.2em}
input{font:inherit;padding:6px 8px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--fg);min-width:0}
button{font:inherit;padding:6px 12px;border-radius:6px;border:1px solid var(--line);background:var(--chip);color:var(--fg);cursor:pointer}
.muted{color:var(--muted)}.chip{display:inline-block;padding:0 8px;border-radius:10px;background:var(--chip);font-size:.85em;margin-right:4px}
ul.rows{list-style:none;padding:0;margin:0}ul.rows li{padding:8px 0;border-bottom:1px solid var(--line)}
table{border-collapse:collapse;width:100%;font-size:.95em}td,th{border-bottom:1px solid var(--line);padding:6px 4px;text-align:left;vertical-align:top}
pre{overflow-x:auto;background:var(--chip);padding:10px;border-radius:6px}code{background:var(--chip);padding:0 3px;border-radius:3px}
.body{overflow-wrap:anywhere}details{margin:.4em 0}
</style></head><body>
<header><a href="/viewer/">Litopys</a>{{block "nav" .}}{{end}}
<form action="/viewer/search" method="get"><input name="q" placeholder="Пошук" aria-label="Пошук"><button>Знайти</button></form>
<a class="muted" href="/viewer/logout">Вийти</a></header>
<main>{{block "main" .}}{{end}}</main></body></html>`

var viewerPages = map[string]string{
	"login": `{{define "main"}}<h1>Вхід у перегляд Litopys</h1>
{{with .Error}}<p><strong>{{.}}</strong></p>{{end}}
{{if .OIDC}}<p><a href="/viewer/oidc"><button type="button">Увійти через обліковий запис Litopys</button></a></p>
<details><summary class="muted">або токеном LTP_</summary>{{end}}
<p class="muted">Вставте токен <code>LTP_…</code>. Він зберігається лише в cookie цього браузера (HttpOnly, 7 днів).</p>
<form method="post" action="/viewer/login"><input name="token" type="password" autocomplete="off" required style="width:100%;max-width:520px">
<p><button>Увійти</button></p></form>{{if .OIDC}}</details>{{end}}{{end}}`,

	"graph": `{{define "title"}}Граф {{.Project}} — Litopys{{end}}
{{define "nav"}}<a class="muted" href="/viewer/p/{{.Project}}">{{.Project}}</a>{{end}}
{{define "main"}}<h1>Зв'язки записів {{.Project}}</h1>
{{if .Nodes}}<p class="muted">записів: {{.Nodes}} · зв'язків: {{.Edges}} · колір — тип запису, розмір — кількість зв'язків; наведіть, щоб побачити назву, клацніть, щоб відкрити</p>{{.SVG}}
{{else}}<p class="muted">У записів цього проєкту ще немає зв'язків.</p>{{end}}{{end}}`,

	"error": `{{define "main"}}<h1>Не вдалося</h1><p>{{.Error}}</p><p class="muted">{{.Code}}</p><p><a href="/viewer/">До каталогу</a></p>{{end}}`,

	// the confirmation of a retention label: everything shown comes from the server
	"confirm": `{{define "title"}}Підтвердити мітку зберігання — Litopys{{end}}
{{define "nav"}}<a class="muted" href="/viewer/p/{{.L.Project}}">{{.L.Project}}</a>{{end}}
{{define "main"}}<h1>Мітка зберігання «{{.L.Key}}» у проєкті {{.L.Project}}</h1>
{{with .Problem}}<p><strong>{{.}}</strong> Перегляньте нову версію нижче — нічого не підтверджено.</p>{{end}}
{{if .L.Confirmed}}<p>Ця версія мітки вже підтверджена {{.L.Confirmed.Local.Format "2006-01-02 15:04"}}.</p>
{{else if not .L.HumanOnly}}<p>Ця мітка не потребує підтвердження: worker застосовує її як є.</p>
{{else}}<p>Модель лише <strong>запропонувала</strong> цю мітку. Worker не застосує її, доки ви не підтвердите.</p>
<table><tr><th>Що</th><td>{{index .Rule "applies_to"}}{{if eq (printf "%v" (index .Rule "applies_to")) "record"}} — <strong>усі записи проєкту</strong>{{end}}</td></tr>
<tr><th>Дія</th><td>{{index .Rule "action"}}{{if eq (printf "%v" (index .Rule "action")) "purge"}} — <strong>стирання без відновлення</strong>{{end}}</td></tr>
{{with index .Rule "after_days"}}<tr><th>Старше за</th><td>{{.}} днів</td></tr>{{end}}
{{with index .Rule "retain_until"}}<tr><th>Усе до</th><td>{{.}}</td></tr>{{end}}
<tr><th>Перший прохід зачепить</th><td>записів: {{.L.Impact.Records}}, задач: {{.L.Impact.Tasks}}</td></tr></table>
<form method="post" action="/viewer/confirm/{{.L.Project}}/{{.L.Key}}">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="event_id" value="{{.L.RecordEventID}}">
<input type="hidden" name="records" value="{{.L.Impact.Records}}"><input type="hidden" name="tasks" value="{{.L.Impact.Tasks}}">
<p><button name="action" value="confirm">Підтвердити</button> <button name="action" value="reject">Відхилити (зняти мітку)</button></p></form>
{{end}}{{end}}`,

	"status": `{{define "title"}}Стан — Litopys{{end}}
{{define "nav"}}<a class="muted" href="/viewer/settings/executors">Виконавці</a>{{end}}
{{define "main"}}<h1>Стан Litopys</h1>
<table><tr><th>Апка</th><td>коміт {{.Commit}} · проєктор v{{.Projector}} · {{if .H.OK}}<span class="chip">ok</span>{{else}}<strong>потрібна увага</strong>{{end}}</td></tr>
<tr><th>Журнал</th><td>подій: {{.H.LastSeq}} · відставання проєктора: {{.H.ProjectorLagSeq}}</td></tr></table>
<h2>Раннери</h2>{{if .Runners}}<table><tr><th>Раннер</th><th>Версія</th><th>Останній сигнал</th><th>Виконавці</th></tr>
{{range .Runners}}<tr><td>{{.Runner}}</td><td>{{.Version}}</td><td>{{.SeenAt.Local.Format "2006-01-02 15:04:05"}}{{if .Stale}} <strong>мовчить</strong>{{end}}</td>
<td>{{range .Executors}}<div>{{index . "name"}}{{with index . "error"}} — <strong>{{.}}</strong>{{end}}{{with index . "backoff_s"}} (пауза {{.}} с){{end}}</div>{{end}}</td></tr>{{end}}</table>
{{else}}<p class="muted">Жоден раннер ще не надсилав сигналу (потрібен LITOPYS_RUNNER_SECRET у раннера й апки).</p>{{end}}
<h2>Виконавці</h2>{{if .Executors}}<table><tr><th>Назва</th><th>Модель</th><th>Доступ</th><th>Ключ</th></tr>
{{range .Executors}}<tr><td>{{.Name}}{{if .P.Disabled}} <span class="chip">вимкнено</span>{{end}}</td><td>{{.P.Model}}</td><td>{{.AccessText}}</td>
<td>{{if .KeySet}}{{with .P.Key}}{{.Fingerprint}}{{end}}{{with .CheckOK}}{{if .}} <span class="chip">перевірено</span>{{else}} <strong>помилка</strong>{{end}}{{end}}{{else if .P.SecretRef}}OpenBao{{else if eq .P.KeyKind "local"}}не потрібен{{else}}<strong>немає</strong>{{end}}</td></tr>{{end}}</table>
{{else}}<p class="muted">Виконавців ще немає.</p>{{end}}
<p><a href="/viewer/settings/executors">Налаштувати виконавців</a></p>
<h2>Споживачі журналу</h2>{{if .Consumers}}<table><tr><th>Споживач</th><th>Відставання</th><th>Курсор рухався</th></tr>
{{range .Consumers}}<tr><td>{{.Consumer}}</td><td>{{.Pending}}</td><td>{{.UpdatedAt.Local.Format "2006-01-02 15:04"}}{{if .Stale}} <strong>застиг</strong>{{end}}</td></tr>{{end}}</table>
{{else}}<p class="muted">Споживачів немає.</p>{{end}}{{end}}`,

	"executors": `{{define "title"}}Виконавці — Litopys{{end}}
{{define "nav"}}<a class="muted" href="/viewer/status">Стан</a>{{end}}
{{define "main"}}<h1>Виконавці раннера</h1>
<p class="muted">Моделі, якими раннер виконує кроки процесів без відкритого чату (дайджести, нагадування, підсумки). Ключ вводиться
лише тут і зберігається зашифрованим; в історію потрапляє тільки його відбиток.</p>
{{with .Problem}}<p><strong>{{.}}</strong></p>{{end}}
{{if not .CanStoreKeys}}<p><strong>Апка не має LITOPYS_SECRET_KEY</strong> — ключі тут не збережуться (лише локальні моделі або ключі в OpenBao).</p>{{end}}
{{range .Executors}}{{$id := .ActorID}}<h2>{{.Name}}{{if .P.Disabled}} <span class="chip">вимкнено</span>{{end}}</h2>
<table><tr><th>Вендор / модель</th><td>{{if .Preset.Title}}{{.Preset.Title}}{{else}}{{.P.Provider}}{{end}} · {{.P.Model}}{{with .P.BaseURL}} · <span class="muted">{{.}}</span>{{end}}</td></tr>
<tr><th>Доступ</th><td>{{.AccessText}}{{with .P.Access}}{{with .Consent}}<div class="muted">прийняті умови: {{.}}</div>{{end}}{{end}}</td></tr>
<tr><th>Ключ</th><td>{{if .KeySet}}{{with .P.Key}}відбиток {{.Fingerprint}}, {{.SetAt}}{{end}}{{with .CheckNote}}<div>{{.}}</div>{{end}}{{else if eq .P.KeyKind "local"}}не потрібен{{else}}<strong>немає</strong>{{end}}</td></tr></table>
<form method="post" action="/viewer/settings/executors"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="executor" value="{{$id}}">
<p><button name="action" value="check">Перевірити ключ</button>
{{if .P.Disabled}}<button name="action" value="enable">Увімкнути</button>{{else}}<button name="action" value="disable">Вимкнути</button>{{end}}</p>
<p><input type="password" name="key" placeholder="новий ключ API" autocomplete="off"> <button name="action" value="key">Замінити ключ</button></p>
<p><select name="access"><option value="preset">за класом ({{.P.MaxDataClass}})</option><option value="projects">частково: обрані проєкти</option><option value="all">усі проєкти</option></select>
<input name="projects" placeholder="проєкти через кому"> <label><input type="checkbox" name="consent" value="yes"> я розумію умови вендора</label>
<button name="action" value="access">Змінити доступ</button></p></form>{{end}}
<h2>Додати виконавця</h2>
<form method="get" action="/viewer/settings/executors"><select name="preset">{{range .Presets}}<option value="{{.Key}}"{{if $.P}}{{if eq .Key $.P.Key}} selected{{end}}{{end}}>{{.Title}}</option>{{end}}</select>
<button>Обрати вендора</button></form>
{{with .P}}<form method="post" action="/viewer/settings/executors"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="preset" value="{{.Key}}">
<table><tr><th>Назва</th><td><input name="name" placeholder="{{.Title}} {{.Model}}"></td></tr>
<tr><th>Модель</th><td><input name="model" value="{{.Model}}"></td></tr>
{{range .Fields}}<tr><th>{{.}}</th><td><input name="field_{{.}}" required></td></tr>{{end}}
{{if ne .KeyKind "local"}}<tr><th>Ключ API</th><td><input type="password" name="key" autocomplete="off" required></td></tr>{{end}}
<tr><th>Клас за пресетом</th><td>{{.DataClass}}{{with .DataPolicy}} · навчання: {{if index . "training"}}<strong>так</strong>{{else}}ні{{end}}{{end}}</td></tr>
<tr><th>Доступ</th><td><select name="access"><option value="preset">за класом</option><option value="projects">частково: обрані проєкти</option><option value="all">усі проєкти</option></select>
<input name="projects" placeholder="проєкти через кому"><div><label><input type="checkbox" name="consent" value="yes"> я розумію умови вендора і свідомо розширюю доступ</label></div></td></tr></table>
<p><button name="action" value="add">Додати</button></p></form>
<details open><summary>Як отримати ключ</summary><div class="body">{{md .Help}}</div></details>{{end}}{{end}}`,

	"catalog": `{{define "title"}}Проєкти — Litopys{{end}}{{define "main"}}<h1>Проєкти</h1><ul class="rows">
{{range .Projects}}<li><a href="/viewer/p/{{.Name}}"><strong>{{.Title}}</strong></a> <span class="muted">{{.Name}}</span>
 <span class="chip">{{.Status}}</span>{{if .OpenTasks}}<span class="chip">задач: {{.OpenTasks}}</span>{{end}}
 <span class="muted">· {{date .UpdatedAt}}</span>{{with .Summary}}<div class="muted">{{.}}</div>{{end}}</li>{{end}}</ul>{{end}}`,

	"project": `{{define "title"}}{{.B.Project.Title}} — Litopys{{end}}
{{define "nav"}}<a class="muted" href="/viewer/p/{{.B.Project.Name}}/all">усі записи</a> <a class="muted" href="/viewer/p/{{.B.Project.Name}}/graph">граф</a> <a class="muted" href="/viewer/export/{{.B.Project.Name}}.zip">експорт .zip</a>{{end}}
{{define "main"}}<h1>{{.B.Project.Title}}</h1>
<p class="muted">{{.B.Project.Name}} · <span class="chip">{{.B.Project.Status}}</span> {{range .B.Project.Labels}}<span class="chip">{{.}}</span>{{end}} · оновлено {{date .B.Project.UpdatedAt}}</p>
{{with .B.Project.Summary}}<p>{{.}}</p>{{end}}
{{if .Current}}<h2>Де ми зараз</h2><div class="body">{{.Current}}</div>{{end}}
{{if .Last}}<h2>Остання сесія</h2><div class="body">{{.Last}}</div>{{end}}
{{with .Docs}}<h2>Документи</h2><ul class="rows">{{range .}}<li><a href="/viewer/s/{{.StatementID}}"><strong>{{.Title}}</strong></a>{{if ne .Status "current"}} <span class="chip">{{.Status}}</span>{{end}} <span class="muted">· {{date .EffectiveAt}}</span></li>{{end}}</ul>{{end}}
{{with .B.Tasks}}<h2>Відкриті задачі</h2><table><tr><th>#</th><th>Задача</th><th>Стан</th><th>Пріоритет</th></tr>
{{range .}}<tr><td>{{.Num}}</td><td>{{.Title}}</td><td>{{.Status}}</td><td>{{with .Priority}}{{.}}{{end}}</td></tr>{{end}}</table>{{end}}
{{with .B.Layers}}<h2>Правила шарів</h2><p class="muted">global: {{with .Global}}{{.}}{{else}}—{{end}} · простори: {{range .Spaces}}{{.}} {{else}}—{{end}}</p>
<table><tr><th>Тип</th><th>Ключ</th><th>Шар</th><th>Правило</th></tr>{{range .Stack}}<tr><td>{{.Kind}}</td><td>{{.Key}}</td><td>{{.Layer}}: {{.Source}}</td>
<td><details><summary>показати</summary><div class="body">{{md .Value}}</div></details></td></tr>{{end}}</table>{{end}}
{{with .B.Decisions}}<h2>Чинні рішення</h2><ul class="rows">{{range .}}<li>{{template "row" .}}</li>{{end}}</ul>{{end}}
{{with .B.RecentStatements}}<h2>Останні записи</h2><ul class="rows">{{range .}}<li>{{template "row" .}}</li>{{end}}</ul>{{end}}
{{with .B.Facts}}<h2>Факти</h2><table>{{range .}}<tr><td>{{.Kind}}</td><td>{{.Key}}</td><td>{{with .Value}}{{.}}{{end}}{{with .SecretRef}}<span class="muted">секрет: {{.}}</span>{{end}}</td></tr>{{end}}</table>{{end}}
{{end}}`,

	"list": `{{define "title"}}{{.Title}} — Litopys{{end}}{{define "main"}}<h1>{{.Title}}</h1>
{{if .Search}}<form action="/viewer/search"><input name="q" value="{{.Q}}" style="width:70%"> <button>Знайти</button></form>{{end}}
<ul class="rows">{{range .Items}}<li>{{template "row" .}}{{with .Snippet}}<div class="muted">{{.}}</div>{{end}}</li>{{else}}{{if .Q}}<li class="muted">Нічого не знайдено.</li>{{end}}{{end}}</ul>
{{with .Next}}<p><a href="/viewer/p/{{$.Project}}/all?before={{.}}">далі →</a></p>{{end}}{{end}}`,

	"statement": `{{define "title"}}{{.S.Title}} — Litopys{{end}}
{{define "nav"}}<a class="muted" href="/viewer/p/{{.S.Project}}">{{.S.Project}}</a>{{end}}
{{define "main"}}<h1>{{.S.Title}}</h1>
<p class="muted"><span class="chip">{{.S.Type}}</span><span class="chip">{{.S.Status}}</span>{{if eq .S.Authority "agent"}}<span class="chip">пропозиція агента</span>{{end}}
 {{date .S.EffectiveAt}} · ревізія {{.S.Revision}} · <a href="/viewer/p/{{.S.Project}}">{{.S.Project}}</a></p>
{{with .S.Concepts}}<p>{{range .}}<span class="chip">{{.}}</span>{{end}}</p>{{end}}
<div class="body">{{.Body}}</div>
{{with .S.Files}}<h2>Файли</h2><ul>{{range .}}<li><code>{{.}}</code></li>{{end}}</ul>{{end}}
{{with .Links}}<h2>Зв'язки</h2><ul>{{range .}}<li>{{.Relation}}: {{if .Href}}<a href="{{.Href}}">{{with .Title}}{{.}}{{else}}запис{{end}}</a>{{else}}{{.Title}}{{end}}</li>{{end}}</ul>{{end}}
{{if gt (len .S.History) 1}}<h2>Історія</h2>{{range .S.History}}<details><summary>ревізія {{.Revision}} · {{date .EffectiveAt}} · {{.Status}}{{with .Reason}} — {{.}}{{end}}</summary>
<p><strong>{{.Title}}</strong></p>{{with .Body}}<div class="body">{{md .}}</div>{{end}}</details>{{end}}{{end}}{{end}}`,
}

const rowTemplate = `{{define "row"}}<a href="/viewer/s/{{.StatementID}}">{{.Title}}</a> <span class="chip">{{.Type}}</span>{{if ne .Status "current"}}<span class="chip">{{.Status}}</span>{{end}}
{{if eq .Authority "agent"}}<span class="chip">агент</span>{{end}}<span class="muted">{{.Project}} · {{date .EffectiveAt}}</span>{{end}}`

var viewerFuncs = template.FuncMap{
	"date": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") },
	"md": func(v any) template.HTML {
		switch x := v.(type) {
		case string:
			return renderMD(x)
		case *string:
			if x != nil {
				return renderMD(*x)
			}
		}
		return ""
	},
}

var viewerTemplates = func() map[string]*template.Template {
	out := map[string]*template.Template{}
	for name, body := range viewerPages {
		t := template.Must(template.New("layout").Funcs(viewerFuncs).Parse(viewerLayout))
		template.Must(t.Parse(rowTemplate))
		out[name] = template.Must(t.Parse(body))
	}
	return out
}()

// page renders a page; the caller has not written the body yet.
func page(w http.ResponseWriter, name string, data map[string]any) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	var b strings.Builder
	if err := viewerTemplates[name].Execute(&b, data); err != nil {
		return err
	}
	_, err := w.Write([]byte(b.String()))
	return err
}
