package main

import "testing"

func TestLiveAppIDsDropTenantAndDuplicates(t *testing.T) {
	got := liveAppIDs(" shop ", "tenant:acme", "shop", "app/secret", "", "other")
	if len(got) != 2 || got[0] != "shop" || got[1] != "other" {
		t.Fatalf("ids = %#v", got)
	}
}
