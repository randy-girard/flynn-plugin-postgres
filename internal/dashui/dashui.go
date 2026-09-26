// Package dashui is the copy-paste kit for a Flynn plugin dashboard UI.
// Serve it under the manifest dashboard.base_url path (usually /dashboard).
// Copy this directory into each plugin; do not import this template module.
package dashui

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Session is the signed-in dashboard user scoped to one app.
type Session struct {
	UserID      string
	Email       string
	AppID       string
	AppName     string
	Plugin      string
	Permissions []string
	ResourceID  string
	Base        string
	Theme       string
}

// Card is GET /card JSON for the Resources tab tile.
type Card struct {
	Status   string            `json:"status"`
	Attached bool              `json:"attached"`
	Summary  string            `json:"summary"`
	EnvCount int               `json:"env_count"`
	Details  map[string]string `json:"details,omitempty"`
}

// MetricEvent is one per-app datastore sample for the dashboard.
type MetricEvent struct {
	AppID      string             `json:"app_id"`
	ResourceID string             `json:"resource_id,omitempty"`
	Plugin     string             `json:"plugin"`
	Timestamp  time.Time          `json:"timestamp"`
	JobID      string             `json:"job_id,omitempty"`
	Series     map[string]float64 `json:"series"`
}

// FromRequest authenticates a dashboard SSO JWT or optional dev headers.
func FromRequest(r *http.Request) (*Session, error) {
	base := strings.TrimSpace(r.Header.Get("X-Flynn-Dashboard-Base"))
	if base == "" {
		base = "/dashboard/"
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	theme := requestTheme(r)
	if optionalSSO() {
		if app := strings.TrimSpace(r.Header.Get("X-Flynn-Dashboard-App")); app != "" {
			return &Session{
				UserID: strings.TrimSpace(first(r.Header.Get("X-Flynn-Dashboard-User"), "dev")),
				Email:  strings.TrimSpace(r.Header.Get("X-Flynn-Dashboard-Email")),
				AppID:  app,
				Plugin: strings.TrimSpace(r.Header.Get("X-Flynn-Dashboard-Plugin")),
				Base:   base,
				Theme:  theme,
			}, nil
		}
		if app := strings.TrimSpace(r.URL.Query().Get("app_id")); app != "" {
			return &Session{UserID: "dev", AppID: app, Base: base, Theme: theme}, nil
		}
	}
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		return nil, fmt.Errorf("missing dashboard SSO token")
	}
	sess, err := parseJWT(token)
	if err != nil {
		return nil, err
	}
	sess.Base = base
	sess.Theme = theme
	if sess.AppID == "" {
		sess.AppID = strings.TrimSpace(r.URL.Query().Get("app_id"))
	}
	if sess.AppID == "" {
		return nil, fmt.Errorf("SSO token missing app_id")
	}
	return sess, nil
}

func optionalSSO() bool {
	v := strings.TrimSpace(os.Getenv("DASHBOARD_SSO_OPTIONAL"))
	return v == "1" || strings.EqualFold(v, "true")
}

