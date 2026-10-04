package main

import (
	"errors"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

// oldAlreadyCurrentByImage is the boot check that caused the cluster storm:
// plugin:update --rebuild uploads a new layer, POSTGRES_IMAGE_ID changes, and
// every primary looked "not current" so API boot started logical upgrades.
func oldAlreadyCurrentByImage(rel *ct.Release, imageID string) bool {
	if rel == nil || len(rel.ArtifactIDs) == 0 {
		return false
	}
	if rel.Env != nil {
		if v := rel.Env["ENGINE_VERSION"]; postgres.NeedsEngineUpgrade(v, postgres.EngineVersion()) {
			return false
		}
	}
	return rel.ArtifactIDs[0] == imageID
}

func TestClusterUpgradeSkipIgnoresPluginRebuildImageID(t *testing.T) {
	engine := postgres.EngineVersion()
	rel := &ct.Release{
		ArtifactIDs: []string{"layer-before-rebuild"},
		Env:         map[string]string{"ENGINE_VERSION": engine},
	}
	newImage := "layer-after-rebuild"
	if oldAlreadyCurrentByImage(rel, newImage) {
		t.Fatal("precondition: the old image-id check must treat a rebuild as not current")
	}
	if !clusterUpgradeSkip(rel, nil, engine) {
		t.Fatal("matching ENGINE_VERSION must skip even when the plugin image id changed")
	}
	if !clusterUpgradeSkip(rel, nil, "stale-store-value") {
		t.Fatal("release ENGINE_VERSION must win over a stale store copy")
	}
}

func TestClusterUpgradeSkipTable(t *testing.T) {
	engine := postgres.EngineVersion()
	older := "15"
	if engine == older {
		older = "14"
	}
	cases := []struct {
		name   string
		rel    *ct.Release
		err    error
		stored string
		skip   bool
	}{
		{
			name: "inspect error does not upgrade",
			err:  errors.New("controller: not found"),
			skip: true,
		},
		{
			name: "missing release does not upgrade",
			skip: true,
		},
		{
			name:   "unknown engine does not upgrade",
			rel:    &ct.Release{ArtifactIDs: []string{"other-image"}, Env: map[string]string{}},
			stored: "",
			skip:   true,
		},
		{
			name:   "matching engine skips after rebuild",
			rel:    &ct.Release{ArtifactIDs: []string{"other-image"}, Env: map[string]string{"ENGINE_VERSION": engine}},
			stored: engine,
			skip:   true,
		},
		{
			name:   "older engine upgrades even on the same image id",
			rel:    &ct.Release{ArtifactIDs: []string{"same-image"}, Env: map[string]string{"ENGINE_VERSION": older}},
			stored: older,
			skip:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clusterUpgradeSkip(tc.rel, tc.err, tc.stored)
			if got != tc.skip {
				t.Fatalf("skip=%v want %v", got, tc.skip)
			}
		})
	}
}

func TestBeginClusterUpgradesSkipsMatchingEngineOnBoot(t *testing.T) {
	h := newHandler(postgres.NewStore())
	if _, _, err := h.store.Provision(postgres.ProvisionRequest{App: "shop"}); err != nil {
		t.Fatal(err)
	}
	started, skipped := h.beginClusterUpgrades()
	if len(started) != 0 {
		t.Fatalf("boot must not logical-upgrade current engines: started %#v skipped %#v", started, skipped)
	}
	if len(skipped) == 0 {
		t.Fatal("current primary must be listed as skipped")
	}
}

func TestBeginClusterUpgradesStartsOlderEngine(t *testing.T) {
	t.Setenv("ENGINE_VERSION", "16")
	h := newHandler(postgres.NewStore())
	if _, _, err := h.store.Provision(postgres.ProvisionRequest{App: "shop"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENGINE_VERSION", "17")
	started, skipped := h.beginClusterUpgrades()
	if len(started) != 1 {
		t.Fatalf("older engine must start a logical upgrade: started %#v skipped %#v", started, skipped)
	}
}

func TestIsolatedUpgradeCandidateSkipsTenantApps(t *testing.T) {
	if isolatedUpgradeCandidate(&postgres.Instance{App: "app-one", Role: postgres.RolePrimary}) {
		t.Fatal("tenant app-one must not be a cluster-upgrade candidate")
	}
	if isolatedUpgradeCandidate(&postgres.Instance{App: "shop", Role: postgres.RolePrimary}) {
		t.Fatal("tenant shop must not be a cluster-upgrade candidate")
	}
	if isolatedUpgradeCandidate(&postgres.Instance{App: "postgresql-harbor-12345", Role: postgres.RoleFollower}) {
		t.Fatal("followers are not upgrade candidates")
	}
	if !isolatedUpgradeCandidate(&postgres.Instance{App: "postgresql-harbor-12345", Role: postgres.RolePrimary}) {
		t.Fatal("isolated primary must be an upgrade candidate")
	}
}
