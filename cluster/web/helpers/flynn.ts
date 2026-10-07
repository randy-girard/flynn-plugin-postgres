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

const uuidResource = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const isolatedPostgresName = /^(postgresql-[a-z0-9]+(?:-[a-z0-9]+)*-[0-9]{5,8}|pg-[a-z]+-[a-z]{6,8})$/i

export function assertIsolatedPostgresName(name: string): void {
  if (!name || uuidResource.test(name) || !isolatedPostgresName.test(name)) {
    throw new Error(`want isolated postgres instance name, got ${JSON.stringify(name)}`)
  }
}

export function parsePgRows(out: string): PgRow[] {
  const rows: PgRow[] = []
  const seen = new Set<string>()
  const re = /\b(postgresql-[a-z0-9]+(?:-[a-z0-9]+)*-[0-9]{5,8}|pg-[a-z]+-[a-z]{6,8})\b/gi
  for (const line of out.split('\n')) {
    const m = line.match(re)
    if (!m) continue
    const name = m[0]
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
    const target = resource || defaultPsqlResource(app)
    const args = target
      ? ['-a', app, 'pg', 'psql', target, '--', '-Atc', 'SELECT 1']
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

export function envHasKey(env: string, key: string): boolean {
  const prefix = `${key}=`
  return env.startsWith(prefix) || env.includes(`\n${prefix}`)
}

export function scopedDatabaseURLKey(name: string): string {
  return `${name.trim().replace(/-/g, '_').toUpperCase()}_DATABASE_URL`
}

export function postgresColorURLKeys(env: string): string[] {
  const keys: string[] = []
  for (const line of env.split('\n')) {
    const eq = line.indexOf('=')
    if (eq <= 0) continue
    const k = line.slice(0, eq)
    if (k.startsWith('FLYNN_POSTGRESQL_') && k.endsWith('_URL')) keys.push(k)
  }
  return keys
}

export function assertPostgresAppEnvCommon(env: string, resource: string): void {
  const named = scopedDatabaseURLKey(resource)
  if (resource && envHasKey(env, named)) {
    throw new Error(`postgres must not set instance-named URL ${named}:\n${env}`)
  }
  for (const k of [
    'PGHOST', 'PGPORT', 'PGUSER', 'PGPASSWORD', 'PGDATABASE', 'PGSSLMODE',
    'POSTGRES_URL', 'POSTGRES_DB', 'POSTGRES_USER', 'POSTGRES_PASSWORD', 'POSTGRES_HOST',
    'FLYNN_POSTGRES', 'POSTGRES_ROLE',
  ]) {
    if (envHasKey(env, k)) {
      throw new Error(`app must not set ${k}:\n${env}`)
    }
  }
}

export function assertPostgresAppEnv(env: string, resource: string): void {
  if (!envHasKey(env, 'DATABASE_URL')) {
    throw new Error(`new provision must set DATABASE_URL:\n${env}`)
  }
  const keys = postgresColorURLKeys(env)
  if (keys.length !== 1) {
    throw new Error(`provision must set exactly one color URL:\n${env}`)
  }
  assertPostgresAppEnvCommon(env, resource)
}

export function assertPostgresAttachEnv(env: string, resource: string): void {
  if (envHasKey(env, 'DATABASE_URL')) {
    throw new Error(`attach of existing resource must not set DATABASE_URL:\n${env}`)
  }
  const keys = postgresColorURLKeys(env)
  if (keys.length !== 1) {
    throw new Error(`attach must set exactly one color URL:\n${env}`)
  }
  assertPostgresAppEnvCommon(env, resource)
}

export function assertPostgresFollowerAppEnv(env: string, resource: string): void {
  if (!envHasKey(env, 'DATABASE_URL')) {
    throw new Error(`primary DATABASE_URL missing after follower:\n${env}`)
  }
  const keys = postgresColorURLKeys(env)
  if (keys.length !== 1) {
    throw new Error(`follower must add exactly one color URL:\n${env}`)
  }
  assertPostgresAppEnvCommon(env, resource)
}

const randomPostgresDB = /^[a-z][a-z0-9]{11}$/

export function assertOneRandomPostgresDatabase(listed: string): void {
  const names = listed.split('\n').map((s) => s.trim()).filter(Boolean)
  if (names.length !== 1) {
    throw new Error(`expected one application database, got ${JSON.stringify(names)}`)
  }
  if (!randomPostgresDB.test(names[0])) {
    throw new Error(`first database name must be random alphanumeric, got ${JSON.stringify(names[0])}`)
  }
}

export function flynnAppListed(name: string): boolean {
  if (!name) return false
  const out = flynn(['apps'], { allowFail: true })
  for (const line of out.split('\n')) {
    const parts = line.trim().split(/\s+/)
    if (parts[parts.length - 1] === name) return true
  }
  return false
}

/** UI delete unlinks immediately; wait until the instance app is gone too. */
export async function waitUntilPostgresInstanceGone(app: string, name: string, timeoutMs = destroy): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const rows = parsePgRows(flynnApp(app, ['pg'], { allowFail: true }))
    if (rows.some((r) => r.name === name)) {
      flynnApp(app, ['resource:remove', name], { timeoutMs: destroy, allowFail: true })
    } else if (!flynnAppListed(name)) {
      return
    } else {
      flynn(['-a', name, 'apps:destroy', '-y'], { timeoutMs: destroy, allowFail: true })
    }
    await sleep(poll)
  }
  throw new Error(`postgres instance ${name} still present after delete`)
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
  const target = resource || defaultPsqlResource(app)
  const args = target
    ? ['pg', 'psql', target, '--', ...extra]
    : ['pg', 'psql', '--', ...extra]
  return flynnApp(app, args, { allowFail: opts?.allowFail })
}

// When more than one postgres resource is attached, pg:psql requires the
// instance name. Prefer the primary so inserts still hit the leader.
function defaultPsqlResource(app: string): string {
  const rows = parsePgRows(flynnApp(app, ['pg'], { allowFail: true }))
  if (rows.length < 2) return ''
  return rows.find((r) => r.role !== 'follower')?.name || ''
}
