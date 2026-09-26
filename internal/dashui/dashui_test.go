package dashui

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFromRequestOptionalHeaders(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	req := httptest.NewRequest(http.MethodGet, "/dashboard/?app_id=demo", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "demo")
	req.Header.Set("X-Flynn-Dashboard-User", "u1")
	req.Header.Set("X-Flynn-Dashboard-Base", "/api/plugin-ui/example")
	sess, err := FromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if sess.AppID != "demo" || sess.UserID != "u1" || sess.Base != "/api/plugin-ui/example/" {
		t.Fatalf("%+v", sess)
	}
}

func TestFromRequestJWTWithoutJWKS(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "")
	t.Setenv("DASHBOARD_SSO_JWKS_URL", "")
	payload, _ := json.Marshal(map[string]any{
		"aud":    "flynn-plugin-ui",
		"sub":    "user-1",
		"app_id": "demo",
		"email":  "a@b.c",
		"exp":    time.Now().Add(time.Hour).Unix(),
	})
	tok := "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	sess, err := FromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if sess.AppID != "demo" || sess.Email != "a@b.c" {
		t.Fatalf("%+v", sess)
	}
}

func TestFromRequestRejectsMissingToken(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "")
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	if _, err := FromRequest(req); err == nil {
		t.Fatal("expected error")
	}
}

func TestWriteJSONAndHTML(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, 200, Card{Status: "ok", Attached: true, Summary: "ready", EnvCount: 2})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"attached":true`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	WriteHTML(rec, &Session{AppID: "demo", AppName: "demo", Base: "/dashboard/", Theme: "dark"}, "Overview", `<div class="card">hello</div>`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<base href=\"/dashboard/\">") || !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-theme="dark"`) || !strings.Contains(rec.Body.String(), "Back to app") {
		t.Fatalf("standalone chrome missing: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	WriteHTML(rec, &Session{AppID: "demo", AppName: "demo", Base: "/api/plugin-ui/example/"}, "Overview", `<div class="card">hello</div>`)
	if strings.Contains(rec.Body.String(), "Back to app") || strings.Contains(rec.Body.String(), "<h1>") {
		t.Fatalf("dashboard iframe should not duplicate the SPA heading: %s", rec.Body.String())
	}
}

func TestNavStandaloneVsHosted(t *testing.T) {
	links := [][2]string{{"./", "Overview"}, {"metrics", "Metrics"}}
	got := Nav(&Session{Base: "/dashboard/"}, links...)
	if !strings.Contains(got, "href=\"./\"") || !strings.Contains(got, "metrics") {
		t.Fatalf("standalone nav: %s", got)
	}
	if Nav(&Session{Base: "/api/plugin-ui/example/"}, links...) != "" {
		t.Fatal("hosted pages must not duplicate SPA tabs")
	}
}

func TestDevHandlerRedirectsRoot(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h := DevHandler(inner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/dashboard/?app_id=demo" {
		t.Fatalf("redirect %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard/?app_id=demo", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("pass-through %d %s", rec.Code, rec.Body.String())
	}
}

func TestDevEnabledAndAddr(t *testing.T) {
	t.Setenv("DASHBOARD_DEV", "")
	if DevEnabled() {
		t.Fatal("empty must be off")
	}
	t.Setenv("DASHBOARD_DEV", "1")
	if !DevEnabled() {
		t.Fatal("1 must be on")
	}
	t.Setenv("PORT", "8091")
	if DevAddr() != ":8091" {
		t.Fatalf("addr %s", DevAddr())
	}
}

func TestRequireUnauthorized(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "")
	h := Require(func(w http.ResponseWriter, r *http.Request, s *Session) {
		t.Fatal("should not run")
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestPostMetricsNoPanic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()
	os.Setenv("DASHBOARD_METRICS_URL", ts.URL)
	t.Cleanup(func() { os.Unsetenv("DASHBOARD_METRICS_URL") })
	PostMetrics(MetricEvent{AppID: "demo", Plugin: "example", Series: map[string]float64{"x": 1}})
}
