package postgres

import (
	"errors"
	"net/url"
	"strings"
)

// ProviderName is the catalog provider `flynn resource:add postgres` selects.
// The platform appliance registers as platform-postgres, not postgres.
const ProviderName = "postgres"

// PlatformProviderName is the built-in appliance provider from pkg/pgappliance.
const PlatformProviderName = "platform-postgres"

// PluginDiscoverdHost is this plugin's API. It must not be the system appliance.
const PluginDiscoverdHost = "postgres-plugin.discoverd"

// PlatformApplianceHost is pkg/pgappliance.PlatformApplianceHost.
// Tenant provision never dials it and never copies its superuser password.
const PlatformApplianceHost = "postgres-api.discoverd"

// ErrPlatformAppliance is returned when a tenant operation would use the system appliance.
var ErrPlatformAppliance = errors.New("tenant Postgres must not target postgres-api.discoverd or the platform-postgres appliance")

// ProviderURL is the resource provider endpoint installed with this plugin.
func ProviderURL() string {
	return "http://" + PluginDiscoverdHost + "/databases"
}

// EndpointAllowed reports whether raw is a tenant provider URL.
// postgres-api.discoverd is the platform appliance and is always rejected.
func EndpointAllowed(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ErrPlatformAppliance
	}
	if strings.Contains(raw, PlatformApplianceHost) || strings.Contains(raw, PlatformProviderName) {
		return ErrPlatformAppliance
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ErrPlatformAppliance
	}
	host := u.Hostname()
	if host == "" || host == PlatformApplianceHost || strings.HasPrefix(host, "postgres-api.") {
		return ErrPlatformAppliance
	}
	return nil
}
