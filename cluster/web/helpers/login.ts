import { expect, type Page } from '@playwright/test'
import { dashboardEmail, dashboardPassword } from './env'
import { login as loginTimeout } from './timeouts'

export async function login(page: Page): Promise<void> {
  await page.goto('/login')
  await page.waitForLoadState('domcontentloaded')
  const email = page.getByLabel('Email')
  const appsHeading = page.getByRole('heading', { name: 'Apps' })
  // Session cookie: /login redirects to /apps. A second login() in the same
  // test (openPostgresTab then openPostgresInstanceTab) hits that redirect.
  // Wait for either surface; do not match the transient /login URL.
  await expect(email.or(appsHeading)).toBeVisible({ timeout: loginTimeout })
  if (await appsHeading.isVisible().catch(() => false)) {
    return
  }
  if (await page.locator('#handle').isVisible({ timeout: 0 }).catch(() => false)) {
    await page.getByRole('link', { name: /already have an account/i }).click()
    await expect(page.getByLabel('Email')).toBeVisible()
  }
  await page.getByLabel('Email').fill(dashboardEmail())
  await page.getByLabel('Password').fill(dashboardPassword())
  await page.getByRole('button', { name: 'Log in' }).click()
  await page.waitForURL(/\/(apps|change-password)(\/|$)/, { timeout: loginTimeout })
  if (page.url().includes('change-password')) {
    throw new Error('Dashboard requires a password change. Sign in once, then set DASHBOARD_PASSWORD to the current admin password.')
  }
  await expect(page.getByRole('heading', { name: 'Apps' })).toBeVisible({ timeout: loginTimeout })
}

export async function expectAppHeading(page: Page, app: string, timeout?: number): Promise<void> {
  await expect(page.getByRole('heading').filter({ hasText: app }).first()).toBeVisible(
    timeout != null ? { timeout } : undefined,
  )
}
