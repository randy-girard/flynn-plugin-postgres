# Flynn Postgres plugin

Tenant provider `postgres` at `postgres-plugin.discoverd`. Never call
`postgres-api.discoverd` or copy the platform appliance superuser into tenant env.

Each resource is one app, one volume, one node. Followers are separate resources.
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
