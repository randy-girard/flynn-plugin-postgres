package main

import (
	"fmt"
	"testing"
	"time"
)

func stubLeaderClaim(t *testing.T, timeout time.Duration) {
	t.Helper()
	origDelay, origTimeout := leaderClaimDelay, leaderClaimTimeout
	t.Cleanup(func() { leaderClaimDelay, leaderClaimTimeout = origDelay, origTimeout })
	leaderClaimDelay = time.Millisecond
	leaderClaimTimeout = timeout
}

var errNotManual = fmt.Errorf(`validation_error: service leader election type is not manual`)

// A discoverd follower that has not applied AddService yet answers
// "not manual" to SetLeader. The instance used to ignore that, so a new
// primary could run with no leader and leader.<name>.discoverd never resolved.
func TestClaimServiceLeaderRetriesWhileServiceConfigIsReplicating(t *testing.T) {
	stubLeaderClaim(t, time.Second)
	calls := 0
	err := claimServiceLeader(func() error {
		calls++
		if calls <= 2 {
			return errNotManual
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("SetLeader calls %d, want 3 (retry until the manual config is visible)", calls)
	}
}

func TestClaimServiceLeaderAcceptsAServiceThatElectsItsOwnLeader(t *testing.T) {
	stubLeaderClaim(t, 20*time.Millisecond)
	if err := claimServiceLeader(func() error { return errNotManual }); err != nil {
		t.Fatalf("a service that stays non-manual must not fail start: %v", err)
	}
}

func TestClaimServiceLeaderReturnsOtherErrors(t *testing.T) {
	stubLeaderClaim(t, time.Second)
	calls := 0
	if err := claimServiceLeader(func() error { calls++; return fmt.Errorf("unauthorized") }); err == nil || calls != 1 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}
