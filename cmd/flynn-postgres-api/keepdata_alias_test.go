package main

import (
	"testing"

	postgres "github.com/randy-girard/flynn-plugin-postgres"
)

// NEW-36(d), CORE-37-F1: a *_SERVICE_ALIAS key is a discoverd name claim
// after a keep-service failover, not a data reference, so it must not keep
// the old primary's data (the redis copy already skips it).
func TestReferencesIgnoresServiceAliasOnly(t *testing.T) {
	if References(map[string]string{postgres.EnvServiceAlias: "postgresql-old-12345"}, "postgresql-old-12345") {
		t.Fatal("the service alias is not a data reference")
	}
	env := map[string]string{postgres.EnvServiceAlias: "postgresql-old-12345", "DATABASE_URL": "postgres://u:p@leader.postgresql-old-12345.discoverd:5432/db"}
	if !References(env, "postgresql-old-12345") {
		t.Fatal("an alias must not hide a real reference in another key")
	}
}
