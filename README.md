# Flynn Postgres plugin

[![coverage](.github/badges/coverage.svg)](https://github.com/randy-girard/flynn-plugin-postgres/actions/workflows/ci.yml)

Tenant Postgres for Flynn. Each `flynn addons:create postgres` creates one
isolated instance: its own Flynn app, its own volume, and exactly one Postgres
node. It does not start a sirenia pair. `flynn addons:create postgres` returns
after the job is scheduled. The instance finishes `initdb` and registers in
discoverd in the background; check with `flynn pg:wait` or the dashboard.
`flynn addons:destroy` detaches immediately; the isolated instance is
destroyed in the background.

This is not the platform appliance. That appliance stays on
`postgres-api.discoverd` with provider name `platform-postgres` and is only for
system apps. This plugin registers provider `postgres` at
`postgres-plugin.discoverd`. `flynn addons:create postgres` hits the plugin.
Tenant env never receives the appliance superuser password.

## Install

```text
sudo flynn-host plugin:install postgres
sudo flynn-host plugin:install https://github.com/randy-girard/flynn-plugin-postgres.git
```

## Usage

```text
flynn addons:create postgres
flynn addons:create postgres --as ANALYTICS
flynn addons:create postgres --follow <resource>
flynn addons:create postgres --follow <resource> --runtime perf-l
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

No database on an instance lets PUBLIC connect. `flynn pg:create`, the
dashboard Databases tab, and a restore that creates its database revoke
PUBLIC CONNECT right after `CREATE DATABASE` and grant CONNECT to the owner;
if the revoke fails the new database is dropped. A user added with a database
gets CONNECT on that database only. A database the app makes with its own SQL
starts with the Postgres default (PUBLIC may connect) until the instance next
starts: every start sweeps `pg_database`, revokes PUBLIC CONNECT on each
database, and grants CONNECT to its owner and the instance login. After a
crash the sweep waits (up to 10 minutes) while Postgres replays WAL and is not
yet accepting connections, instead of exiting and crash-looping.

`flynn-host postgres:availability` is `manual` or `system` for the whole cluster.
Hosted and self-hosted use the same values. `system` keeps one platform replica
per primary when another host can take the volume. That replica is not an
add-on. `manual` creates nothing extra. Add follower stays available in both
modes. Automatic failover of a user follower applies only while the setting is
`manual`. A cluster admin can set the same value on Cluster → Settings.
After a system failover, user followers stay read-only and keep following
`leader.<original>.discoverd`. A later failover keeps that same name.

Each instance claims `leader.<instance>.discoverd` itself when it starts (a
manual-leader discoverd service). A discoverd peer that has not yet replicated
the new service answers "not manual"; the instance retries that answer for up
to 15 seconds instead of running without a leader name.

A follower is a new resource, not an extra node. It copies the leader with
streaming replication and must run the same engine version. It is read-only.
A follower can only follow a resource owned by or attached to the requesting
app; another tenant's resource fails with "not found", and the controller
ignores a caller-supplied `app` or `tenant`. The follower keeps the leader's app
role credentials, because streaming replication copies the role table. A
follower cannot follow another follower. Its `--as` name does not replace
the leader URL. Promoting an auto-failover follower first fences (scales to 0 and waits for) the
current primary; if the fence fails, the promote is refused and nothing changes.
`pg:promote` makes it writable, ends the follow, and rewrites
the primary attachment `*_URL`. The old leader remains its own resource.
`pg:unfollow` stops replication and leaves a standalone writable copy that no
longer receives leader writes.

There is no in-place resize or upgrade. `flynn pg:upgrade` (and dashboard
Upgrade) uses logical replication, promotes a new primary, then recreates
each follower against that primary. The new follower takes over the old
follower's name and is scaled back up to its original process count; if the
switch fails, both apps are restored to their previous names and scale.
Runtime sizing is a name string.

The colon form (`flynn pg:psql`) is canonical. The space form (`flynn pg psql`)
is a fallback.

| Command | Purpose |
| --- | --- |
| `flynn pg` / `pg:list` | List attached instances |
| `flynn pg:info` | Show leader, followers, lag, and attached apps |
| `flynn pg:create <name>` | Create a logical database on this instance |
| `flynn pg:follow` | Create a streaming read-only follower |
| `flynn pg:wait` | Print live copy progress and block until follower lag is zero |
| `flynn pg:promote` | Make a follower writable and rewrite the primary URL |
| `flynn pg:unfollow` | Stop replication and leave a standalone writable copy |
| `flynn pg:upgrade` | Follow, wait with live progress, promote, then recreate followers |
| `flynn pg:dump` | When remote object storage is configured, write each logical database there and print the timestamp. While the cluster blobstore is still the built-in store, the dashboard Backup tab downloads and restores a file instead. Pass the instance name when the app has more than one Postgres resource |
| `flynn pg:backups [<resource>]` | List backup timestamps. Pass the instance name when the app has more than one Postgres resource |
| `flynn pg:restore [<timestamp>]` | Restore the newest listed timestamp, or the one you name. Pass the instance name before the timestamp when the app has more than one Postgres resource |
| `flynn pg:psql` | Open psql against this instance |

The dashboard pages (overview, metrics, databases, users, backup, follow) are
served by this plugin. A signed-in app sees only its own instances, and every dashboard action that
names an instance (settings delete, replication, users, databases, progress)
resolves it only among that app's own resources; naming another app's instance
is refused. The Metrics
tab charts samples the API posts once a minute
(`active_connections`, `db_size_bytes`, `index_size_bytes`, cache hit rates, `xact_commit`,
`replay_lag_seconds`, `slow_query_count`, and related series). The dashboard
stores every posted series over time so Metrics charts and **Add alert**
threshold previews share the same history. Slow queries
from `pg_stat_statements` (falling back to long-running `pg_stat_activity`
rows) live on the Slow queries tab. **Add alert** on Metrics (and the app
Alerts page) can watch those series when this plugin is installed.

The API sleeps until each minute boundary, collects every instance, and posts
one sample per controller resource to each app the resource is attached to
and to the instance app. `timestamp` is that minute (UTC) and `resource_id` is
the controller resource UUID. An instance with no controller resource posts to
its own app with the instance ID. A failed scrape posts only
`errors=1` and `service_available=0`. Dashboard page views do not post
samples. Both API replicas may post; the dashboard keeps one sample per minute.

### TLS

Each instance has its own private CA. At provision the API issues a server
certificate for `leader.<instance>.discoverd` (and `<instance>.discoverd`,
`localhost`) and stores `POSTGRES_TLS_CA_CERT`, `POSTGRES_TLS_SERVER_CERT` and
`POSTGRES_TLS_SERVER_KEY` on the instance app's release. `start.sh` runs
`flynn-postgres install-tls /data` on every start, so a follower replaces the
files its `pg_basebackup` copied from the primary. The CA key is not kept.

The plugin's own connections (dashboard queries, dump and restore, metrics,
scheduled backups, and the replica checks behind failover) use
`sslmode=verify-full` with that instance's CA as `sslrootcert`, so the server
must present a certificate from that CA that names the dialed host. Tenant
`DATABASE_URL` values keep `sslmode=require`. An instance whose release has no
`POSTGRES_TLS_CA_CERT` (created before this change) keeps its self-signed
certificate and is reached with `sslmode=require`; `pg:maintenance` replaces
it with an instance that has a CA. Whether a CA exists is read from the
controller release, never from the network. When a replacement adopts the
original instance name, the plugin issues a new CA and certificate for that
name.

Those connections never put the instance password on a command line. Every
`psql`, `pg_dump` and `pg_restore` the plugin API runs gets the URL without
its password and the password in `PGPASSWORD`, and error text is redacted.
The dashboard dump and restore target the instance's first database, with the
same `pg_restore --clean --if-exists --exit-on-error` flags as a scheduled
restore; an instance with no database of its own (only the catalog
`postgres`) has nothing to dump or restore from the dashboard.

### Instance metrics endpoint

Every instance release has `FLYNN_METRICS_PORT=9180`. The postgres job serves
`GET /metrics/log` on that port (9180 when unset). The response is
`200 text/plain` with one line, collected on request with a 1.5 s timeout.
Keys that fail to collect are omitted. The port is not a release process port
and tenant jobs cannot reach it; flynn-host can.

```text
flynn-postgres source=postgresql-harbor-12345 addon=res-abc sample#service-available=1 sample#db_size=1024bytes sample#tables=3 sample#active-connections=2 sample#index-cache-hit-rate=0.99000
```

The job does not print its own periodic sample line. Once a minute, on the
minute, flynn-host requests that line and writes one `metrics ...` system line
to the instance app log: the container metrics fields followed by the
`flynn-postgres` fields above. If the request fails, the line has container
fields only.

```text
flynn -a postgresql-harbor-12345 log | grep metrics
```

Cluster `flynn-host backup` does **not** include tenant instance volumes
(`pg_dumpall` is the platform appliance only). Use the dashboard Backup page
or `flynn pg:dump` / `flynn pg:restore` against this instance. Scheduled dumps run once a day, at a fixed time of day per resource (a hash of the resource id). A dump that finishes late does not move the next run: it is due again at the same time the next day, and a dump older than 25 hours is retried at once. After a failed dump the next scheduled try waits for a backoff measured from the failure time: 5 minutes at first, doubling with the time spent failing, up to 6 hours. A database whose name is not a plain identifier is skipped and listed as `skipped` in the dump response and the scheduler log; the other databases are still dumped. The instance login is a member of `pg_read_all_data`, so the dump reads tables owned by every database user.

A restore (dashboard file or `pg:restore`) skips the dump's `EXTENSION` entries
and their comments. Every instance database gets `uuid-ossp`, `pgcrypto` and
`pg_stat_statements` from the OS `postgres` role, so the instance login can
neither drop them (`--clean`) nor comment on them, and `--exit-on-error` would
stop the restore at the first one. Each skipped extension is created with
`CREATE EXTENSION IF NOT EXISTS` first (a no-op when the database has it); one
the login may not create fails the restore only if a restored object needs it.
The restore spools the dump to a temporary file, because it reads the table of
contents before the data.

### Volume size

The `/data` volume is sized from the database runtime's disk (`--runtime`, default `small`; see `flynn-host db-runtime`), or from a raw `--disk` when a cluster admin has run `flynn-host db-runtime:allow-custom`. A raw disk sent without that switch is rejected by the provider too, not only by the CLI. A follower gets its own runtime's disk or its leader's volume size, whichever is larger, so a replica always has room for the leader's data. The size is the volume's ZFS refquota, set when the volume is created. Existing volumes keep their size: instances created before this have the 20 GiB host default. There is no in-place resize.

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

## Image build

`./script/plugin-build` stacks Flynn ubuntu-noble, a cached `img/packages.sh`
layer, and a binaries layer. Repeat builds skip apt while `img/` and the Flynn
OS layer stay the same. `GOCACHE` lives in `.plugin-build-cache/`. Set
`PLUGIN_BUILD_NO_PACKAGES_CACHE=1` to force a packages rebuild.


## Dashboard SSO

The SSO token must be signed with a key in the dashboard JWKS (matching `kid`), carry
`iss=flynn-dashboard`, `aud=flynn-plugin-ui` and an `exp`, and name this plugin
(`plugin=postgres`), so a token handed to another plugin cannot be replayed here
(`github.com/randy-girard/flynn/pkg/pluginui`). Every route that changes state, including replication, settings, the dump and restore, needs `app:resources:write`; a session with no permissions is refused. The dump (`/dashboard/api/dump`) is POST only; GET gets 405. Local tests may set
`DASHBOARD_SSO_OPTIONAL=1` and pass permissions in `X-Flynn-Dashboard-Permissions`.