func bearer(h string) string {
	h = strings.TrimSpace(h)
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func first(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func requestTheme(r *http.Request) string {
	t := strings.ToLower(strings.TrimSpace(first(r.URL.Query().Get("theme"), r.Header.Get("X-Flynn-Dashboard-Theme"))))
	if t == "light" || t == "dark" {
		return t
	}
	return ""
}

func parseJWT(raw string) (*Session, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid SSO JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid SSO JWT payload")
	}
	var claims struct {
		Aud         string   `json:"aud"`
		Exp         int64    `json:"exp"`
		Sub         string   `json:"sub"`
		Email       string   `json:"email"`
		AppID       string   `json:"app_id"`
		AppName     string   `json:"app_name"`
		Plugin      string   `json:"plugin"`
		Permissions []string `json:"permissions"`
		ResourceID  string   `json:"resource_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("invalid SSO JWT claims")
	}
	if claims.Aud != "" && claims.Aud != "flynn-plugin-ui" {
		return nil, fmt.Errorf("unexpected SSO audience")
	}
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, fmt.Errorf("SSO token expired")
	}
	if jwksURL := strings.TrimSpace(os.Getenv("DASHBOARD_SSO_JWKS_URL")); jwksURL != "" {
		if err := verifyES256(jwksURL, parts[0]+"."+parts[1], parts[2]); err != nil {
			return nil, err
		}
	}
	return &Session{
		UserID:      claims.Sub,
		Email:       claims.Email,
		AppID:       claims.AppID,
		AppName:     claims.AppName,
		Plugin:      claims.Plugin,
		Permissions: claims.Permissions,
		ResourceID:  claims.ResourceID,
	}, nil
}

func verifyES256(jwksURL, signingInput, sigB64 string) error {
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != 64 {
		return fmt.Errorf("invalid SSO signature")
	}
	req, err := http.NewRequest(http.MethodGet, jwksURL, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("jwks: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc.Keys) == 0 {
		return fmt.Errorf("jwks: invalid document")
	}
	key := doc.Keys[0]
	xb, err := base64.RawURLEncoding.DecodeString(key.X)
	if err != nil {
		return err
	}
	yb, err := base64.RawURLEncoding.DecodeString(key.Y)
	if err != nil {
		return err
	}
	pub := ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}
	sum := sha256.Sum256([]byte(signingInput))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&pub, sum[:], r, s) {
		return fmt.Errorf("SSO signature mismatch")
	}
	return nil
}

// WriteJSON writes v as application/json.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// AppHref is the dashboard SPA URL for the session app (open with target=_parent).
func AppHref(sess *Session) string {
	if sess == nil {
		return "/apps"
	}
	id := first(sess.AppName, sess.AppID)
	if id == "" {
		return "/apps"
	}
	return "/apps/" + url.PathEscape(id)
}

func hostedInDashboard(sess *Session) bool {
	return sess != nil && strings.Contains(sess.Base, "/api/plugin-ui/")
}

// Hosted is true when the Flynn dashboard SPA embeds this page in an iframe.
func Hosted(sess *Session) bool {
	return hostedInDashboard(sess)
}

// Nav is section links for standalone / docker-compose viewing. The dashboard
// SPA already owns tabs, so this is empty when Hosted.
func Nav(sess *Session, items ...[2]string) string {
	if Hosted(sess) || len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<nav class="row muted" aria-label="Add-on sections">`)
	for i, it := range items {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, `<a href="%s">%s</a>`, html.EscapeString(it[0]), html.EscapeString(it[1]))
	}
	b.WriteString(`</nav>`)
	return b.String()
}

// DevEnabled is set by local docker compose (never by flynn-plugin.json).
func DevEnabled() bool {
	v := strings.TrimSpace(os.Getenv("DASHBOARD_DEV"))
	return v == "1" || strings.EqualFold(v, "true")
}

// DevAddr is the HTTP listen address for docker-compose dashboard-dev.
func DevAddr() string {
	p := strings.TrimSpace(os.Getenv("PORT"))
	if p == "" {
		return ":8090"
	}
	if strings.HasPrefix(p, ":") {
		return p
	}
	return ":" + p
}

