package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func TestDashBackupDoesNotDownloadAFile(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	if _, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"}); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/backup", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "Back up now") || strings.Contains(body, "Download dump") || strings.Contains(body, `type="file"`) {
		t.Fatalf("backup page %d %s", rec.Code, body)
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard/api/dump", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 || strings.Contains(rec.Body.String(), "-- flynn postgres dump") || rec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("dump endpoint still returned a file: %d %s %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Disposition"))
	}
	req = httptest.NewRequest(http.MethodPost, "/dashboard/api/restore", strings.NewReader("not-a-file"))
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 || rec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("restore endpoint accepted a file: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBackupAPIUsesTheCatalog(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	h.backupAPI = &recordingBackups{}
	req := httptest.NewRequest(http.MethodPost, "/databases/"+inst.App+"/backups/restore", strings.NewReader(`{"timestamp":"../other"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	allowUnauthenticatedAPI = func() bool { return true }
	t.Cleanup(func() { allowUnauthenticatedAPI = func() bool { return true } })
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatalf("raw path restored: %s", rec.Body.String())
	}
}

func TestBackupListJSONUsesLowercaseFields(t *testing.T) {
	store := postgres.NewStore()
	h := newHandler(store)
	h.backupAPI = jsonBackups{}
	allowUnauthenticatedAPI = func() bool { return true }
	t.Cleanup(func() { allowUnauthenticatedAPI = func() bool { return true } })
	req := httptest.NewRequest(http.MethodGet, "/databases/shop-a/backups", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"timestamp":"20261111T160405Z"`) || !strings.Contains(body, `"databases":["appdb"]`) || strings.Contains(body, `"Timestamp"`) {
		t.Fatalf("list %d %s", rec.Code, body)
	}
	if !strings.Contains(body, `"backend":"s3"`) || !strings.Contains(body, `"miss":true`) {
		t.Fatalf("destination %s", body)
	}
}

type jsonBackups struct{}

func (jsonBackups) View(string) (backupView, error) {
	return backupView{
		Destination: postgres.BackupDestination{Backend: "s3", Bucket: "flynn-backups", Allowed: true},
		Miss:        true,
		Backups:     []postgres.BackupDump{{Timestamp: "20261111T160405Z", Databases: []string{"appdb"}}},
	}, nil
}
func (jsonBackups) Create(string) (postgres.BackupResult, error) {
	return postgres.BackupResult{}, nil
}
func (jsonBackups) Restore(string, string) error { return nil }

func TestDumpTaskRejectsAFileFlag(t *testing.T) {
	err := runPluginTask([]string{"dump", "postgresql-oak-10000", "-f", "postgres.dump"})
	if err == nil || !strings.Contains(err.Error(), "blobstore") {
		t.Fatalf("err = %v", err)
	}
}
