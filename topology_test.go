package postgres

import (
	"strings"
	"testing"
)

func TestFormatTopologyLine(t *testing.T) {
	got := FormatTopologyLine("postgresql-basin-73690", "failover", "from=postgresql-basin-73690 to=postgresql-upland-22935")
	want := "flynn-postgres source=postgresql-basin-73690 event#failover from=postgresql-basin-73690 to=postgresql-upland-22935"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if TopologyEventName(CodeFailover) != "failover" || TopologySeverity(CodeFailover) != "warning" {
		t.Fatalf("%s %s", TopologyEventName(CodeFailover), TopologySeverity(CodeFailover))
	}
	if TopologySeverity(CodeSwapFailed) != "error" || TopologySeverity(CodePromoted) != "info" {
		t.Fatal("severity")
	}
	desc := TopologyDescription(CodeFailover, "postgresql-basin-73690", "postgresql-upland-22935", "")
	if !strings.Contains(desc, "postgresql-upland-22935") || !strings.Contains(desc, "postgresql-basin-73690") {
		t.Fatalf("desc %q", desc)
	}
}
