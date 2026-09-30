package postgres

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// DefaultEngineVersion is the PostgreSQL major version this plugin image ships.
const DefaultEngineVersion = "16"

// EngineVersion is the database engine version this plugin image installs.
// ENGINE_VERSION is baked into the plugin app via flynn-plugin.json image_env.
func EngineVersion() string {
	if v := strings.TrimSpace(os.Getenv("ENGINE_VERSION")); v != "" {
		return v
	}
	return DefaultEngineVersion
}

// LogEngineVersion writes the engine version this process will run.
func LogEngineVersion(w io.Writer) {
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "flynn-plugin-postgres engine version %s\n", EngineVersion())
}

// NeedsEngineUpgrade is true when an instance is not on the image's engine version.
func NeedsEngineUpgrade(installed, available string) bool {
	available = strings.TrimSpace(available)
	if available == "" {
		return false
	}
	installed = strings.TrimSpace(installed)
	if installed == "" {
		return true
	}
	return installed != available
}

// InstanceVersion is one resource in GET /version.
type InstanceVersion struct {
	ID               string `json:"id"`
	App              string `json:"app"`
	Role             string `json:"role"`
	Version          string `json:"version,omitempty"`
	UpgradeAvailable bool   `json:"upgrade_available"`
	Followers        int    `json:"followers,omitempty"`
}

// VersionReport is GET /version for the dashboard.
type VersionReport struct {
	Engine           string             `json:"engine"`
	ImageVersion     string             `json:"image_version"`
	UpgradeAvailable bool               `json:"upgrade_available"`
	Instances        []InstanceVersion  `json:"instances"`
}

// VersionReport lists each stored instance against the current plugin image.
func (s *Store) VersionReport() VersionReport {
	available := EngineVersion()
	out := VersionReport{
		Engine:       "postgres",
		ImageVersion: available,
		Instances:    []InstanceVersion{},
	}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, inst := range s.byID {
		if inst == nil {
			continue
		}
		row := instanceVersionLocked(inst, available)
		if row.UpgradeAvailable {
			out.UpgradeAvailable = true
		}
		out.Instances = append(out.Instances, row)
	}
	return out
}

func instanceVersionLocked(inst *Instance, available string) InstanceVersion {
	v := strings.TrimSpace(inst.EngineVersion)
	return InstanceVersion{
		ID:               inst.ID,
		App:              inst.App,
		Role:             string(inst.Role),
		Version:          v,
		UpgradeAvailable: NeedsEngineUpgrade(v, available),
		Followers:        len(inst.Followers),
	}
}

// EngineCardDetails is dashboard card fields for current vs available engine version.
func EngineCardDetails(insts []*Instance) map[string]string {
	available := EngineVersion()
	d := map[string]string{
		"engine_version":   available,
		"engine_available": available,
	}
	if len(insts) == 0 {
		return d
	}
	seen := map[string]bool{}
	var versions []string
	upgrade := false
	for _, inst := range insts {
		if inst == nil {
			continue
		}
		v := strings.TrimSpace(inst.EngineVersion)
		if NeedsEngineUpgrade(v, available) {
			upgrade = true
		}
		if v == "" {
			continue
		}
		if !seen[v] {
			seen[v] = true
			versions = append(versions, v)
		}
	}
	if len(versions) > 0 {
		d["engine_version"] = strings.Join(versions, ",")
	}
	if upgrade {
		d["upgrade_available"] = "true"
	}
	return d
}
