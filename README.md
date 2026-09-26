# Flynn Postgres plugin

[![coverage](.github/badges/coverage.svg)](https://github.com/randy-girard/flynn-plugin-postgres/actions/workflows/ci.yml)

Tenant Postgres for Flynn. Each `flynn resource:add postgres` creates one
isolated instance: its own Flynn app, its own volume, and exactly one Postgres
node. It does not start a sirenia pair.

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
flynn resource:add postgres --follow <resource> --replication streaming
flynn resource:add postgres --follow <resource> --replication logical --runtime perf-l
```

`--as ANALYTICS` sets only `ANALYTICS_URL`. The default name `DATABASE` sets
only `DATABASE_URL`. The same resource can attach to other apps under different
names. Detach removes that one variable. `flynn env:set` of an attached `*_URL`
is rejected until detach.

Inside one instance the owner can add databases and users. Those roles exist
only in that instance. Two resources do not share an app, volume, superuser,
or any credential that can read the other instance.

A follower is a new resource, not an extra node. It copies the leader and stays
caught up. It is read-only. A follower cannot follow another follower. Its
`--as` name does not replace the leader URL. `pg:promote` makes it writable,
ends the follow, and rewrites the primary attachment `*_URL`. The old leader
remains its own resource. `pg:unfollow` stops replication and leaves a
standalone writable copy that no longer receives leader writes.

There is no in-place resize or upgrade. The path is follow, wait, promote.
`streaming` is same-major replication. `logical` is the major-upgrade mode.
Both are recorded on the follower. Runtime sizing is a name string; the
database-runtimes ticket lands later.

| Command | Purpose |
| --- | --- |
| `flynn pg:info` | Leader, followers, and lag |
| `flynn pg:follow` | New read-only follower resource |
| `flynn pg:wait` | Block until lag is zero |
| `flynn pg:promote` | Writable primary; rewrite the primary URL |
| `flynn pg:unfollow` | Stop replication; keep a writable copy |
| `flynn pg:psql` | `psql` against this instance's URL only |

The dashboard pages (overview, databases, users, backup, follow) are served by
this plugin. A signed-in app sees only its own instances.

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
