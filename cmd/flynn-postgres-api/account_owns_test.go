package main

import (
	"testing"

	ct "github.com/randy-girard/flynn/controller/types"
)

type fakeResourceLister []*ct.Resource

func (f fakeResourceLister) ResourceListAll() ([]*ct.Resource, error) { return f, nil }

// NEW-27: an unattached follow is allowed when the leader's controller
// resource belongs to the stamped owner account.
func TestAccountOwnsLeaderChecksControllerOwner(t *testing.T) {
	list := fakeResourceLister{
		{ID: "r1", OwnerAccount: "user:ada", Env: map[string]string{"FLYNN_POSTGRES": "postgres-gorge-85967"}},
		{ID: "r2", OwnerAccount: "user:eve", Env: map[string]string{"FLYNN_POSTGRES": "postgres-other-11111"}},
	}
	if !accountOwnsLeader(list, "postgres-gorge-85967", "user:ada") {
		t.Fatal("owner account must own its leader")
	}
	if accountOwnsLeader(list, "postgres-gorge-85967", "user:eve") {
		t.Fatal("another account must not")
	}
	if accountOwnsLeader(list, "postgres-gorge-85967", "") || accountOwnsLeader(nil, "postgres-gorge-85967", "user:ada") {
		t.Fatal("no account or no client must not")
	}
}
