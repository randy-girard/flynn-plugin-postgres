import { expect, test, type Page } from '@playwright/test'
import { dashboardPassword, uniqueSuffix } from '../helpers/env'
import {
  assertPostgresAppEnv,
  destroyAppBestEffort,
  envHasKey,
  flynnApp,
  parsePgRows,
  pgPsql,
  postgresColorURLKeys,
  sleep,
  waitForPsql,
  waitUntilPostgresInstanceGone,
} from '../helpers/flynn'
import { login } from '../helpers/login'
import {
  destroy,
  dump as dumpMs,
  follow as followMs,
  optional,
  poll,
  provision,
  replicaWait,
  ui,
} from '../helpers/timeouts'

test.describe('postgres dashboard (live cluster)', () => {
  test.describe.configure({ mode: 'serial' })
  let app = ''
  let peer = ''
  let follower = ''
  const dbName = `e2e_web_${uniqueSuffix()}`
  const userName = `e2e_wu_${uniqueSuffix()}`
  const userPass = `P${uniqueSuffix()}${uniqueSuffix()}`
  const probeN = '42'
  const probeFollow = '99'

  test.beforeAll(() => {
    dashboardPassword()
  })

  test.afterAll(() => {
    destroyAppBestEffort(peer)
    destroyAppBestEffort(app)
  })

  test.beforeEach(async ({ page }) => {
    page.on('console', (msg) => {
      console.log(`[browser:${msg.type()}] ${msg.text()}`)
    })
    page.on('pageerror', (err) => {
      console.log(`[browser:pageerror] ${err.message}`)
    })
    page.on('response', (res) => {
      if (res.status() >= 400 || res.url().includes('/api/plugin-ui/postgres')) {
        console.log(`[browser:http] ${res.status()} ${res.request().method()} ${res.url()}`)
      }
    })
  })

  test.afterEach(async ({ page }, testInfo) => {
    if (testInfo.status === testInfo.expectedStatus) return
    const text = await page.locator('body').innerText().catch(() => '')
    console.log(`--- failed page ${page.url()} ---\n${text.slice(0, 6000)}\n---`)
  })

  test('logs in with a real browser', async ({ page }) => {
    await login(page)
  })

  test('creates an app', async ({ page }) => {
    await login(page)
    app = `pg-e2e-web-${uniqueSuffix()}`
    await page.goto('/apps')
    await page.getByRole('button', { name: 'Add app' }).click()
    const panel = page.getByRole('dialog', { name: 'Add app' })
    await expect(panel).toBeVisible()
    await panel.getByLabel('App name').fill(app)
    await expect(page.getByText(`${app} is available.`)).toBeVisible()
    await panel.getByRole('button', { name: 'Create app' }).click()
    await expect(panel).toBeHidden()
    await expect(page.getByRole('link', { name: app, exact: true })).toBeVisible({ timeout: ui })
    await page.getByRole('link', { name: app, exact: true }).click()
    await expect(page.getByRole('heading', { name: app })).toBeVisible()
  })

  test('provisions postgres', async ({ page }) => {
    test.setTimeout(provision + 30_000)
    await openAppResources(page, app)
    await page.getByRole('button', { name: 'Provision resource' }).click()
    const panel = page.getByRole('dialog', { name: 'Provision resource' })
    await expect(panel).toBeVisible()
    await panel.getByRole('button', { name: 'Plugin' }).click()
    await page.getByRole('option', { name: /Postgres/i }).click()
    await panel.getByRole('button', { name: 'Provision' }).click()
    await page.waitForURL(/\/resources\/postgres\//, { timeout: provision })
    await waitForPsql(app)
    await expect(page.locator('h1')).toContainText(/postgresql-[a-z0-9]+(?:-[a-z0-9]+)*-[0-9]{5,8}|pg-[a-z]+-[a-z]{6,8}/i, { timeout: ui })
  })

  test('creates and lists a logical database', async ({ page }) => {
    await openPostgresTab(page, app, 'Databases')
    await expect(page.getByRole('button', { name: 'Create database' })).toBeVisible()
    await page.getByRole('button', { name: 'Create database' }).click()
    const panel = page.getByRole('dialog', { name: 'Create database' })
    await expect(panel).toBeVisible()
    await panel.locator('#logical-db-name').fill(dbName)
    await panel.getByRole('button', { name: 'Create' }).click()
    await expect(page.locator('.banner-error')).toHaveCount(0)
    await expect(panel).toBeHidden()
    const listed = pgPsql(app, ['-Atc', `SELECT 1 FROM pg_database WHERE datname = '${dbName}'`])
    expect(listed, 'logical database must exist on the instance after Create').toContain('1')
    await page.reload()
    await expect(page.locator('code').filter({ hasText: dbName })).toBeVisible()
  })

  test('adds and removes a user', async ({ page }) => {
    await openPostgresTab(page, app, 'Users')
    const add = page.getByRole('button', { name: 'Add user' })
    await expect(add).toBeEnabled()
    await add.click()
    const panel = page.getByRole('dialog', { name: 'New user' })
    await expect(panel).toBeVisible()
    await panel.getByLabel('Username').fill(userName)
    await panel.getByLabel('Password').fill(userPass)
    const dbTrigger = panel.getByRole('button', { name: 'Database' })
    if (await dbTrigger.isVisible({ timeout: optional }).catch(() => false)) {
      await dbTrigger.click()
      await page.getByRole('option').first().click()
    }
    await panel.getByRole('button', { name: 'Create user' }).click()
    await expect(page.locator('.banner-error')).toHaveCount(0)
    await expect(panel).toBeHidden()
    const listed = pgPsql(app, ['-Atc', `SELECT 1 FROM pg_roles WHERE rolname = '${userName}'`])
    expect(listed, 'login role must exist on the instance after Create user').toContain('1')
    await page.reload()
    const row = page.getByRole('row', { name: new RegExp(userName) })
    await expect(row).toBeVisible()
    page.once('dialog', (d) => d.accept())
    await row.getByRole('button', { name: 'Remove' }).click()
    await expect(row).toHaveCount(0)
  })

  test('inserts data on the primary and reads it back', async () => {
    pgPsql(app, ['-c', 'DROP TABLE IF EXISTS e2e_probe'])
    pgPsql(app, ['-c', 'CREATE TABLE e2e_probe (n int)'])
    pgPsql(app, ['-c', `INSERT INTO e2e_probe VALUES (${probeN})`])
    const got = pgPsql(app, ['-Atc', 'SELECT n FROM e2e_probe']).trim()
    expect(got).toBe(probeN)
  })

  test('attaches existing postgres to another app in the same account', async ({ page }) => {
    test.setTimeout(provision + destroy + 45_000)
    await login(page)
    peer = `pg-e2e-web-peer-${uniqueSuffix()}`
    await page.goto('/apps')
    await page.getByRole('button', { name: 'Add app' }).click()
    const add = page.getByRole('dialog', { name: 'Add app' })
    await expect(add).toBeVisible()
    await add.getByLabel('App name').fill(peer)
    await expect(page.getByText(`${peer} is available.`)).toBeVisible()
    await add.getByRole('button', { name: 'Create app' }).click()
    await expect(add).toBeHidden()

    const rows = parsePgRows(flynnApp(app, ['pg']))
    const primary = rows.find((r) => r.role !== 'follower')?.name
    if (!primary) throw new Error(`no primary postgres resource on ${app}`)

    await page.goto(`/apps/${app}/resources/postgres/${primary}`)
    const shareBtn = page.getByRole('button', { name: 'Attach to another app' })
    await expect(shareBtn).toBeVisible()
    expect(await shareBtn.evaluate((el) => el.closest('.tab-toolbar') !== null)).toBe(true)
    await shareBtn.click()
    const share = page.getByRole('dialog', { name: 'Attach to another app' })
    await expect(share).toBeVisible()
    await share.getByRole('button', { name: 'App' }).click()
    await page.getByRole('option', { name: new RegExp(peer) }).click()
    await share.getByRole('button', { name: 'Attach' }).click()
    await expect(share).toBeHidden({ timeout: ui })

    const attachedEnv = flynnApp(peer, ['env'])
    assertPostgresAppEnv(attachedEnv, primary)
    assertPostgresAppEnv(flynnApp(app, ['env']), primary)
    const peerRead = pgPsql(peer, ['-Atc', 'SELECT n FROM e2e_probe ORDER BY n'], primary).trim()
    expect(peerRead, 'peer app must read the owner instance').toContain(probeN)

    await openPostgresInstanceTab(page, app, primary, 'Settings')
    await expect(page.getByRole('button', { name: 'Delete resource' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Detach' })).toHaveCount(0)

    let removed = false
    try {
      flynnApp(peer, ['resource:remove', primary])
      removed = true
    } catch (err) {
      const msg = String(err).toLowerCase()
      if (!msg.includes('does not own') && !msg.includes('detach')) throw err
    }
    if (removed) throw new Error('attached app must not delete the resource')

    await openPostgresInstanceTab(page, peer, primary, 'Settings')
    await expect(page.getByRole('button', { name: 'Detach' })).toBeEnabled()
    await expect(page.getByRole('button', { name: 'Delete resource' })).toHaveCount(0)
    page.once('dialog', (d) => d.accept())
    await page.getByRole('button', { name: 'Detach' }).click()
    await page.waitForURL(/\/resources\/?$/, { timeout: ui })
    const detachedEnv = flynnApp(peer, ['env'], { allowFail: true })
    if (postgresColorURLKeys(detachedEnv).length !== 0 || envHasKey(detachedEnv, 'DATABASE_URL')) {
      throw new Error(`detach must remove attachment env:\n${detachedEnv}`)
    }
    assertPostgresAppEnv(flynnApp(app, ['env']), primary)

    await openAppResources(page, peer)
    await page.getByRole('button', { name: 'Attach existing' }).click()
    const attach = page.getByRole('dialog', { name: 'Attach existing' })
    await expect(attach).toBeVisible()
    await attach.getByRole('button', { name: 'Postgres resource' }).click()
    await page.getByRole('option', { name: new RegExp(primary) }).click()
    await attach.getByRole('button', { name: 'Attach' }).click()
    await page.waitForURL(new RegExp(`/resources/postgres/${primary}`), { timeout: ui })
    assertPostgresAppEnv(flynnApp(peer, ['env']), primary)

    await openPostgresInstanceTab(page, peer, primary, 'Settings')
    page.once('dialog', (d) => d.accept())
    await page.getByRole('button', { name: 'Detach' }).click()
    await page.waitForURL(/\/resources\/?$/, { timeout: ui })
  })

  test('adds a follower and verifies replication', async ({ page }) => {
    test.setTimeout(followMs + replicaWait * 3 + 30_000)
    const before = new Set(
      parsePgRows(flynnApp(app, ['pg']))
        .filter((r) => r.role === 'follower')
        .map((r) => r.name),
    )
    await openPostgresTab(page, app, 'Followers')
    const addFollower = page.getByRole('button', { name: 'Add follower' })
    await expect(addFollower).toBeEnabled()
    await addFollower.click()
    follower = await waitForNewFollower(app, before)
    await waitForPsql(app, follower, replicaWait)
    let got = await waitForReplicaRow(app, follower, 'SELECT n FROM e2e_probe', (n) => n === probeN)
    expect(got).toBe(probeN)
    pgPsql(app, ['-c', `INSERT INTO e2e_probe VALUES (${probeFollow})`])
    got = await waitForReplicaRow(app, follower, 'SELECT n FROM e2e_probe ORDER BY n', (n) => n.includes(probeFollow))
    expect(got).toContain(probeFollow)
  })

  test('primary settings cannot delete while a follower exists', async ({ page }) => {
    await openPostgresTab(page, app, 'Settings')
    await expect(page.getByRole('heading', { name: 'Settings' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Delete resource' })).toBeDisabled()
    await expect(page.getByText(/still has 1 follower/)).toBeVisible()
  })

  test('downloads a dump from the backup tab', async ({ page }) => {
    await openPostgresTab(page, app, 'Backup')
    await expect(page.getByRole('button', { name: 'Download dump' })).toBeVisible()
    const [download] = await Promise.all([
      page.waitForEvent('download', { timeout: dumpMs }),
      page.getByRole('button', { name: 'Download dump' }).click(),
    ])
    expect(download.suggestedFilename()).toMatch(/\.dump$/i)
    expect(await download.path()).toBeTruthy()
  })

  test('deletes the follower from settings', async ({ page }) => {
    test.setTimeout(destroy * 2 + 30_000)
    if (!follower) throw new Error('expected a follower from the follow test')
    await openPostgresInstanceTab(page, app, follower, 'Settings')
    await expect(page.getByRole('heading', { name: 'Settings' })).toBeVisible()
    const del = page.getByRole('button', { name: 'Delete resource' })
    await expect(del).toBeEnabled()
    page.once('dialog', (d) => d.accept())
    await del.click()
    await page.waitForURL(/\/resources\/?$/, { timeout: destroy })
    await waitUntilPostgresInstanceGone(app, follower)
    follower = ''
  })

  test('deletes the primary from settings', async ({ page }) => {
    test.setTimeout(destroy * 2 + 30_000)
    const rows = parsePgRows(flynnApp(app, ['pg']))
    for (const fol of rows.filter((r) => r.role === 'follower')) {
      await waitUntilPostgresInstanceGone(app, fol.name)
    }
    const primary = parsePgRows(flynnApp(app, ['pg'])).find((r) => r.role !== 'follower')?.name
    if (!primary) throw new Error(`no primary postgres resource on ${app}`)
    await openPostgresInstanceTab(page, app, primary, 'Settings')
    await expect(page.getByRole('heading', { name: 'Settings' })).toBeVisible()
    const del = page.getByRole('button', { name: 'Delete resource' })
    await expect(del).toBeEnabled()
    page.once('dialog', (d) => d.accept())
    await del.click()
    await page.waitForURL(/\/resources\/?$/, { timeout: destroy })
    const deadline = Date.now() + destroy
    while (Date.now() < deadline) {
      const listed = parsePgRows(flynnApp(app, ['pg'], { allowFail: true }))
      if (listed.length === 0) return
      await sleep(poll)
    }
    throw new Error('primary still listed after settings delete')
  })

  test('deletes the app', async ({ page }) => {
    test.setTimeout(destroy * 2 + 15_000)
    await login(page)
    await page.goto(`/apps/${app}/settings`)
    page.once('dialog', (d) => d.accept())
    await page.getByRole('button', { name: 'Delete app' }).click()
    await page.waitForURL(/\/apps\/?$/, { timeout: destroy })
    const deadline = Date.now() + destroy
    while (Date.now() < deadline) {
      await page.reload()
      if ((await page.getByRole('link', { name: app, exact: true }).count()) === 0) {
        app = ''
        return
      }
      await sleep(poll)
    }
    await expect(page.getByRole('link', { name: app, exact: true })).toHaveCount(0)
    app = ''
  })
})

async function openAppResources(page: Page, app: string): Promise<void> {
  await login(page)
  await page.goto('/apps')
  await page.getByRole('link', { name: app, exact: true }).click()
  await page.getByRole('navigation', { name: 'App sections' }).getByRole('link', { name: 'Resources' }).click()
  await expect(page.getByRole('button', { name: 'Provision resource' })).toBeVisible()
}

async function openPostgresInstanceTab(page: Page, app: string, instance: string, tab: string): Promise<void> {
  await login(page)
  await page.goto(`/apps/${app}/resources/postgres/${instance}`)
  const tabs = page.getByRole('navigation', { name: /postgres sections/i })
  await tabs.getByRole('link', { name: tab }).click()
  await expect(tabs.getByRole('link', { name: tab })).toBeVisible()
}

async function openPostgresTab(page: Page, app: string, tab: string): Promise<void> {
  await login(page)
  const rows = parsePgRows(flynnApp(app, ['pg']))
  const primary = rows.find((r) => r.role !== 'follower')?.name
  if (!primary) throw new Error(`no primary postgres resource on ${app}`)
  await openPostgresInstanceTab(page, app, primary, tab)
}

async function waitForNewFollower(appName: string, before: Set<string>): Promise<string> {
  const deadline = Date.now() + followMs
  while (Date.now() < deadline) {
    const rows = parsePgRows(flynnApp(appName, ['pg'], { allowFail: true }))
    const next = rows.find((r) => r.role === 'follower' && !before.has(r.name))
    if (next) return next.name
    await sleep(poll)
  }
  throw new Error('no new follower appeared in flynn pg')
}

async function waitForReplicaRow(
  appName: string,
  follower: string,
  sql: string,
  ok: (got: string) => boolean,
): Promise<string> {
  const deadline = Date.now() + replicaWait
  let last = ''
  while (Date.now() < deadline) {
    try {
      last = pgPsql(appName, ['-Atc', sql], follower).trim()
      if (ok(last)) return last
    } catch (err) {
      last = String(err)
    }
    await sleep(poll)
  }
  throw new Error(`replica ${follower} ${sql}: ${last}`)
}