// DevHandler redirects `/` to the overview with a demo app_id so a browser can
// open the UI without SSO headers. Production plugin binaries ignore this.
func DevHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/dashboard/?app_id=demo", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WriteHTML writes a page that matches Flynn dashboard chrome.
// When the dashboard hosts this UI in an iframe it omits the extra heading so
// the SPA can own the page title and back-link.
func WriteHTML(w http.ResponseWriter, sess *Session, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	base := "/"
	theme := ""
	kicker := ""
	if sess != nil {
		base = html.EscapeString(sess.Base)
		theme = sess.Theme
		if !hostedInDashboard(sess) {
			app := html.EscapeString(first(sess.AppName, sess.AppID))
			kicker = fmt.Sprintf(
				`<header class="plugin-ui-head"><h1>%s</h1><p class="muted"><a href="%s" target="_parent">Back to app</a> · <code>%s</code></p></header>`,
				html.EscapeString(title), html.EscapeString(AppHref(sess)), app,
			)
		}
	}
	attr := ""
	if theme != "" {
		attr = ` data-theme="` + html.EscapeString(theme) + `"`
	}
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html%s>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<base href="%s">
<title>%s</title>
<style>
:root {
  color-scheme: dark;
  --font-sans: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  --font-mono: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  --radius: 6px;
  --color-bg: #121018;
  --color-surface: #1b1824;
  --color-surface-hover: #242030;
  --color-border: #2d2838;
  --color-border-strong: #3d3750;
  --color-text: #f4f2f8;
  --color-text-muted: #a39bb3;
  --color-primary: #b4a6ff;
  --color-on-primary: #16131e;
  --color-danger: #ff6b7a;
  --color-danger-soft: rgba(255,107,122,.12);
  --color-success: #3dd68c;
  --shadow-sm: 0 1px 2px rgba(0,0,0,.28);
}
@media (prefers-color-scheme: light) {
  :root:not([data-theme="dark"]) {
    color-scheme: light;
    --color-bg: #f4f6f9;
    --color-surface: #ffffff;
    --color-surface-hover: #eef1f6;
    --color-border: #e4e0d9;
    --color-border-strong: #d4cfc6;
    --color-text: #1f1b2d;
    --color-text-muted: #5f586c;
    --color-primary: #5341d6;
    --color-on-primary: #ffffff;
    --color-danger: #c23b4e;
    --color-danger-soft: rgba(194,59,78,.10);
    --color-success: #1a8f5c;
    --shadow-sm: 0 1px 2px rgba(31,27,45,.05);
  }
}
html[data-theme="light"] {
  color-scheme: light;
  --color-bg: #f4f6f9;
  --color-surface: #ffffff;
  --color-surface-hover: #eef1f6;
  --color-border: #e4e0d9;
  --color-border-strong: #d4cfc6;
  --color-text: #1f1b2d;
  --color-text-muted: #5f586c;
  --color-primary: #5341d6;
  --color-on-primary: #ffffff;
  --color-danger: #c23b4e;
  --color-danger-soft: rgba(194,59,78,.10);
  --color-success: #1a8f5c;
  --shadow-sm: 0 1px 2px rgba(31,27,45,.05);
}
html, body { margin: 0; background: var(--color-bg); color: var(--color-text); font-family: var(--font-sans); line-height: 1.5; }
body.plugin-ui { padding: 0 0 1.5rem; }
.plugin-ui-head { margin: 0 0 1.15rem; }
.plugin-ui-head h1 { font-size: 1.05rem; font-weight: 650; margin: 0 0 .35rem; letter-spacing: -0.01em; }
.muted { color: var(--color-text-muted); font-size: .85rem; }
.card { background: var(--color-surface); border: 1px solid var(--color-border); border-radius: var(--radius); padding: 1.05rem 1.15rem; margin: 0 0 1rem; box-shadow: var(--shadow-sm); }
.card h2 { font-size: .92rem; font-weight: 650; margin: 0 0 .65rem; }
.row { display: flex; gap: .6rem; flex-wrap: wrap; align-items: center; }
nav.row { margin: 0 0 1rem; }
label { display: block; font-size: .8rem; font-weight: 600; margin: .45rem 0 .2rem; }
input, textarea, select {
  width: 100%%; max-width: 40rem; font: inherit; color: var(--color-text);
  background: var(--color-bg); border: 1px solid var(--color-border); border-radius: 4px; padding: .45rem .6rem;
}
button, .btn {
  font: inherit; font-weight: 600; font-size: .875rem; padding: .45rem .9rem; border-radius: 4px; cursor: pointer;
  background: var(--color-surface); color: var(--color-text); border: 1px solid var(--color-border); box-shadow: var(--shadow-sm);
}
button:hover, .btn:hover { background: var(--color-surface-hover); border-color: var(--color-border-strong); }
button.primary, .btn-primary { background: var(--color-primary); color: var(--color-on-primary); border-color: var(--color-primary); }
button.danger, .btn-danger { color: var(--color-danger); border-color: var(--color-danger); background: var(--color-danger-soft); }
table { width: 100%%; border-collapse: collapse; }
th, td { text-align: left; padding: .45rem .4rem; border-bottom: 1px solid var(--color-border); font-size: .9rem; vertical-align: top; }
th { color: var(--color-text-muted); font-size: .75rem; font-weight: 650; text-transform: uppercase; letter-spacing: .04em; }
code, pre { font-family: var(--font-mono); font-size: .85rem; }
pre { white-space: pre-wrap; overflow: auto; }
a { color: var(--color-primary); }
.banner { padding: .65rem .8rem; border-radius: var(--radius); background: var(--color-danger-soft); color: var(--color-danger); border: 1px solid var(--color-danger); margin-bottom: 1rem; }
.ok { color: var(--color-success); }
.pill { display: inline-block; font-size: .72rem; font-weight: 650; letter-spacing: .03em; text-transform: uppercase; padding: .12rem .45rem; border-radius: 999px; border: 1px solid var(--color-border); color: var(--color-text-muted); }
.mono { font-family: var(--font-mono); font-size: .85rem; }
</style>
</head>
<body class="plugin-ui">
%s
%s
</body></html>`, attr, base, html.EscapeString(title), kicker, body)
}

// Require is middleware that attaches *Session or writes 401.
func Require(next func(http.ResponseWriter, *http.Request, *Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := FromRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next(w, r, sess)
	}
}

// PostMetrics best-effort posts a sample to the dashboard plugin-metrics webhook.
func PostMetrics(ev MetricEvent) {
	url := strings.TrimSpace(os.Getenv("DASHBOARD_METRICS_URL"))
	if url == "" {
		url = "http://dashboard.discoverd/webhooks/plugin-metrics"
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s := strings.TrimSpace(os.Getenv("DASHBOARD_METRICS_SECRET")); s != "" {
		req.Header.Set("X-Flynn-Webhook-Secret", s)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
}
