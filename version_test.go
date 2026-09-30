package postgres

import (
	"bytes"
	"strings"
	"testing"
)

func TestEngineVersionDefault(t *testing.T) {
	t.Setenv("ENGINE_VERSION", "")
	if got := EngineVersion(); got != DefaultEngineVersion {
		t.Fatalf("EngineVersion()=%q", got)
	}
}

func TestEngineVersionFromEnv(t *testing.T) {
	t.Setenv("ENGINE_VERSION", "17")
	if got := EngineVersion(); got != "17" {
		t.Fatalf("EngineVersion()=%q", got)
	}
}

func TestLogEngineVersion(t *testing.T) {
	t.Setenv("ENGINE_VERSION", "16")
	var buf bytes.Buffer
	LogEngineVersion(&buf)
	if !strings.Contains(buf.String(), "engine version 16") {
		t.Fatalf("log: %s", buf.String())
	}
}

func TestNeedsEngineUpgrade(t *testing.T) {
	if NeedsEngineUpgrade("16", "16") {
		t.Fatal("same version")
	}
	if !NeedsEngineUpgrade("16", "17") {
		t.Fatal("major bump")
	}
	if !NeedsEngineUpgrade("", "16") {
		t.Fatal("unknown installed version is an upgrade")
	}
}

func TestVersionReportMarksStaleInstance(t *testing.T) {
	t.Setenv("ENGINE_VERSION", "17")
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.byID[leader.ID].EngineVersion = "16"
	s.mu.Unlock()
	rep := s.VersionReport()
	if !rep.UpgradeAvailable || rep.ImageVersion != "17" {
		t.Fatalf("%+v", rep)
	}
	if len(rep.Instances) != 1 || !rep.Instances[0].UpgradeAvailable || rep.Instances[0].Version != "16" {
		t.Fatalf("%+v", rep.Instances)
	}
}
