package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func stubIsolationPsql(t *testing.T, fn func(psql, sql string) error) {
	t.Helper()
	orig := runIsolationPsql
	origDelay, origTimeout := isolationRetryDelay, isolationRetryTimeout
	t.Cleanup(func() {
		runIsolationPsql = orig
		isolationRetryDelay, isolationRetryTimeout = origDelay, origTimeout
	})
	isolationRetryDelay = time.Millisecond
	isolationRetryTimeout = time.Second
	t.Setenv("POSTGRES_USER", "app_abc")
	t.Setenv("POSTGRES_DB", "db_abc")
	runIsolationPsql = fn
}

// After a crash the port opens while Postgres still replays WAL. The
// isolation sweep then hit "not yet accepting connections", start exited,
// and the instance crash-looped on every restart.
func TestEnsureConnectIsolationWaitsForCrashRecovery(t *testing.T) {
	calls := 0
	stubIsolationPsql(t, func(psql, sql string) error {
		calls++
		if calls <= 2 {
			return fmt.Errorf(`exit status 2: psql: error: connection to server on socket "/tmp/.s.PGSQL.5432" failed: FATAL:  the database system is not yet accepting connections`)
		}
		if !strings.Contains(sql, "FROM pg_database") {
			t.Fatalf("isolation SQL lost the sweep: %s", sql)
		}
		return nil
	})
	if err := ensureConnectIsolation("/usr/lib/postgresql/16/bin/postgres"); err != nil {
		t.Fatalf("isolation failed while Postgres was still recovering: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls %d, want 3", calls)
	}
}

func TestEnsureConnectIsolationDoesNotRetrySQLErrors(t *testing.T) {
	calls := 0
	stubIsolationPsql(t, func(psql, sql string) error {
		calls++
		return fmt.Errorf(`exit status 3: ERROR:  syntax error at or near "DO"`)
	})
	if err := ensureConnectIsolation("/usr/lib/postgresql/16/bin/postgres"); err == nil {
		t.Fatal("expected the SQL error")
	}
	if calls != 1 {
		t.Fatalf("a SQL error was retried %d times", calls)
	}
}

func TestEnsureConnectIsolationGivesUpAfterTimeout(t *testing.T) {
	stubIsolationPsql(t, func(psql, sql string) error {
		return fmt.Errorf(`FATAL:  the database system is starting up`)
	})
	isolationRetryTimeout = 20 * time.Millisecond
	if err := ensureConnectIsolation("/usr/lib/postgresql/16/bin/postgres"); err == nil {
		t.Fatal("expected an error after the recovery wait timed out")
	}
}
