import { defineConfig } from '@playwright/test'
import { nav, ui } from './helpers/timeouts'

const baseURL = process.env.DASHBOARD_URL?.trim() || 'https://dashboard.1.localflynn.com'

export default defineConfig({
  testDir: './tests',
  timeout: 3 * 60 * 1000,
  expect: { timeout: ui },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list'], ['html', { open: 'never' }]],
  use: {
    browserName: 'chromium',
    baseURL,
    ignoreHTTPSErrors: true,
    headless: process.env.HEADLESS !== '0',
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
    video: 'off',
    actionTimeout: ui,
    navigationTimeout: nav,
    viewport: { width: 1280, height: 900 },
    launchOptions: process.env.HEADLESS === '0' ? { slowMo: 40 } : undefined,
  },
})
