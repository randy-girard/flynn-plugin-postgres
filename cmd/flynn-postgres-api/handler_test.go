package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func TestHTTPProvisionDoesNotTargetAppliance(t *testing.T) {
	h := newHandler(postgres.NewStore())
	body := []byte(`{"app":"shop","as":"ANALYTICS"}`)
	req := httptest.NewRequest(http.MethodPost, "/databases", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "postgres-api.discoverd") {
		t.Fatalf("response targets appliance: %s", rec.Body.String())
	}
	var out struct {
		Env  map[string]string `json:"env"`
		Plan struct {
			Processes map[string]int `json:"Processes"`
			Sirenia   bool           `json:"Sirenia"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Env["ANALYTICS_URL"] == "" || out.Env["FLYNN_POSTGRES"] == "" || out.Env["POSTGRES_URL"] == "" || len(out.Env) != 3 {
		t.Fatalf("env %#v", out.Env)
	}
	if out.Env["POSTGRES_URL"] != out.Env["ANALYTICS_URL"] {
		t.Fatalf("psql URL %#v", out.Env)
	}
	if out.Plan.Sirenia || out.Plan.Processes["postgres"] != 1 || len(out.Plan.Processes) != 1 {
		t.Fatalf("plan %+v", out.Plan)
	}
	for _, c := range h.store.Contacts() {
		if strings.Contains(c, postgres.PlatformApplianceHost) {
			t.Fatal(c)
		}
	}
}

func TestHTTPProvisionWithoutAppReturnsDatabaseURL(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Env["DATABASE_URL"] == "" || out.Env["FLYNN_POSTGRES"] == "" || out.Env["POSTGRES_URL"] != out.Env["DATABASE_URL"] {
		t.Fatalf("env %#v", out.Env)
	}
	if strings.Contains(out.Env["DATABASE_URL"], "postgres-api.discoverd") {
		t.Fatal(out.Env["DATABASE_URL"])
	}
}

func TestHTTPEnvSetRejected(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create: %s %v", rec.Body.String(), err)
	}
	req = httptest.NewRequest(http.MethodPost, "/databases/"+created.ID+"/env-set", strings.NewReader(`{"app":"shop","vars":{"DATABASE_URL":"postgres://x"}}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatalf("env set should fail: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "attached") {
		t.Fatalf("body: %s", rec.Body.String())
	}
}

func TestHTTPPlatformMarkerRejected(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"platform":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 || !strings.Contains(rec.Body.String(), "postgres-api.discoverd") {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
}
