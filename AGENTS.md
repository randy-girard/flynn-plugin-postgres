# Flynn Postgres plugin

Tenant provider `postgres` at `postgres-plugin.discoverd`. Never call
`postgres-api.discoverd` or copy the platform appliance superuser into tenant env.

Each resource is one app, one volume, one node. Followers are separate resources.
No in-place resize: follow, wait, promote.

Unit tests are an in-memory state machine. Do not require a running Postgres.

```text
GOFLAGS='-mod=mod' go test ./...
```
