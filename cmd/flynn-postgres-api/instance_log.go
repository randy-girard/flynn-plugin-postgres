package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	logagg "github.com/randy-girard/flynn/logaggregator/types"
	"github.com/randy-girard/flynn/pkg/syslog/rfc5424"
	"github.com/randy-girard/flynn/pkg/syslog/rfc6587"
)

// resourceLogHostname is syslog HOSTNAME for plugin-injected instance notes.
// It must not be a flynn-host ID: host sinks resume from logaggregator cursors
// keyed by hostname, and a colliding seq would skip real job logs.
const resourceLogHostname = "postgres-plugin"

type resourceLogTarget struct {
	AppID       string
	JobID       string
	JobName     string
	ProcessType string
}

func topologyResourceApps(source *postgres.Instance, fromApp, toApp string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] || !postgres.IsolatedInstanceApp(s) {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	if source != nil {
		add(source.App)
	}
	add(fromApp)
	add(toApp)
	return out
}

var writeResourceLog = writeResourceLogDefault

func writeResourceLogDefault(h *handler, appRef, line string) {
	line = strings.TrimRight(strings.TrimSpace(line), "\n")
	if h == nil || line == "" || !postgres.IsolatedInstanceApp(appRef) {
		return
	}
	target := h.resourceLogTarget(appRef)
	if target == nil {
		return
	}
	msg := buildResourceSyslog(target, line)
	if err := sendResourceSyslog(msg); err != nil && h.log != nil {
		h.log.Error("instance topology log", "app", appRef, "err", err)
	}
}

func (h *handler) resourceLogTarget(appRef string) *resourceLogTarget {
	if h == nil || h.client == nil {
		return nil
	}
	appRef = strings.TrimSpace(appRef)
	if appRef == "" {
		return nil
	}
	t := &resourceLogTarget{
		AppID:       appRef,
		ProcessType: postgres.ProcessName,
	}
	if app, err := h.client.GetApp(appRef); err == nil && app != nil && strings.TrimSpace(app.ID) != "" {
		t.AppID = app.ID
	}
	job := runningPostgresJob(h.client, appRef)
	if job == nil && t.AppID != appRef {
		job = runningPostgresJob(h.client, t.AppID)
	}
	if job != nil {
		t.JobID = firstNonEmptyLocal(job.ID, job.UUID)
		t.JobName = ct.JobDisplayName(job)
		if strings.TrimSpace(job.Type) != "" {
			t.ProcessType = job.Type
		}
	}
	if strings.TrimSpace(t.AppID) == "" {
		return nil
	}
	return t
}

func runningPostgresJob(c interface {
	JobList(string) ([]*ct.Job, error)
}, app string) *ct.Job {
	if c == nil || strings.TrimSpace(app) == "" {
		return nil
	}
	jobs, err := c.JobList(app)
	if err != nil {
		return nil
	}
	var fallback *ct.Job
	for _, j := range jobs {
		if j == nil {
			continue
		}
		if !postgresJob(j) {
			continue
		}
		switch j.State {
		case ct.JobStateUp, ct.JobStateStarting:
			return j
		case ct.JobStatePending, ct.JobStateStopping:
			if fallback == nil {
				fallback = j
			}
		}
	}
	return fallback
}

func postgresJob(j *ct.Job) bool {
	if j == nil {
		return false
	}
	if strings.TrimSpace(j.Type) == postgres.ProcessName {
		return true
	}
	name := strings.ToLower(ct.JobDisplayName(j))
	return strings.HasPrefix(name, postgres.ProcessName+".")
}

func buildResourceSyslog(t *resourceLogTarget, line string) *rfc5424.Message {
	if t == nil {
		t = &resourceLogTarget{}
	}
	procType := firstNonEmptyLocal(t.ProcessType, postgres.ProcessName)
	procID := procType
	if id := strings.TrimSpace(t.JobID); id != "" {
		procID = procType + "." + id
	}
	hdr := &rfc5424.Header{
		Hostname: []byte(resourceLogHostname),
		AppName:  []byte(strings.TrimSpace(t.AppID)),
		ProcID:   []byte(procID),
		MsgID:    []byte(logagg.MsgIDSystem),
		Severity: 6,
		Facility: 23,
		Version:  1,
	}
	msg := rfc5424.NewMessage(hdr, []byte(line))
	seq := uint64(time.Now().UnixNano())
	sd := &rfc5424.StructuredData{
		ID: []byte("flynn"),
		Params: []rfc5424.StructuredDataParam{
			{Name: []byte("seq"), Value: []byte(strconv.FormatUint(seq, 10))},
		},
	}
	if name := strings.TrimSpace(t.JobName); name != "" {
		sd.Params = append(sd.Params, rfc5424.StructuredDataParam{
			Name:  []byte("job_name"),
			Value: []byte(name),
		})
	}
	var buf bytes.Buffer
	_ = sd.Encode(&buf)
	msg.StructuredData = buf.Bytes()
	return msg
}

var logaggregatorSyslogAddrs = func() []string {
	c := postgres.NewDiscoverdClient()
	insts, err := c.Instances("logaggregator", time.Second)
	if err != nil || len(insts) == 0 {
		return nil
	}
	out := make([]string, 0, len(insts))
	for _, inst := range insts {
		if inst == nil {
			continue
		}
		addr := strings.TrimSpace(inst.Addr)
		if addr == "" {
			continue
		}
		out = append(out, addr)
	}
	return out
}

var syslogWrite = func(addr string, payload []byte) error {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Write(payload)
	return err
}

func sendResourceSyslog(msg *rfc5424.Message) error {
	if msg == nil || len(msg.AppName) == 0 || len(msg.Msg) == 0 {
		return nil
	}
	addrs := logaggregatorSyslogAddrs()
	if len(addrs) == 0 {
		return fmt.Errorf("no logaggregator syslog addrs")
	}
	frame := rfc6587.Bytes(msg)
	var last error
	ok := 0
	for _, addr := range addrs {
		if err := syslogWrite(addr, frame); err != nil {
			last = err
			continue
		}
		ok++
	}
	if ok == 0 {
		if last == nil {
			last = fmt.Errorf("logaggregator syslog write failed")
		}
		fmt.Fprintf(os.Stderr, "postgres-api: instance log syslog: %v\n", last)
		return last
	}
	return nil
}
