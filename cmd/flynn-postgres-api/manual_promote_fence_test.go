package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

func manualPromoteFixture(t *testing.T) (*handler, *postgres.Store, *postgres.Instance, *postgres.Instance) {
	t.Helper()
	s := postgres.NewStore()
	leader, _, err := s.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(postgres.ProvisionRequest{App: "shop-a", Follow: leader.App, AutoFailover: true})
	if err != nil {
		t.Fatal(err)
	}
	return newHandler(s), s, leader, fol
}

func stubFence(h *handler, scaleErr error, scaled *[]string) {
	h.jobs = func(string) ([]*ct.Job, error) {
		if len(*scaled) > 0 && scaleErr == nil {
			return nil, nil
		}
		return []*ct.Job{{ReleaseID: "rel-running", State: ct.JobStateUp}}, nil
	}
	h.getRelease = func(string) (*ct.Release, error) { return &ct.Release{ID: "rel-running"}, nil }
	h.scaleRelease = func(app, _ string, _ ct.ScaleOptions) error {
		*scaled = append(*scaled, app)
		return scaleErr
	}
}

func TestManualPromoteRefusesWhenFenceFails(t *testing.T) {
	h, s, leader, fol := manualPromoteFixture(t)
	var scaled []string
	stubFence(h, errors.New("host down"), &scaled)
	req := httptest.NewRequest(http.MethodPost, "/databases/"+fol.ID+"/promote", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatalf("promote succeeded despite fence failure: %s", rec.Body.String())
	}
	got, _ := s.Get(fol.ID)
	if got.Role != postgres.RoleFollower {
		t.Fatalf("follower role %s, want follower", got.Role)
	}
	old, _ := s.Get(leader.ID)
	if old.Role == postgres.RoleDeposed {
		t.Fatal("leader deposed without being fenced")
	}
}

func TestManualPromoteFencesLeaderBeforePromote(t *testing.T) {
	h, s, leader, fol := manualPromoteFixture(t)
	var scaled []string
	stubFence(h, nil, &scaled)
	req := httptest.NewRequest(http.MethodPost, "/databases/"+fol.ID+"/promote", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("promote %d %s", rec.Code, rec.Body.String())
	}
	if len(scaled) == 0 || scaled[0] != leader.App {
		t.Fatalf("fenced %v, want leader %s", scaled, leader.App)
	}
	got, _ := s.Get(fol.ID)
	if got.Role != postgres.RolePrimary {
		t.Fatalf("role %s", got.Role)
	}
}

func TestDashManualPromoteRefusesWhenFenceFails(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	h, s, _, fol := manualPromoteFixture(t)
	var scaled []string
	stubFence(h, errors.New("host down"), &scaled)
	form := strings.NewReader("action=promote&instance=" + fol.App)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/replication", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	req.Header.Set("X-Flynn-Dashboard-Permissions", "app:resources:write")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "Follower promoted") {
		t.Fatalf("dashboard promoted despite fence failure: %s", rec.Body.String())
	}
	got, _ := s.Get(fol.ID)
	if got.Role != postgres.RoleFollower {
		t.Fatalf("role %s", got.Role)
	}
}

// NEW-51: a plain follower's promote is a switchover; the primary is
// stopped first, and a failed stop refuses the promote.
func TestManualPromoteOfPlainFollowerFencesPrimary(t *testing.T) {
	s := postgres.NewStore()
	leader, _, _ := s.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	fol, _, _ := s.Provision(postgres.ProvisionRequest{App: "shop-a", Follow: leader.App})
	h := newHandler(s)
	var scaled []string
	stubFence(h, errors.New("host down"), &scaled)
	req := httptest.NewRequest(http.MethodPost, "/databases/"+fol.ID+"/promote", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 || len(scaled) == 0 || scaled[0] != leader.App {
		t.Fatalf("code %d scaled %v", rec.Code, scaled)
	}
	if got, _ := s.Get(fol.ID); got.Role != postgres.RoleFollower {
		t.Fatalf("role %s", got.Role)
	}
}
