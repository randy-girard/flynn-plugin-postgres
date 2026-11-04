package main

import (
	"io"
	"net/http"
	"strings"

	"github.com/randy-girard/flynn/pkg/appliancehttp"
)

// allowUnauthenticatedAPI is nil in production, so the API fails closed.
// Handler tests set it to reach routes without a cluster key. The auth
// regression turns it off.
var allowUnauthenticatedAPI func() bool

func pluginAPIPublic(path string) bool {
	if path == "/ping" || appliancehttp.PublicStatusPath(path) {
		return true
	}
	return path == "/dashboard" || strings.HasPrefix(path, "/dashboard/")
}

func rejectUnauthenticatedAPI(w http.ResponseWriter, r *http.Request) bool {
	if r == nil || pluginAPIPublic(r.URL.Path) {
		return false
	}
	if allowUnauthenticatedAPI != nil && allowUnauthenticatedAPI() {
		return false
	}
	if appliancehttp.Authorized(r, appliancehttp.Key()) {
		return false
	}
	appliancehttp.Unauthorized(w)
	return true
}

func pluginAPIRequest(method, rawURL, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	appliancehttp.SetAuth(req, appliancehttp.Key())
	return http.DefaultClient.Do(req)
}
