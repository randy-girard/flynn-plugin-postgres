# Flynn Postgres plugin

Tenant provider `postgres` at `postgres-plugin.discoverd`. Never call
`postgres-api.discoverd` or copy the platform appliance superuser into tenant env.

Each resource is one app, one volume, one node. Followers are separate resources.
A follower's tenant login is its own read-only role (`ReaderUser`, created on
the leader before the base backup, since replication copies roles); its
`AppUser` stays the replicated leader app role for the plugin and replication.
The store is per API process (web scales to 2), so its database list can be
stale: SQL against an instance uses the instance's own listing, not the store.
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

Dashboard Destroy and Detach (Settings `action=delete` / `action=detach`; `dashres.go`, DATA-08, wired in `dashboard_lifecycle.go`) go through the controller as the dashboard user: `dashUserController(sess.ControllerToken)` (the SSO `controller_token`, else `X-Flynn-Access-Token`; `internal/dashui` copies it into the session), never the plugin's `CONTROLLER_KEY`, and no token is 401. Lookups still use the plugin's client (`appResources`). The plugin also checks the session first: Detach needs Manage or Admin (`dashCanManage`), Destroy needs Admin (`dashCanAdmin`), the owning app (`OwnedByApp`) and `confirm` set to the instance name, and a primary with followers is refused (`postgres.CanDeleteResource` / `followerApps`). Detach only unbinds an attached non-owner (`DeleteResourceApp`); the owner gets 409. Each action also unsets the attachment env on the app (`dashApplyEnv`, like `flynn env:unset`). `dashres.go` is the same in the mysql, mongodb, kafka, clickhouse, redis and postgres plugins; keep the copies in step. Tests (`dashres_test.go`) replace `dashUserController` with a fake for the token `user-token` and check the plugin's own client sees no writes.
