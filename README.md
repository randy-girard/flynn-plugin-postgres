# Flynn Postgres plugin

[![coverage](.github/badges/coverage.svg)](https://github.com/randy-girard/flynn-plugin-postgres/actions/workflows/ci.yml)

Tenant Postgres for Flynn. Each `flynn resource:add postgres` creates one
isolated instance: its own Flynn app, its own volume, and exactly one Postgres
node. It does not start a sirenia pair. Provision waits until the instance
registers in discoverd (after `initdb`), not until a 30s job-up scale probe.

This is not the platform appliance. That appliance stays on
`postgres-api.discoverd` with provider name `platform-postgres` and is only for
system apps. This plugin registers provider `postgres` at
`postgres-plugin.discoverd`. `flynn resource:add postgres` hits the plugin.
Tenant env never receives the appliance superuser password.

## Install

```text
sudo flynn-host plugin:install postgres
sudo flynn-host plugin:install https://github.com/randy-girard/flynn-plugin-postgres.git
```

## Usage

```text
flynn resource:add postgres
flynn resource:add postgres --as ANALYTICS
flynn resource:add postgres --follow <resource>
flynn resource:add postgres --follow <resource> --runtime perf-l
```

`--as ANALYTICS` sets `ANALYTICS_URL`. A new provision also sets
`DATABASE_URL` when the app does not already have it. Every provision and
attach sets `FLYNN_POSTGRESQL_<COLOR>_URL` unless `--as` names the attachment.
`--as AMBER` sets `FLYNN_POSTGRESQL_AMBER_URL`. Attaching an existing resource
does not set `DATABASE_URL`. The first logical database on
a new instance is a random alphanumeric name. The same resource can attach
to other apps under different names. Detach removes that one variable.
`flynn env:set` of an attached `*_URL` is rejected until detach.

Inside one instance the owner can add databases and users. Those roles exist
only in that instance. Two resources do not share an app, volume, superuser,
or any credential that can read the other instance.

A follower is a new resource, not an extra node. It copies the leader with
streaming replication and must run the same engine version. It is read-only.
A follower cannot follow another follower. Its `--as` name does not replace
the leader URL. `pg:promote` makes it writable, ends the follow, and rewrites
the primary attachment `*_URL`. The old leader remains its own resource.
`pg:unfollow` stops replication and leaves a standalone writable copy that no
longer receives leader writes.

There is no in-place resize or upgrade. `flynn pg:upgrade` (and dashboard
Upgrade) uses logical replication, promotes a new primary, then recreates
each follower against that primary. Runtime sizing is a name string.

The colon form (`flynn pg:psql`) is canonical. The space form (`flynn pg psql`)
is a fallback.

| Command | Purpose |
| --- | --- |
| `flynn pg` / `pg:list` | List attached instances |
| `flynn pg:info` | Show leader, followers, and lag |
| `flynn pg:create <name>` | Create a logical database on this instance |
| `flynn pg:follow` | Create a streaming read-only follower |
| `flynn pg:wait` | Block until follower lag is zero |
| `flynn pg:promote` | Make a follower writable and rewrite the primary URL |
| `flynn pg:unfollow` | Stop replication and leave a standalone writable copy |
| `flynn pg:upgrade` | Follow, wait, promote, then recreate followers |
| `flynn pg:dump` / `pg:restore` | Custom-format dump of this instance |
| `flynn pg:psql` | Open psql against this instance |

The dashboard pages (overview, databases, users, backup, follow) are served by
this plugin. A signed-in app sees only its own instances.

Cluster `flynn-host backup` does **not** include tenant instance volumes
(`pg_dumpall` is the platform appliance only). Use the dashboard Backup page
or `flynn pg:dump` / `flynn pg:restore` against this instance.

## State machine

Unit tests drive an in-memory state machine. They do not start a Postgres
process. `NodePlan` records the one-node scale request a controller would
apply. Catch-up uses a fake lag that a test sets to zero.

Local dashboard development:

```text
docker compose up
```

Open `http://localhost:8091/dashboard/?app_id=demo`. Compose is not part of
plugin install.

## Cluster e2e

Live-cluster coverage for CLI and the dashboard lives in `cluster/`. The web
suite drives Chromium via Playwright. It is not run by `go test ./...`.

```text
cluster/run.sh --cluster local --web-only --headed
```

See `cluster/README.md`.
