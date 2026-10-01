import { spawnSync } from 'node:child_process'
import { flynnBin } from './env'
import { destroy, flynnCmd, poll, provision } from './timeouts'

export type PgRow = { name: string; role: 'primary' | 'follower' }

export function flynn(args: string[], opts?: { timeoutMs?: number; allowFail?: boolean }): string {
  const timeout = opts?.timeoutMs ?? flynnCmd
  const r = spawnSync(flynnBin(), args, {
    encoding: 'utf8',
    timeout,
    env: process.env,
  })
  const stdout = (r.stdout || '').trim()
  const stderr = (r.stderr || '').trim()
  if (r.error) {
    throw new Error(`flynn ${args.join(' ')}: ${r.error.message}\n${stdout}\n${stderr}`)
  }
  if (!opts?.allowFail && r.status !== 0) {
    throw new Error(`flynn ${args.join(' ')} failed (${r.status}):\n${stdout}\n${stderr}`)
  }
  return stdout
}

export function flynnApp(app: string, args: string[], opts?: { timeoutMs?: number; allowFail?: boolean }): string {
  return flynn(['-a', app, ...args], opts)
}

export function parsePgRows(out: string): PgRow[] {
  const rows: PgRow[] = []
  const seen = new Set<string>()
  for (const line of out.split('\n')) {
    const m = line.match(/\b(pg-[a-z]+-[a-z]{6,8})\b/i)
    if (!m) continue
    const name = m[1]
    if (seen.has(name)) continue
    seen.add(name)
    rows.push({
      name,
      role: /\bfollower\b/i.test(line) ? 'follower' : 'primary',
    })
  }
  return rows
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

export async function waitForPsql(app: string, resource = '', timeoutMs = provision): Promise<void> {
  const deadline = Date.now() + timeoutMs
  let last = ''
  while (Date.now() < deadline) {
    const args = resource
      ? ['-a', app, 'pg', 'psql', resource, '--', '-Atc', 'SELECT 1']
      : ['-a', app, 'pg', 'psql', '--', '-Atc', 'SELECT 1']
    try {
      const out = flynn(args, { timeoutMs: flynnCmd })
      if (out.includes('1')) return
      last = out
    } catch (err) {
      last = String(err)
    }
    await sleep(poll)
  }
  throw new Error(`postgres not ready for ${app} ${resource}: ${last}`)
}

export function destroyAppBestEffort(app: string): void {
  if (!app) return
  let list = ''
  try {
    list = flynnApp(app, ['pg'], { allowFail: true })
  } catch {
    list = ''
  }
  const rows = parsePgRows(list)
  const followers = rows.filter((r) => r.role === 'follower').map((r) => r.name)
  const primaries = rows.filter((r) => r.role !== 'follower').map((r) => r.name)
  for (const name of [...followers, ...primaries]) {
    flynnApp(app, ['resource:remove', name], { timeoutMs: destroy, allowFail: true })
  }
  flynnApp(app, ['apps:destroy', '-y'], { timeoutMs: destroy, allowFail: true })
}

export function pgPsql(app: string, extra: string[], resource = '', opts?: { allowFail?: boolean }): string {
  const args = resource
    ? ['pg', 'psql', resource, '--', ...extra]
    : ['pg', 'psql', '--', ...extra]
  return flynnApp(app, args, { allowFail: opts?.allowFail })
}
