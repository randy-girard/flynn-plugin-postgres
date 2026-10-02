package postgres

import (
	"fmt"
	"os"
	"strings"

	discoverd "github.com/randy-girard/flynn/discoverd/client"
)

// NewDiscoverdClient returns a discoverd HTTP client that sends
// DISCOVERD_AUTH_KEY (SEC-003). GitHub-pinned Flynn modules from before
// 2026-09-21 compiled a client with no Auth-Key, so provision failed with
// "valid Auth-Key header or Basic auth password required" even when the
// plugin job env had the key.
func NewDiscoverdClient() *discoverd.Client {
	c := discoverd.NewClient()
	if k := strings.TrimSpace(os.Getenv("DISCOVERD_AUTH_KEY")); k != "" {
		c.Key = k
	}
	return c
}

// WrapDiscoverdAuth restates a discoverd 401 so operators rebuild the plugin
// against current Flynn instead of chasing a missing env var that is already set.
func WrapDiscoverdAuth(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.Contains(msg, "Auth-Key") && !strings.Contains(strings.ToLower(msg), "unauthorized") {
		return err
	}
	return fmt.Errorf("discoverd rejected Auth-Key (rebuild this plugin against current Flynn so the client sends DISCOVERD_AUTH_KEY): %w", err)
}
