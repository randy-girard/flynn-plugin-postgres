package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func TestPrimaryProvisionPublishesProgressUntilReady(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()
	t.Setenv("DASHBOARD_METRICS_SECRET", "sekrit")
	t.Setenv("DASHBOARD_METRICS_URL", ts.URL+"/webhooks/plugin-metrics")

	store := postgres.NewStore()
	var reads int
	store.SetLiveProgress(func(inst *postgres.Instance) (*postgres.ReplicaProgress, error) {
		reads++
		p := &postgres.ReplicaProgress{
			Follower: inst.App,
			Phase:    postgres.PhaseStarting,
			Percent:  5,
			Message:  "starting",
		}
		if reads > 1 {
			p.Phase = postgres.PhaseReady
			p.Percent = 100
			p.Ready = true
			p.Available = true
			p.Message = "ready"
		}
		return p, nil
	})
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-id", Tenant: "shop-id"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Role == postgres.RoleFollower {
		t.Fatal("expected a primary")
	}
	h := &handler{store: store}
	h.watchCopyProgress(inst)

	deadline := time.Now().Add(3 * time.Second)
	posts := 0
	for time.Now().Before(deadline) && posts < 2 {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		posts = 0
		for _, raw := range bodies {
			if strings.Contains(raw, `"app_id":"shop-id"`) && strings.Contains(raw, `"kind":"progress"`) {
				posts++
			}
		}
		mu.Unlock()
	}
	if posts < 2 {
		t.Fatalf("primary progress published %d time(s), want the starting and ready notices", posts)
	}
}
