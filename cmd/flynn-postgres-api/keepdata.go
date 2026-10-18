// Package datastoreguard stops a datastore app from being deleted while
// another app still points at its data. Image refresh and orphan cleanup
// must not drop that app; resource removal clears those references first.
package main

import (
	"fmt"
	"strings"

	ct "github.com/randy-girard/flynn/controller/types"
)

// ReleaseSource lists apps and their current releases.
type ReleaseSource interface {
	AppList() ([]*ct.App, error)
	GetAppRelease(string) (*ct.Release, error)
}

// RefuseIfReferenced returns an error when any app other than appName still
// points at that datastore. A lookup failure also refuses the delete: an
// unknown reference is not proof that the data is unused.
func RefuseIfReferenced(src ReleaseSource, appName string) error {
	appName = strings.TrimSpace(appName)
	if src == nil || appName == "" {
		return fmt.Errorf("refusing to delete datastore %q: cannot check references", appName)
	}
	who, err := ReferencingApp(src, appName)
	if err != nil {
		return fmt.Errorf("refusing to delete datastore %s: %w", appName, err)
	}
	if who != "" {
		return fmt.Errorf("refusing to delete datastore %s: %s still references its data", appName, who)
	}
	return nil
}

// ReferencingApp is the other app whose current release still points at
// appName. The datastore's own release is ignored.
func ReferencingApp(src ReleaseSource, appName string) (string, error) {
	appName = strings.TrimSpace(appName)
	if src == nil || appName == "" {
		return "", fmt.Errorf("missing datastore name")
	}
	apps, err := src.AppList()
	if err != nil {
		return "", err
	}
	for _, app := range apps {
		if app == nil || strings.EqualFold(app.Name, appName) {
			continue
		}
		rel, err := src.GetAppRelease(app.ID)
		if err != nil {
			if notFound(err) {
				continue
			}
			return "", err
		}
		if rel == nil {
			continue
		}
		if References(rel.Env, appName) {
			if app.Name != "" {
				return app.Name, nil
			}
			return app.ID, nil
		}
	}
	return "", nil
}

// References reports whether env still points at a datastore app. It matches
// every data plugin: a discoverd host in any URL, or an env value that is
// exactly the app name (FLYNN_POSTGRES, FLYNN_REDIS, FLYNN_MYSQL,
// FLYNN_MONGODB, FLYNN_KAFKA, FLYNN_CLICKHOUSE, and *_LEADER).
func References(env map[string]string, appName string) bool {
	name := strings.TrimSpace(appName)
	if name == "" || len(env) == 0 {
		return false
	}
	hosts := []string{
		"leader." + name + ".discoverd",
		name + ".discoverd",
	}
	for _, v := range env {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if strings.EqualFold(v, name) {
			return true
		}
		for _, host := range hosts {
			if containsHost(v, host) {
				return true
			}
		}
	}
	return false
}

func containsHost(value, host string) bool {
	lower := strings.ToLower(value)
	host = strings.ToLower(host)
	from := 0
	for from < len(lower) {
		i := strings.Index(lower[from:], host)
		if i < 0 {
			return false
		}
		i += from
		beforeOK := i == 0 || isBoundary(lower[i-1])
		end := i + len(host)
		afterOK := end == len(lower) || isBoundary(lower[end])
		if beforeOK && afterOK {
			return true
		}
		from = i + 1
	}
	return false
}

func isBoundary(c byte) bool {
	switch c {
	case ':', '/', '@', '?', '&', '=', '"', '\'', ' ', ',', ';':
		return true
	default:
		return false
	}
}

func notFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}
