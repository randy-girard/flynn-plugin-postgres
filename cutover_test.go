package postgres

import "testing"

func TestRewriteAttachmentEnvSwapsHostAndApp(t *testing.T) {
	prev := &Instance{App: "postgresql-basin-73690", ServiceHost: "leader.postgresql-basin-73690.discoverd"}
	next := &Instance{App: "postgresql-orchid-80127", ServiceHost: "leader.postgresql-orchid-80127.discoverd"}
	env := map[string]string{
		"FLYNN_POSTGRES":             prev.App,
		"DATABASE_URL":               "postgres://u:p@leader.postgresql-basin-73690.discoverd:5432/db?sslmode=require",
		"FLYNN_POSTGRESQL_AMBER_URL": "postgres://u:p@leader.postgresql-basin-73690.discoverd:5432/db?sslmode=require",
		"POSTGRES_ROLE":              "primary",
	}
	RewriteAttachmentEnv(env, prev, next)
	if env["FLYNN_POSTGRES"] != next.App {
		t.Fatalf("FLYNN_POSTGRES=%s", env["FLYNN_POSTGRES"])
	}
	if env["DATABASE_URL"] != "postgres://u:p@leader.postgresql-orchid-80127.discoverd:5432/db?sslmode=require" {
		t.Fatalf("DATABASE_URL=%s", env["DATABASE_URL"])
	}
	if env["FLYNN_POSTGRESQL_AMBER_URL"] == env["DATABASE_URL"] && env["FLYNN_POSTGRESQL_AMBER_URL"] == "" {
		t.Fatal("color URL must also move")
	}
	if got := env["FLYNN_POSTGRESQL_AMBER_URL"]; got != env["DATABASE_URL"] {
		t.Fatalf("color URL=%s", got)
	}
}

func TestRewriteAttachmentEnvNilSafe(t *testing.T) {
	RewriteAttachmentEnv(nil, nil, nil)
	RewriteAttachmentEnv(map[string]string{}, nil, &Instance{App: "postgresql-basin-73690"})
}
