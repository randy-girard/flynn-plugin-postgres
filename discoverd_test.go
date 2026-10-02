package postgres

import (
	"errors"
	"strings"
	"testing"
)

func TestWrapDiscoverdAuthPassthrough(t *testing.T) {
	if WrapDiscoverdAuth(nil) != nil {
		t.Fatal("nil")
	}
	err := errors.New("connection refused")
	if WrapDiscoverdAuth(err) != err {
		t.Fatal("non-auth errors must stay unchanged")
	}
}

func TestWrapDiscoverdAuthUnauthorized(t *testing.T) {
	err := WrapDiscoverdAuth(errors.New("unauthorized: valid Auth-Key header or Basic auth password required"))
	if err == nil || !strings.Contains(err.Error(), "rebuild this plugin") {
		t.Fatalf("got %v", err)
	}
}

func TestNewDiscoverdClientSetsKeyFromEnv(t *testing.T) {
	t.Setenv("DISCOVERD_AUTH_KEY", "disc-secret")
	t.Setenv("DISCOVERD", "http://127.0.0.1:1111")
	c := NewDiscoverdClient()
	if c == nil || c.Key != "disc-secret" {
		t.Fatalf("Key=%q", c.Key)
	}
}
