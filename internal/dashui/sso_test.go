package dashui

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

const testKID = "flynn-dashboard-ec-p256"

func goodClaims() map[string]any {
	return map[string]any{
		"iss":         "flynn-dashboard",
		"aud":         "flynn-plugin-ui",
		"sub":         "user-1",
		"app_id":      "demo",
		"plugin":      PluginName,
		"email":       "a@b.c",
		"permissions": []string{"app:resources:write"},
		"exp":         time.Now().Add(time.Hour).Unix(),
	}
}

func sessionFor(t *testing.T, token string) (*Session, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return FromRequest(req)
}

func TestSignedJWTAcceptedWhenJWKSConfigured(t *testing.T) {
	key := useTestJWKS(t)
	sess, err := sessionFor(t, signSSO(t, key, goodClaims()))
	if err != nil {
		t.Fatal(err)
	}
	if sess.AppID != "demo" || sess.Email != "a@b.c" || sess.Plugin != PluginName {
		t.Fatalf("%+v", sess)
	}

	payload, _ := json.Marshal(goodClaims())
	forged := "eyJhbGciOiJFUzI1NiIsImtpZCI6ImZseW5uLWRhc2hib2FyZC1lYy1wMjU2In0." + base64.RawURLEncoding.EncodeToString(payload) + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := sessionFor(t, forged); err == nil {
		t.Fatal("forged SSO signature accepted")
	}
}

func TestSSORejectsMissingClaimsAndForeignPlugin(t *testing.T) {
	key := useTestJWKS(t)
	cases := map[string]func(m map[string]any){
		"no aud":         func(m map[string]any) { delete(m, "aud") },
		"wrong aud":      func(m map[string]any) { m["aud"] = "other" },
		"no exp":         func(m map[string]any) { delete(m, "exp") },
		"expired":        func(m map[string]any) { m["exp"] = time.Now().Add(-time.Hour).Unix() },
		"no iss":         func(m map[string]any) { delete(m, "iss") },
		"foreign plugin": func(m map[string]any) { m["plugin"] = PluginName + "-other" },
		"no plugin":      func(m map[string]any) { delete(m, "plugin") },
	}
	for name, mut := range cases {
		c := goodClaims()
		mut(c)
		if _, err := sessionFor(t, signSSO(t, key, c)); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
	// A token with an unknown kid is refused even when the signature is valid.
	if _, err := sessionFor(t, signSSOHeader(t, key, map[string]any{"alg": "ES256", "kid": "other"}, goodClaims())); err == nil {
		t.Error("unknown kid accepted")
	}
}

func TestSSOIgnoresQueryAppID(t *testing.T) {
	key := useTestJWKS(t)
	c := goodClaims()
	delete(c, "app_id")
	req := httptest.NewRequest(http.MethodGet, "/dashboard/?app_id=victim", nil)
	req.Header.Set("Authorization", "Bearer "+signSSO(t, key, c))
	if sess, err := FromRequest(req); err == nil {
		t.Fatalf("app_id taken from the query string: %+v", sess)
	}
}

func TestPluginNameMatchesManifest(t *testing.T) {
	raw, err := os.ReadFile("../../flynn-plugin.json")
	if err != nil {
		t.Skip("no manifest next to the module")
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != PluginName {
		t.Fatalf("dashui.PluginName = %q, flynn-plugin.json name = %q", PluginName, m.Name)
	}
}

func useTestJWKS(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	x := make([]byte, 32)
	y := make([]byte, 32)
	key.X.FillBytes(x)
	key.Y.FillBytes(y)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC",
			"crv": "P-256",
			"kid": testKID,
			"x":   base64.RawURLEncoding.EncodeToString(x),
			"y":   base64.RawURLEncoding.EncodeToString(y),
		}}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "")
	t.Setenv("DASHBOARD_SSO_JWKS_URL", srv.URL)
	return key
}

func signSSO(t *testing.T, key *ecdsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	return signSSOHeader(t, key, map[string]any{"alg": "ES256", "typ": "JWT", "kid": testKID}, claims)
}

func signSSOHeader(t *testing.T, key *ecdsa.PrivateKey, hdr, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(hdr)
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}
