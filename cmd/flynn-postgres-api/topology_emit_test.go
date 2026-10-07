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

func TestTopologyMessage(t *testing.T) {
	got := topologyMessage("postgresql-basin-73690", "postgresql-upland-22935", "reason=auto")
	if got != "from=postgresql-basin-73690 to=postgresql-upland-22935 reason=auto" {
		t.Fatalf("%q", got)
	}
}
