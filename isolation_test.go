package postgres

import (
	"strings"
	"testing"
)

func TestConnectIsolationSQL(t *testing.T) {
	got, err := ConnectIsolationSQL("app_abc", "db_abc")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		`REVOKE CONNECT ON DATABASE postgres FROM PUBLIC`,
		`REVOKE CONNECT ON DATABASE template1 FROM PUBLIC`,
		`REVOKE CONNECT ON DATABASE db_abc FROM PUBLIC`,
		`GRANT CONNECT ON DATABASE db_abc TO app_abc`,
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("missing %q in %s", needle, got)
		}
	}
}

func TestConnectIsolationSQLRejectsBadIdent(t *testing.T) {
	if _, err := ConnectIsolationSQL("app;drop", "db_abc"); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidDatabaseName(t *testing.T) {
	if err := ValidDatabaseName("shop_analytics"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "bad-name", "1shop", "postgres", "template1", "drop;me"} {
		if err := ValidDatabaseName(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestCreateDatabaseSQL(t *testing.T) {
	got, err := CreateDatabaseSQL("shop_analytics")
	if err != nil || len(got) == 0 || got[0] != "CREATE DATABASE shop_analytics" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := CreateDatabaseSQL("bad-name"); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidUserName(t *testing.T) {
	if err := ValidUserName("app_reader"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "bad-name", "1user", "postgres", "pg_signal_backend", "drop;me"} {
		if err := ValidUserName(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestCreateUserSQL(t *testing.T) {
	got, err := CreateUserSQL("app_reader", "s'ecret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "CREATE ROLE app_reader LOGIN") || !strings.Contains(got, "PASSWORD 's''ecret'") {
		t.Fatalf("%q", got)
	}
	if _, err := CreateUserSQL("app_reader", ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestDropOwnedSQL(t *testing.T) {
	got, err := DropOwnedSQL("app_reader")
	if err != nil || got != "DROP OWNED BY app_reader CASCADE" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestRevokeUserDatabaseSQL(t *testing.T) {
	got, err := RevokeUserDatabaseSQL("app_reader", "appdb")
	if err != nil || got != "REVOKE ALL ON DATABASE appdb FROM app_reader" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestCleanupUserSQL(t *testing.T) {
	got, err := CleanupUserSQL("app_reader")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"REVOKE ALL ON SCHEMA public FROM app_reader",
		"GRANT app_reader TO CURRENT_USER",
		"DROP OWNED BY app_reader CASCADE",
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM app_reader",
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("missing %q in %s", needle, got)
		}
	}
}

func TestCreateDatabaseSQLRevokesPublicConnect(t *testing.T) {
	got, err := CreateDatabaseSQL("shop_analytics")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CREATE DATABASE shop_analytics",
		"REVOKE CONNECT ON DATABASE shop_analytics FROM PUBLIC",
		"GRANT CONNECT ON DATABASE shop_analytics TO CURRENT_USER",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("CreateDatabaseSQL statements:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, stmt := range got {
		if strings.Contains(stmt, ";") {
			t.Fatalf("CREATE DATABASE cannot share a multi-statement string: %q", stmt)
		}
	}
}

func TestConnectIsolationSQLSweepsEveryDatabase(t *testing.T) {
	got, err := ConnectIsolationSQL("app_abc", "db_abc")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		`FROM pg_database`,
		`REVOKE CONNECT ON DATABASE %I FROM PUBLIC`,
		`GRANT CONNECT ON DATABASE %I TO %I`,
		`pg_get_userbyid(datdba)`,
		`GRANT CONNECT ON DATABASE %I TO app_abc`,
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("start-up sweep missing %q in %s", needle, got)
		}
	}
	if strings.Contains(got, "template0") && !strings.Contains(got, "datallowconn") {
		t.Fatalf("sweep must skip databases that refuse connections: %s", got)
	}
}
