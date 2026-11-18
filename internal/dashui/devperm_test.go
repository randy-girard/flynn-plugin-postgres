package dashui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDevSessionPermissionsAreExplicit(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	t.Setenv("DASHBOARD_DEV", "")
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "demo")
	sess, err := FromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Permissions) != 0 {
		t.Fatalf("header-only session got permissions %v", sess.Permissions)
	}
	req.Header.Set("X-Flynn-Dashboard-Permissions", "app:resources:read, app:resources:write")
	sess, _ = FromRequest(req)
	if len(sess.Permissions) != 2 || sess.Permissions[1] != "app:resources:write" {
		t.Fatalf("%v", sess.Permissions)
	}
	t.Setenv("DASHBOARD_DEV", "1")
	req.Header.Del("X-Flynn-Dashboard-Permissions")
	sess, _ = FromRequest(req)
	if len(sess.Permissions) != 1 || sess.Permissions[0] != "*" {
		t.Fatalf("%v", sess.Permissions)
	}
}
