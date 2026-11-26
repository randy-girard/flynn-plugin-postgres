package main

import (
	"path"
	"strings"
	"testing"

	flynnplugin "github.com/randy-girard/flynn/pkg/plugin"
)

// DATA-69: both maintenance actions run this plugin's API binary, which the
// flynn CLI (owned action, on the user app) and flynn-host (cluster action,
// on the plugin app) give the plugin API key, and they reach its
// maintenance command.
func TestMaintenanceActionsRunThePluginAPI(t *testing.T) {
	m, err := flynnplugin.LoadManifest("../..")
	if err != nil {
		t.Fatal(err)
	}
	var owned, cluster *flynnplugin.CLIAction
	for i := range m.CLI.Actions {
		a := &m.CLI.Actions[i]
		if a.Name != "maintenance" {
			continue
		}
		if a.Owned && a.EffectiveScope() == flynnplugin.CLIScopeApp {
			owned = a
		}
		if a.EffectiveScope() == flynnplugin.CLIScopeCluster {
			cluster = a
		}
	}
	if owned == nil || cluster == nil {
		t.Fatalf("want an owned app maintenance action and a cluster one: %+v", m.CLI.Actions)
	}
	const bin = "/bin/flynn-postgres-api"
	if path.Base(owned.Args[0]) != path.Base(bin) || path.Base(cluster.Args[0]) != path.Base(bin) {
		t.Fatalf("maintenance must run %s: owned %q cluster %q", bin, owned.Args, cluster.Args)
	}
	if got := strings.Join(owned.Args[1:], " "); got != "task maintenance" {
		t.Fatalf("owned maintenance args %q, want %q", got, "task maintenance")
	}
	// flynn-host flynn-postgres-api:maintenance <names> appends "maintenance <names>".
	if !cluster.Passthrough || cluster.ReleaseEnv {
		t.Fatalf("cluster maintenance must pass its words through without the release env: %+v", cluster)
	}
	if got := strings.Join(append(append([]string{}, cluster.Args[1:]...), "maintenance"), " "); got != "task maintenance" {
		t.Fatalf("flynn-host maintenance runs %q, want %q", got, "task maintenance")
	}
}
