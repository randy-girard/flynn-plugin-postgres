import { expect, type Page } from '@playwright/test'
import { dashboardEmail, dashboardPassword } from './env'
import { login as loginTimeout } from './timeouts'

export async function login(page: Page): Promise<void> {
  await page.goto('/login')
  await page.waitForLoadState('domcontentloaded')
  if (/\/apps(\/|$)/.test(page.url()) && await page.getByRole('heading', { name: 'Apps' }).isVisible({ timeout: 0 }).catch(() => false)) {
    return
  }
  await expect(page.getByLabel('Email')).toBeVisible()
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
