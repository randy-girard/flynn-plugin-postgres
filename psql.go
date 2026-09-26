package postgres

import (
	"fmt"
	"net/url"
	"strings"
)

// PsqlCommand is the stub for `pg:psql`. It targets this instance's URL only.
func PsqlCommand(rawURL string) ([]string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if err := EndpointAllowed(rawURL); err != nil {
		return nil, err
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "postgres" || u.Host == "" {
		return nil, fmt.Errorf("pg:psql requires this instance's postgres URL")
	}
	if u.Hostname() == PlatformApplianceHost || strings.Contains(u.Host, PlatformApplianceHost) {
		return nil, ErrPlatformAppliance
	}
	return []string{"psql", rawURL}, nil
}
