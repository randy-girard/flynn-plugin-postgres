package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func TestParseBasebackupRow(t *testing.T) {
	copied, total, phase, ok := parseBasebackupRow("12345|67890|streaming database files\n")
	if !ok || copied != 12345 || total != 67890 || phase != "streaming database files" {
		t.Fatalf("%d %d %q %v", copied, total, phase, ok)
	}
	if _, _, _, ok := parseBasebackupRow(""); ok {
		t.Fatal("empty")
	}
}

func TestHTTPProgressJSON(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID  string            `json:"id"`
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	folReq := httptest.NewRequest(http.MethodPost, "/databases/"+created.ID+"/follow", strings.NewReader(`{"app":"shop"}`))
	folRec := httptest.NewRecorder()
	h.ServeHTTP(folRec, folReq)
	if folRec.Code != 200 {
		t.Fatalf("follow %d %s", folRec.Code, folRec.Body.String())
	}
	var fol struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(folRec.Body.Bytes(), &fol); err != nil {
		t.Fatal(err)
	}
	name := strings.TrimSpace(fol.Env["FLYNN_POSTGRES"])
	if name == "" {
		t.Fatalf("follower env %#v", fol.Env)
	}
	if err := h.store.SetBackupProgress(name, 25, 100); err != nil {
		t.Fatal(err)
	}
	progReq := httptest.NewRequest(http.MethodGet, "/databases/"+name+"/progress", nil)
	progRec := httptest.NewRecorder()
	h.ServeHTTP(progRec, progReq)
	if progRec.Code != 200 {
		t.Fatalf("progress %d %s", progRec.Code, progRec.Body.String())
	}
	var p postgres.ReplicaProgress
	if err := json.Unmarshal(progRec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Phase != postgres.PhaseBasebackup || p.Percent == 0 || p.Ready {
		t.Fatalf("%+v", p)
	}
	if !strings.Contains(p.Message, "%") {
		t.Fatalf("message %q", p.Message)
	}
}

func TestLiveDiscoverdProgressForPrimary(t *testing.T) {
	orig := peekDiscoverdInstances
	t.Cleanup(func() { peekDiscoverdInstances = orig })
	inst := &postgres.Instance{App: "pg-harbor-abcdef"}
	peekDiscoverdInstances = func(service string) bool {
		if service != inst.App {
			t.Fatalf("service %q", service)
		}
		return false
	}
	got := liveDiscoverdProgress(inst)
	if got == nil || got.Ready || got.Phase != postgres.PhaseStarting {
		t.Fatalf("starting: %+v", got)
	}
	peekDiscoverdInstances = func(string) bool { return true }
	got = liveDiscoverdProgress(inst)
	if got == nil || !got.Ready || got.Phase != postgres.PhaseReady {
		t.Fatalf("ready: %+v", got)
	}
}

func TestReplicaCopyProgressIgnoresOtherReplicaLag(t *testing.T) {
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	leader := &postgres.Instance{
		App:         "postgresql-basin-73690",
		AppUser:     "u",
		AppPassword: "p",
		ServiceHost: "leader.postgresql-basin-73690.discoverd",
		Role:        postgres.RolePrimary,
	}
	fol := &postgres.Instance{
		App:         "postgresql-ember-50780",
		AppUser:     "u",
		AppPassword: "p",
		ServiceHost: "leader.postgresql-ember-50780.discoverd",
		Role:        postgres.RoleFollower,
		LeaderID:    "postgresql-basin-73690",
	}
	runSQL = func(connURL, query string) (string, error) {
		if strings.Contains(query, "pg_stat_replication") {
			t.Fatal("must not use another replica's pg_stat_replication row")
		}
		if strings.Contains(connURL, "ember") && strings.Contains(query, "pg_is_in_recovery") {
			return "", errors.New("follower not up")
		}
		if strings.Contains(connURL, "basin") && strings.Contains(query, "pg_stat_progress_basebackup") {
			return "", errors.New("no backup")
		}
		return "", errors.New("unexpected " + query)
	}
	got := replicaCopyProgress(fol, leader)
	if got == nil || got.Ready {
		t.Fatalf("upgrade copy is not ready just because another replica is: %+v", got)
	}
	if got.Phase != postgres.PhaseStarting {
		t.Fatalf("phase %+v", got)
	}
}

func TestReplicaCopyProgressNotReadyWhenWritable(t *testing.T) {
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	leader := &postgres.Instance{
		App:         "postgresql-basin-73690",
		AppUser:     "u",
		AppPassword: "p",
		ServiceHost: "leader.postgresql-basin-73690.discoverd",
		Role:        postgres.RolePrimary,
	}
	fol := &postgres.Instance{
		App:         "postgresql-ember-50780",
		AppUser:     "u",
		AppPassword: "p",
		ServiceHost: "leader.postgresql-ember-50780.discoverd",
		Role:        postgres.RoleFollower,
		LeaderID:    leader.App,
	}
	runSQL = func(connURL, query string) (string, error) {
		if strings.Contains(connURL, "ember") && strings.Contains(query, "pg_is_in_recovery") {
			return "0|0\n", nil
		}
		t.Fatalf("unexpected %s %s", connURL, query)
		return "", nil
	}
	got := replicaCopyProgress(fol, leader)
	if got == nil || got.Ready {
		t.Fatalf("initdb primary must not look caught up: %+v", got)
	}
	if !got.Available {
		t.Fatalf("writable instance must be available on resource pages: %+v", got)
	}
}

func TestReplicaCopyProgressReadyWhenFollowerCaughtUp(t *testing.T) {
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	leader := &postgres.Instance{
		App:         "postgresql-basin-73690",
		AppUser:     "u",
		AppPassword: "p",
		ServiceHost: "leader.postgresql-basin-73690.discoverd",
		Role:        postgres.RolePrimary,
	}
	fol := &postgres.Instance{
		App:         "postgresql-ember-50780",
		AppUser:     "u",
		AppPassword: "p",
		ServiceHost: "leader.postgresql-ember-50780.discoverd",
		Role:        postgres.RoleFollower,
		LeaderID:    leader.App,
	}
	runSQL = func(connURL, query string) (string, error) {
		if strings.Contains(connURL, "ember") && strings.Contains(query, "pg_is_in_recovery") {
			return "1|100\n", nil
		}
		if strings.Contains(connURL, "basin") && strings.Contains(query, "pg_current_wal_lsn") {
			return "100\n", nil
		}
		t.Fatalf("unexpected %s %s", connURL, query)
		return "", nil
	}
	got := replicaCopyProgress(fol, leader)
	if got == nil || !got.Ready || got.Phase != postgres.PhaseReady {
		t.Fatalf("caught up: %+v", got)
	}
}

func TestReplicaCopyProgressLeaderUnreachable(t *testing.T) {
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	leader := &postgres.Instance{
		App:         "postgresql-valley-30620",
		AppUser:     "u",
		AppPassword: "p",
		ServiceHost: "leader.postgresql-valley-30620.discoverd",
		Role:        postgres.RolePrimary,
	}
	fol := &postgres.Instance{
		App:         "postgresql-fjord-67828",
		AppUser:     "u",
		AppPassword: "p",
		ServiceHost: "leader.postgresql-fjord-67828.discoverd",
		Role:        postgres.RoleFollower,
		LeaderID:    leader.App,
	}
	runSQL = func(connURL, query string) (string, error) {
		if strings.Contains(connURL, "fjord") && strings.Contains(query, "pg_is_in_recovery") {
			return "1|100\n", nil
		}
		if strings.Contains(connURL, "valley") {
			return "", errors.New("could not translate host name")
		}
		t.Fatalf("unexpected %s %s", connURL, query)
		return "", nil
	}
	got := replicaCopyProgress(fol, leader)
	if got == nil || got.Ready || !got.Available {
		t.Fatalf("orphan replica: %+v", got)
	}
	if !strings.Contains(got.Message, "unreachable") || !strings.Contains(got.Message, "postgresql-valley-30620") {
		t.Fatalf("message %q", got.Message)
	}
}
