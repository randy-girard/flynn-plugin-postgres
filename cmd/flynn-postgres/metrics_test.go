package main

import (
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func TestEmitInstanceMetricLineWritesResourceSample(t *testing.T) {
	t.Setenv("FLYNN_POSTGRES", "postgresql-basin-73690")
	t.Setenv(postgres.ResourceIDEnv, "39cd10de1737e3e7")
	origPsql := instancePsql
	origLog := instanceMetricsLog
	t.Cleanup(func() {
		instancePsql = origPsql
		instanceMetricsLog = origLog
	})
	instancePsql = func(query string) (string, error) {
		if strings.Contains(query, "concat_ws") {
			return "24114245|11|6|0|100|0.93548|1|70583|26097078|18898|0|0", nil
		}
		if strings.Contains(query, "pg_extension") {
			return "t", nil
		}
		return `[{"query":"SELECT 1"}]`, nil
	}
	var got string
	instanceMetricsLog = func(line string) { got = line }
	emitInstanceMetricLine()
	if !strings.Contains(got, "flynn-postgres") {
		t.Fatalf("line=%q", got)
	}
	if !strings.Contains(got, "source=postgresql-basin-73690") {
		t.Fatalf("line=%q", got)
	}
	if !strings.Contains(got, "addon=39cd10de1737e3e7") {
		t.Fatalf("line=%q", got)
	}
	if !strings.Contains(got, "sample#service-available=1") || !strings.Contains(got, "sample#tables=11") {
		t.Fatalf("line=%q", got)
	}
	if strings.Contains(got, "CONTEXT") || strings.Contains(got, "RAISE") {
		t.Fatalf("must print a clean sample line, not plpgsql noise: %q", got)
	}
}

func TestLogInstanceTopologyWritesFlynnLine(t *testing.T) {
	t.Setenv("FLYNN_POSTGRES", "postgresql-upland-22935")
	t.Setenv("POSTGRES_ROLE", "follower")
	t.Setenv("POSTGRES_LEADER", "postgresql-basin-73690")
	origLog := instanceMetricsLog
	t.Cleanup(func() { instanceMetricsLog = origLog })
	var got string
	instanceMetricsLog = func(line string) { got = line }
	logInstanceTopology()
	if got != "flynn-postgres source=postgresql-upland-22935 event#follower role=follower leader=postgresql-basin-73690" {
		t.Fatalf("got %q", got)
	}
}
