// Discovery benchmark: does a clean model understand Litopys from the
// connection alone and act right? Two commands, both against a throw-away facade (run.ps1):
//   node disc.mjs seed                       seeds scenarios.json through the native HTTP API (DISC_TOKEN)
//   node disc.mjs score <scenario> <log> <answer> <seconds>   one client run -> one JSON line on stdout
// The facade's audit log is the vendor-neutral record of what the model did (api_method / call lines);
// the database state, read back through the API, is what it achieved. No memory content is printed
// beyond what the scenario itself seeded or asked for.
import { readFileSync } from 'node:fs'

const here = new URL('.', import.meta.url)
const spec = JSON.parse(readFileSync(new URL('scenarios.json', here), 'utf8'))
const base = process.env.DISC_URL ?? 'http://127.0.0.1:8795'
const token = process.env.DISC_TOKEN
if (!token) throw new Error('DISC_TOKEN is not set')

async function call(verb, args = {}) {
  const [domain, name] = verb.split('.')
  const r = await fetch(`${base}/v1/${domain}/${name}`, {
    method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify(args),
  })
  const body = await r.json()
  if (!r.ok) throw new Error(`${verb}: ${r.status} ${JSON.stringify(body.error ?? body)}`)
  return body
}

// tools whose success changes memory content; sessions and resume are bookkeeping, not content
const contentWrite = /^(statement_(assert|amend|retract|reinstate|supersede)|task_(declare|amend|close|reopen|retract)|fact_(record|retire)|project_(declare|amend|transition)|link_(assert|retire)|policy_set|actor_(register|revoke))$/

function parseLog(text) {
  const calls = [], methods = [], security = []
  for (const line of text.split(/\r?\n/)) {
    let m
    if ((m = line.match(/ call api=v1 tool=(\S+) .* ok=(true|false)/))) calls.push({ tool: m[1], ok: m[2] === 'true' })
    else if ((m = line.match(/ api_method method=(\S+)/))) methods.push(m[1])
    else if ((m = line.match(/ security event=(\S+)/))) security.push(m[1])
  }
  return { calls, methods, security }
}

async function projects() {
  const r = await call('project.list', {})
  return (r.projects ?? r.items ?? []).map(p => ({ name: p.name ?? p.slug, title: p.title ?? '' }))
}

async function seed() {
  for (const p of spec.seed.projects) await call('project.declare', p)
  for (const p of spec.seed.projects) {
    await call('project.resume', { project: p.name })
    for (const s of spec.seed.statements.filter(s => s.project === p.name)) await call('statement.assert', s)
    for (const t of spec.seed.tasks.filter(t => t.project === p.name)) await call('task.declare', t)
  }
  await call('session.close', { summary: 'benchmark seed' }).catch(() => {}) // the harness's own session
  console.log(JSON.stringify({ seeded: (await projects()).map(p => p.name) }))
}

async function score(id, logFile, answerFile, seconds) {
  const sc = spec.scenarios.find(s => s.id === id)
  if (!sc) throw new Error(`unknown scenario ${id}`)
  const { calls, methods, security } = parseLog(readFileSync(logFile, 'utf8'))
  const answer = readFileSync(answerFile, 'utf8')
  const lower = answer.toLowerCase()
  const writes = calls.filter(c => c.ok && contentWrite.test(c.tool))
  const firstWrite = calls.findIndex(c => c.ok && contentWrite.test(c.tool))
  const e = sc.expect, checks = {}
  const seeded = new Set(spec.seed.projects.map(p => p.name))
  if (e.understanding) checks.understanding = e.understanding.filter(g => g.some(w => lower.includes(w))).length >= Math.ceil(e.understanding.length * 0.6)
  if (e.answerMatches) checks.answer = e.answerMatches.every(w => lower.includes(w))
  // the answer must come from Litopys, not from the client's own memory or a guess
  if (e.mustCall) checks.usedLitopys = calls.some(c => c.ok && new RegExp(e.mustCall).test(c.tool))
  if (e.statementIn) {
    const r = await call('recall.search', { query: e.statementIn.match, project: e.statementIn.project, limit: 10 })
    checks.statementIn = JSON.stringify(r).toLowerCase().includes(e.statementIn.match)
  }
  const now = await projects()
  const fresh = now.filter(p => !seeded.has(p.name))
  if (e.noNewProjects) checks.noNewProjects = fresh.length === 0
  if (e.newProject) {
    const re = new RegExp(e.newProject.match, 'i')
    const hit = fresh.find(p => re.test(p.name) || re.test(p.title))
    checks.newProject = Boolean(hit)
    if (e.taskInNewProject) {
      const tre = new RegExp(e.taskInNewProject.match, 'i')
      checks.taskInNewProject = hit ? tre.test(JSON.stringify(await call('task.list', { project: hit.name, state: 'all' }))) : false
    }
  }
  const pass = Object.values(checks).every(Boolean)
  console.log(JSON.stringify({
    scenario: id, pass, checks,
    metrics: {
      seconds: Number(seconds),
      calls: calls.length,
      exploratoryCalls: firstWrite === -1 ? calls.length : firstWrite, // reads before the first content write
      failedCalls: calls.filter(c => !c.ok).length,
      contentWrites: writes.length,
      unnecessaryWrites: Math.max(0, writes.length - e.contentWrites),
      resumeFirst: calls[0]?.tool?.endsWith('_resume') ?? false,
      askedUser: !pass && /\?\s*$/.test(answer.trim()), // stopped to ask instead of acting
      securityEvents: security.length,
    },
    sequence: calls.map(c => c.ok ? c.tool : `${c.tool}!`),
    methods: [...new Set(methods)],
    newProjects: fresh,
  }))
}

const [cmd, ...rest] = process.argv.slice(2)
if (cmd === 'seed') await seed()
else if (cmd === 'score') await score(...rest)
else throw new Error('usage: disc.mjs seed | score <scenario> <log> <answer> <seconds>')
