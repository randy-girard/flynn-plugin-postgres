# Cluster e2e (live Flynn)

Tests the postgres plugin against an **already-installed** cluster. They are
not part of `go test ./...` or GitHub Actions. The web suite drives a real
Chromium window through Playwright (login, apps, provision, workspace
Datastores list, databases, users, attach/detach to another app, followers,
backup). The CLI suite shells out to
`flynn`, including `resource:attach` / `resource:detach` on a second app that
must not be able to delete the instance.

## Requirements

- `flynn` on PATH
- postgres plugin installed on **that** Flynn cluster (`flynn plugin`)
- dashboard reachable on that cluster
- `DASHBOARD_PASSWORD` for the browser suite (or `BOOTSTRAP_ADMIN_PASSWORD`
  still on the dashboard app)

The script does not pick Vagrant vs Linode itself. It uses the same cluster
`flynn` would (`flynn cluster:default`), unless you set `FLYNN_CLUSTER` or
pass `--cluster`.

## Run

From the plugin repo:

```text
cluster/run.sh
cluster/run.sh --cluster local --web-only --headed
cluster/run.sh --cli-only
cluster/run.sh --web-only
cluster/run.sh --web-only --headed
```

`--headed` (or `HEADLESS=0`) shows the browser. Local Flynn TLS is accepted
(`ignoreHTTPSErrors`).

| Variable | Default |
| --- | --- |
| `FLYNN` | `flynn` |
| `FLYNN_CLUSTER` / `--cluster` | Flynn's default cluster (`flynn cluster:default`) |
| `DASHBOARD_URL` | dashboard URL for that cluster, else `https://dashboard.1.localflynn.com` |
| `DASHBOARD_EMAIL` | `admin@` + cluster domain |
| `DASHBOARD_PASSWORD` | `BOOTSTRAP_ADMIN_PASSWORD` from `dashboard-plugin` if still set |
| `HEADLESS` | `1` (`0` shows the window) |

CI stay on unit tests. Run this after dashboard or postgres plugin changes
instead of clicking through the UI by hand.

Apps are named `pg-e2e-cli-*` / `pg-e2e-web-*` and destroyed at the end (or
on failure). UI assertions fail in about 8s. Provision and follow are capped
at 90s so a stuck cluster fails the suite instead of hanging for tens of
minutes.

The dashboard has no SQL console. Insert/query and follower catch-up checks
use `flynn pg psql` against the instance the browser just provisioned. A new
provision sets `DATABASE_URL` when that key is free plus exactly one color
URL, and creates exactly one random alphanumeric application database.
Attaching an existing resource to another app sets only the color URL.
