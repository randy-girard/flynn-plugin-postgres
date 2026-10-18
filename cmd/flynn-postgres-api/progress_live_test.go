package main

import (
	"strings"
	"testing"
)

func TestDashProgressScriptUsesLiveClient(t *testing.T) {
	script := dashProgressScript(`abc");alert(1)`)
	if strings.Contains(script, `var topic = "abc");alert`) {
		t.Fatal("app id broke out of the script string")
	}
	if !strings.Contains(script, "FlynnLive") || !strings.Contains(script, "/api/live.js") {
		t.Fatal("progress page should use the shared live client")
	}
	if !strings.Contains(script, "FlynnLive.watch(topic, tick, 0)") {
		t.Fatal("idle progress page must not poll; the timer starts only while a copy is running")
	}
	if strings.Contains(script, "tenant:") {
		t.Fatal("progress topic must not be a tenant name")
	}
}
