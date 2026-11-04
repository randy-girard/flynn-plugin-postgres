package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func init() {
	allowUnauthenticatedAPI = func() bool { return true }
}

func TestPluginAPIRequiresControllerKey(t *testing.T) {
	allowUnauthenticatedAPI = func() bool { return false }
	t.Cleanup(func() { allowUnauthenticatedAPI = func() bool { return true } })
	t.Setenv("CONTROLLER_KEY", "")
	t.Setenv("AUTH_KEY", "")
	h := newHandler(postgres.NewStore())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty key provision %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/databases?id=shop", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty key deprovision %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ping %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/status", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("health must stay public")
	}

	t.Setenv("CONTROLLER_KEY", "test-controller-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing credential %d", rec.Code)
	}
	bad := httptest.NewRequest(http.MethodGet, "/version", nil)
	bad.SetBasicAuth("", "wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key %d", rec.Code)
	}
	ok := httptest.NewRequest(http.MethodGet, "/version", nil)
	ok.Header.Set("Authorization", "Bearer test-controller-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, ok)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed version %d", rec.Code)
	}
	prov := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	prov.SetBasicAuth("", "test-controller-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, prov)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed provision %d", rec.Code)
	}
}
