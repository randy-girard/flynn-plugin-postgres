package dashui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireWriteRefusesWithoutWritePermission(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	t.Setenv("DASHBOARD_DEV", "")
	ran := 0
	h := RequireWrite(func(http.ResponseWriter, *http.Request, *Session) { ran++ })
	for _, perms := range []string{"", "app:resources:read", "app:logs:read,app:overview:read"} {
		req := httptest.NewRequest(http.MethodPost, "/dashboard/x", nil)
		req.Header.Set("X-Flynn-Dashboard-App", "demo")
		if perms != "" {
			req.Header.Set("X-Flynn-Dashboard-Permissions", perms)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || ran != 0 {
			t.Fatalf("perms %q: code %d ran %d", perms, rec.Code, ran)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/x", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "demo")
	req.Header.Set("X-Flynn-Dashboard-Permissions", "app:resources:write")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || ran != 1 {
		t.Fatalf("write permission: code %d ran %d", rec.Code, ran)
	}
	// No identity at all is still a 401.
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dashboard/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
}
