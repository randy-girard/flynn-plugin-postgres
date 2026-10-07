package main

import (
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	logagg "github.com/randy-girard/flynn/logaggregator/types"
	"github.com/randy-girard/flynn/logaggregator/utils"
	"github.com/randy-girard/flynn/pkg/syslog/rfc5424"
)

func TestTopologyResourceAppsOnlyIsolated(t *testing.T) {
	src := &postgres.Instance{App: "postgresql-concave-78237"}
	got := topologyResourceApps(src, "app-one", "postgresql-orchid-06245")
	if len(got) != 2 || got[0] != "postgresql-concave-78237" || got[1] != "postgresql-orchid-06245" {
		t.Fatalf("%v", got)
	}
	if topologyResourceApps(nil, "postgres-plugin", "") != nil {
		t.Fatal("plugin app is not an instance")
	}
}

func TestBuildResourceSyslogIsFlynnSystemLine(t *testing.T) {
	line := postgres.FormatTopologyLine("postgresql-concave-78237", "swap-start", "from=postgresql-concave-78237 reason=image-refresh action=create-replica")
	msg := buildResourceSyslog(&resourceLogTarget{
		AppID:       "app-uuid-1",
		JobID:       "host-a-jobuuid",
		JobName:     "postgres.2702",
		ProcessType: "postgres",
	}, line)
	if string(msg.AppName) != "app-uuid-1" {
		t.Fatalf("app %q", msg.AppName)
	}
	if string(msg.Hostname) != resourceLogHostname {
		t.Fatalf("hostname %q must not steal flynn-host cursors", msg.Hostname)
	}
	if string(msg.ProcID) != "postgres.host-a-jobuuid" {
		t.Fatalf("procid %q", msg.ProcID)
	}
	if string(msg.MsgID) != string(logagg.MsgIDSystem) {
		t.Fatalf("msgid %q", msg.MsgID)
	}
	if !strings.Contains(string(msg.Msg), "event#swap-start") {
		t.Fatalf("msg %q", msg.Msg)
	}
	if utils.StreamType(msg) != logagg.StreamTypeSystem {
		t.Fatalf("stream %s", utils.StreamType(msg))
	}
	parsed, err := rfc5424.Parse(msg.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	sd, err := rfc5424.ParseStructuredData(parsed.StructuredData)
	if err != nil || sd == nil {
		t.Fatalf("sd %v %v", sd, err)
	}
	gotName := ""
	for _, p := range sd.Params {
		if string(p.Name) == "job_name" {
			gotName = string(p.Value)
		}
	}
	if gotName != "postgres.2702" {
		t.Fatalf("job_name %q", gotName)
	}
}

func TestRunningPostgresJobPrefersUp(t *testing.T) {
	c := &fakeJobClient{jobs: map[string][]*ct.Job{
		"postgresql-concave-78237": {
			{Type: "web", State: ct.JobStateUp, UUID: "web1", Name: "web.1"},
			{Type: "postgres", State: ct.JobStateStopping, UUID: "old", Name: "postgres.1"},
			{Type: "postgres", State: ct.JobStateUp, ID: "host-a-uuid", UUID: "uuid", Name: "postgres.2702"},
		},
	}}
	job := runningPostgresJob(c, "postgresql-concave-78237")
	if job == nil || job.Name != "postgres.2702" {
		t.Fatalf("%+v", job)
	}
}
