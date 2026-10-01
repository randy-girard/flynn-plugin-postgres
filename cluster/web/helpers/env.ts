export function flynnBin(): string {
  return process.env.FLYNN?.trim() || 'flynn'
}

export function dashboardURL(): string {
  return process.env.DASHBOARD_URL?.trim() || 'https://dashboard.1.localflynn.com'
}

export function dashboardEmail(): string {
  const explicit = process.env.DASHBOARD_EMAIL?.trim()
  if (explicit) return explicit
  try {
    const host = new URL(dashboardURL()).hostname
    const domain = host.replace(/^dashboard\./, '')
    return `admin@${domain}`
  } catch {
    return 'admin@1.localflynn.com'
  }
}

export function dashboardPassword(): string {
  const v = process.env.DASHBOARD_PASSWORD?.trim()
  if (!v) {
    throw new Error('DASHBOARD_PASSWORD is required for the browser suite (cluster/run.sh)')
  }
  return v
}

export function uniqueSuffix(): string {
  return Math.random().toString(36).slice(2, 8)
}
