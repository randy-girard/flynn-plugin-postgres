package main

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	hh "github.com/randy-girard/flynn/pkg/httphelper"
)

// pgDatabaseListQuery marks the leader's live database listing in a fake runSQL.
const pgDatabaseListQuery = "FROM pg_database"

func fastReaderRetry(t *testing.T, timeout time.Duration) {
	t.Helper()
	d, to := followerReaderRetryDelay, followerReaderRetryTimeout
	t.Cleanup(func() { followerReaderRetryDelay, followerReaderRetryTimeout = d, to })
	followerReaderRetryDelay = time.Millisecond
	followerReaderRetryTimeout = timeout
}

// CORE-25-F1-F3-F4: a database the tenant dropped with plain SQL (the CLI e2e
// does DROP DATABASE through pg:psql) stays in the plugin's in-memory record.
// The follower grant must use what the leader really has, not that record.
func TestFollowerReaderSkipsDatabaseDroppedOutsideThePlugin(t *testing.T) {
	leader, fol := readerFollowerPair()
	leader.Databases = append(leader.Databases, postgres.Database{Name: "e2e_db_gone"})
	var joined []string
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(connURL, query string) (string, error) {
		_, db := urlUserDB(connURL)
		if strings.Contains(query, pgDatabaseListQuery) {
			return "db_extra\ndb_shop\n", nil
		}
		if strings.Contains(query, "e2e_db_gone") || db == "e2e_db_gone" {
			return "", errors.New(`psql: exit status 1: ERROR:  database "e2e_db_gone" does not exist`)
		}
		joined = append(joined, "["+db+"] "+query)
		return "", nil
	}
	if err := createFollowerReader(leader, fol); err != nil {
		t.Fatalf("follower login must not fail on a database the leader no longer has: %v", err)
	}
	all := strings.Join(joined, "\n")
	for _, needle := range []string{"GRANT CONNECT ON DATABASE db_shop TO ro_fol", "GRANT CONNECT ON DATABASE db_extra TO ro_fol"} {
		if !strings.Contains(all, needle) {
			t.Fatalf("missing %q in %s", needle, all)
		}
	}
}

// A database dropped between the listing and the grant is skipped too.
func TestFollowerReaderSkipsDatabaseDroppedDuringGrant(t *testing.T) {
	leader, fol := readerFollowerPair()
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(connURL, query string) (string, error) {
		_, db := urlUserDB(connURL)
		if strings.Contains(query, pgDatabaseListQuery) {
			return "db_extra\ndb_shop\n", nil
		}
		if strings.Contains(query, "DATABASE db_extra") {
			return "", errors.New(`psql: exit status 1: ERROR:  database "db_extra" does not exist`)
		}
		if db == "db_extra" {
			return "", errors.New(`psql: exit status 2: psql: error: connection to server failed: FATAL:  database "db_extra" does not exist`)
		}
		return "", nil
	}
	if err := createFollowerReader(leader, fol); err != nil {
		t.Fatalf("a database dropped mid-grant must be skipped: %v", err)
	}
}

// The leader can be unreachable for a moment right after it starts or after a
// network blip on its host. The follower login waits for it instead of
// failing the follow outright.
func TestFollowerReaderRetriesWhileLeaderUnreachable(t *testing.T) {
	fastReaderRetry(t, 5*time.Second)
	leader, fol := readerFollowerPair()
	refused := 3
	var created bool
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(_ string, query string) (string, error) {
		if refused > 0 {
			refused--
			return "", errors.New(`psql: exit status 2: psql: error: connection to server at "leader.postgresql-lead-11111.discoverd" (100.100.61.7), port 5432 failed: Connection refused` + "\n\tIs the server running on that host and accepting TCP/IP connections?")
		}
		if strings.Contains(query, pgDatabaseListQuery) {
			return "db_shop\n", nil
		}
		if strings.Contains(query, "CREATE ROLE ro_fol") {
			created = true
		}
		return "", nil
	}
	if err := createFollowerReader(leader, fol); err != nil {
		t.Fatalf("follower login must retry an unreachable leader: %v", err)
	}
	if !created {
		t.Fatal("reader role was not created after the leader came back")
	}
}

// A leader that stays unreachable fails the follow with a retryable 503, not
// a 400 validation_error with retry:false.
func TestFollowerReaderUnreachableLeaderIsRetryable(t *testing.T) {
	fastReaderRetry(t, 20*time.Millisecond)
	leader, fol := readerFollowerPair()
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(string, string) (string, error) {
		return "", errors.New(`psql: exit status 2: psql: error: connection to server at "leader.x.discoverd" (100.100.61.7), port 5432 failed: Connection refused`)
	}
	err := createFollowerReader(leader, fol)
	if err == nil {
		t.Fatal("an unreachable leader must fail the follower login")
	}
	if !errors.Is(err, errLeaderUnavailable) {
		t.Fatalf("want errLeaderUnavailable, got %v", err)
	}
	w := httptest.NewRecorder()
	writeAPIError(w, err)
	if w.Code != 503 {
		t.Fatalf("status %d, want 503: %s", w.Code, w.Body.String())
	}
	var body hh.JSONError
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != hh.ServiceUnavailableErrorCode || !body.Retry {
		t.Fatalf("want service_unavailable retry:true, got %+v", body)
	}
	if !strings.Contains(body.Message, "create follower login") {
		t.Fatalf("message lost its context: %q", body.Message)
	}
}

// SQL errors other than connection failures are not retried.
func TestFollowerReaderDoesNotRetrySQLErrors(t *testing.T) {
	fastReaderRetry(t, 5*time.Second)
	leader, fol := readerFollowerPair()
	calls := 0
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(string, string) (string, error) {
		calls++
		return "", errors.New("psql: exit status 1: ERROR:  permission denied to create role")
	}
	if err := createFollowerReader(leader, fol); err == nil || errors.Is(err, errLeaderUnavailable) {
		t.Fatalf("want a plain error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("a SQL error was retried %d times", calls)
	}
}

// The CLI's pg:follow goes through the controller, which relays the
// provider's JSON error. The follow handler must keep a retryable code.
func TestWriteAPIErrorKeepsRelayedRetryableError(t *testing.T) {
	w := httptest.NewRecorder()
	writeAPIError(w, hh.JSONError{Code: hh.ServiceUnavailableErrorCode, Message: "leader unreachable", Retry: true})
	if w.Code != 503 {
		t.Fatalf("status %d, want 503", w.Code)
	}
	var body hh.JSONError
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if !body.Retry || body.Code != hh.ServiceUnavailableErrorCode {
		t.Fatalf("relayed retryable error lost: %+v", body)
	}
	w = httptest.NewRecorder()
	writeAPIError(w, errors.New("plain"))
	if w.Code != 400 {
		t.Fatalf("plain errors stay validation errors, got %d", w.Code)
	}
}
