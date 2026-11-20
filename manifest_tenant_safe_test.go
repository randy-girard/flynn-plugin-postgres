package postgres

import (
	"testing"

	flynnplugin "github.com/randy-girard/flynn/pkg/plugin"
)

// Flynn reads tenant_safe only from the provider object. A top-level key is
// dropped by the Manifest type, so hosted tenants could never provision.
func TestManifestProviderTenantSafe(t *testing.T) {
	m, err := flynnplugin.LoadManifest(".")
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider == nil || !m.Provider.TenantSafe {
		t.Fatalf("provider.tenant_safe must be true, got %+v", m.Provider)
	}
}
