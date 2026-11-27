package main

import (
	"testing"

	ct "github.com/randy-girard/flynn/controller/types"
	postgres "github.com/randy-girard/flynn-plugin-postgres"
)

type countingReleases struct {
	fakeAppRelease
	reads map[string]int
}

func (c countingReleases) GetAppRelease(id string) (*ct.Release, error) {
	c.reads[id]++
	return c.fakeAppRelease.GetAppRelease(id)
}

// NEW-36(c): a tenant app attached to a follower carries
// FLYNN_POSTGRES_LEADER (an attachment key). It is not a replica, so it must
// not block destroying the primary or be reaped as an orphan follower, and
// its release (like that of every non-instance app) is not read at all.
func TestLiveFollowerAppNamesSkipsTenantAndPluginApps(t *testing.T) {
	leader := &postgres.Instance{App: "postgresql-fjord-21944"}
	c := countingReleases{reads: map[string]int{}, fakeAppRelease: fakeAppRelease{
		apps: map[string]*ct.App{
			"postgresql-fjord-21944":  {ID: "p", Name: "postgresql-fjord-21944"},
			"postgresql-orchid-80127": {ID: "f", Name: "postgresql-orchid-80127"},
			"shop":                    {ID: "shop-id", Name: "shop"},
			"postgres-plugin":         {ID: "plug", Name: "postgres-plugin", Meta: map[string]string{"flynn-plugin": "true"}},
		},
		releases: map[string]*ct.Release{
			"p": {Env: map[string]string{"FLYNN_POSTGRES": "postgresql-fjord-21944"}},
			"f": {Env: map[string]string{
				"FLYNN_POSTGRES":        "postgresql-orchid-80127",
				"FLYNN_POSTGRES_LEADER": "postgresql-fjord-21944",
				"FLYNN_POSTGRES_ROLE":   "follower",
			}},
			"shop-id": {Env: map[string]string{
				"FLYNN_POSTGRES_LEADER": "postgresql-fjord-21944",
				"FLYNN_POSTGRES_ROLE":   "follower",
			}},
			"plug": {Env: map[string]string{"FLYNN_POSTGRES_LEADER": "postgresql-fjord-21944"}},
		},
	}}
	names, err := liveFollowerAppNames(c, leader)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "postgresql-orchid-80127" {
		t.Fatalf("only the replica is a follower, got %q", names)
	}
	if c.reads["shop-id"] != 0 || c.reads["plug"] != 0 {
		t.Fatalf("releases of non-instance apps must not be read: %v", c.reads)
	}
}
