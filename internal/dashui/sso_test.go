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
	"testing"
	"time"
)

func TestSignedJWTAcceptedWhenJWKSConfigured(t *testing.T) {
	key := useTestJWKS(t)
	claims := map[string]any{
		"aud":    "flynn-plugin-ui",
		"sub":    "user-1",
		"app_id": "demo",
		"email":  "a@b.c",
		"exp":    time.Now().Add(time.Hour).Unix(),
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	req.Header.Set("Authorization", "Bearer "+signSSO(t, key, claims))
	sess, err := FromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if sess.AppID != "demo" || sess.Email != "a@b.c" {
		t.Fatalf("%+v", sess)
	}

	payload, _ := json.Marshal(claims)
	forged := "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	req = httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	if _, err := FromRequest(req); err == nil {
		t.Fatal("forged SSO signature accepted")
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
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(payload)
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
