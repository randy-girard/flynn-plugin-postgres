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
