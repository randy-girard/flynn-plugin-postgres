package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
)

func TestEmitTopologyWritesIsolatedInstanceLog(t *testing.T) {
	orig := writeResourceLog
	t.Cleanup(func() { writeResourceLog = orig })
	var got []string
	writeResourceLog = func(_ *handler, appRef, line string) {
		got = append(got, appRef+"|"+line)
	}
	h := &handler{}
	inst := &postgres.Instance{App: "postgresql-concave-78237"}
	h.emitTopology(postgres.CodeSwapStarted, inst, inst.App, "", "reason=image-refresh action=create-replica")
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "postgresql-concave-78237|") {
		t.Fatalf("must log on the original instance: %v", got)
	}
	if !strings.Contains(joined, "event#swap-start") || !strings.Contains(joined, "action=create-replica") {
		t.Fatalf("missing swap-start: %v", got)
	}
	if !strings.Contains(joined, "event#note") || !strings.Contains(joined, "Image refresh follower-swap started") {
		t.Fatalf("missing human note: %v", got)
	}
	for _, line := range got {
		if strings.Contains(line, "app-one|") {
			t.Fatalf("must not inject tenant app logs: %v", got)
		}
	}
}

func TestEmitTopologyPostsFlynnEvent(t *testing.T) {
	var got []dashui.FlynnEvent
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev dashui.FlynnEvent
		_ = json.NewDecoder(r.Body).Decode(&ev)
		got = append(got, ev)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("DASHBOARD_EVENTS_URL", ts.URL)
	t.Setenv("DASHBOARD_METRICS_SECRET", "s")
	h := &handler{}
	inst := &postgres.Instance{
		ID:     "res-1",
		App:    "postgresql-upland-22935",
		Tenant: "app-one",
	}
	h.emitTopology(postgres.CodeFailover, inst, "postgresql-basin-73690", inst.App, "")
	if len(got) == 0 {
		t.Fatal("no events posted")
	}
	apps := map[string]bool{}
	for _, ev := range got {
		if ev.Code != postgres.CodeFailover || ev.ProcessType != postgres.ProcessName {
			t.Fatalf("%+v", ev)
		}
		if ev.HostID == "" || ev.EventID == "" {
			t.Fatalf("missing ids %+v", ev)
		}
		apps[ev.AppID] = true
	}
	if !apps["postgresql-upland-22935"] {
		t.Fatalf("apps %v", apps)
	}
}

func followerForRemovalTest(t *testing.T) (http.Handler, string, string, *[]string) {
	t.Helper()
	orig := writeResourceLog
	t.Cleanup(func() { writeResourceLog = orig })
	var got []string
	writeResourceLog = func(_ *handler, appRef, line string) {
		got = append(got, appRef+"|"+line)
	}
	h := newHandler(postgres.NewStore())
	post := func(path string) map[string]string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"app":"shop"}`)))
		if rec.Code != 200 {
			t.Fatalf("%s %d %s", path, rec.Code, rec.Body.String())
		}
		var out struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Env
	}
	leader := post("/databases")["FLYNN_POSTGRES"]
	follower := post("/databases/" + leader + "/follow")["FLYNN_POSTGRES"]
	if leader == "" || follower == "" {
		t.Fatalf("leader %q follower %q", leader, follower)
	}
	got = nil
	return h, leader, follower, &got
}

func assertFollowerRemovedOnPrimary(t *testing.T, got []string, leader, follower, reason string) {
	t.Helper()
	for _, line := range got {
		if strings.HasPrefix(line, leader+"|") && strings.Contains(line, "event#follower-removed") &&
			strings.Contains(line, "to="+follower) && strings.Contains(line, reason) {
			return
		}
	}
	t.Fatalf("primary %s log must record follower %s removal (%s): %v", leader, follower, reason, got)
}

func TestDeleteFollowerLogsOnPrimary(t *testing.T) {
	h, leader, follower, got := followerForRemovalTest(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/databases?id="+follower, nil))
	if rec.Code != 200 {
		t.Fatalf("delete %d %s", rec.Code, rec.Body.String())
	}
	assertFollowerRemovedOnPrimary(t, *got, leader, follower, "reason=deleted")
}

func TestUnfollowLogsOnPrimary(t *testing.T) {
	h, leader, follower, got := followerForRemovalTest(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/databases/"+follower+"/unfollow", strings.NewReader("{}")))
	if rec.Code != 200 {
		t.Fatalf("unfollow %d %s", rec.Code, rec.Body.String())
	}
	assertFollowerRemovedOnPrimary(t, *got, leader, follower, "reason=unfollow")
}

func TestDeletePrimaryDoesNotLogFollowerRemoved(t *testing.T) {
	orig := writeResourceLog
	t.Cleanup(func() { writeResourceLog = orig })
	var got []string
	writeResourceLog = func(_ *handler, appRef, line string) {
		got = append(got, appRef+"|"+line)
	}
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	newHandler(store).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/databases?id="+inst.App, nil))
	if rec.Code != 200 {
		t.Fatalf("delete %d %s", rec.Code, rec.Body.String())
	}
	for _, line := range got {
		if strings.Contains(line, "follower-removed") {
			t.Fatalf("deleting a primary is not a follower removal: %v", got)
		}
	}
}

func TestTopologyMessage(t *testing.T) {
	got := topologyMessage("postgresql-basin-73690", "postgresql-upland-22935", "reason=auto")
	if got != "from=postgresql-basin-73690 to=postgresql-upland-22935 reason=auto" {
		t.Fatalf("%q", got)
	}
}
