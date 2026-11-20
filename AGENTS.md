# Flynn Postgres plugin

Tenant provider `postgres` at `postgres-plugin.discoverd`. Never call
`postgres-api.discoverd` or copy the platform appliance superuser into tenant env.

Each resource is one app, one volume, one node. Followers are separate resources.
A follower's tenant login is its own read-only role (`ReaderUser`, created on
the leader before the base backup, since replication copies roles); its
`AppUser` stays the replicated leader app role for the plugin and replication.
Tenant resource env never carries `FLYNN_POSTGRES_PRIMARY_URL`.
No in-place resize: pg:upgrade follows, waits, then promotes.

Unit tests are an in-memory state machine. Do not require a running Postgres.

`plugin-build` caches the `img/packages.sh` squashfs layer and Go’s compile
cache under `.plugin-build-cache/` so a Go-only image rebuild does not reinstall
Postgres packages.

```text
GOFLAGS='-mod=mod' go test ./...
```

## Environment

Cluster, test, and CI steps are the `flynn-environment` skill in the sibling Flynn checkout: `../flynn/.cursor/skills/flynn-environment/SKILL.md`. Read it before booting VMs, running e2e, or pushing. This file stays the repo-specific contract.

## Tests come first

A behavior change or bug fix starts with a test that fails for the missing behavior. Run that test, then change the code until it and the rest of the unit tests pass.

## Dashboard SSO

SSO verification is `github.com/randy-girard/flynn/pkg/pluginui` called from `internal/dashui` (set `dashui.PluginName` to the manifest name; a test checks it). Tests that use `DASHBOARD_SSO_OPTIONAL=1` pass permissions in `X-Flynn-Dashboard-Permissions`; write checks refuse an empty set. Register state-changing routes with `wrapWrite` (a test enforces it).
