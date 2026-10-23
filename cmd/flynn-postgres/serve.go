package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn/discoverd/client"
	"github.com/randy-girard/flynn/pkg/httphelper"
	"github.com/randy-girard/flynn/pkg/shutdown"
)

// servePostgres runs the database as a child and registers it with discoverd.
// The API waits for that registration before it returns the resource.
func servePostgres() error {
	bin := os.Getenv("POSTGRES_BIN")
	service := os.Getenv("FLYNN_POSTGRES")
	if bin == "" || service == "" {
		return fmt.Errorf("POSTGRES_BIN and FLYNN_POSTGRES are required")
	}
	cmd := exec.Command("setpriv", "--reuid=postgres", "--regid=postgres", "--init-groups", "--inh-caps=-all", bin, "-D", "/data")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	shutdown.BeforeExit(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	})

	if err := waitLocalPort("127.0.0.1:5432", 60*time.Second); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return err
	}
	postgres.LogEngineVersion(os.Stderr)
	ensurePgStatStatements(bin)
	ensureWalKeepSize()
	if _, err := os.Stat("/data/standby.signal"); err != nil && os.Getenv("FLYNN_POSTGRES_PRIMARY_URL") == "" {
		if err := ensureConnectIsolation(bin); err != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			return err
		}
	}
	disc := postgres.NewDiscoverdClient()
	// Isolated databases are LeaderTypeManual. Oldest election never runs, so
	// leader.<service>.discoverd stays empty after a reboot until this process
	// claims its own service. A service that already elects its own leader
	// rejects SetLeader.
	if err := disc.AddService(service, &discoverd.ServiceConfig{LeaderType: discoverd.LeaderTypeManual}); err != nil && !httphelper.IsObjectExistsError(err) {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return postgres.WrapDiscoverdAuth(err)
	}
	inst := &discoverd.Instance{
		Addr: ":5432",
		Meta: map[string]string{"flynn-datastore": "true"},
	}
	hb, err := disc.RegisterInstance(service, inst)
	if err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return postgres.WrapDiscoverdAuth(err)
	}
	shutdown.BeforeExit(func() { hb.Close() })
	// FLYNN_POSTGRES is this job's own service. A follower must claim it too:
	// image-swap reads replay from leader.<follower>.discoverd. That claim
	// does not point leader.<primary>.discoverd at the replica.
	if err := ignoreLeaderElectionError(disc.Service(service).SetLeader(inst.ID)); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return postgres.WrapDiscoverdAuth(err)
	}

	go serveInstanceMetrics()
	logInstanceTopology()
	return <-exited
}

// ignoreLeaderElectionError drops SetLeader failures from services that elect
// their own leader. A manual service has no leader until the primary claims it.
func ignoreLeaderElectionError(err error) error {
	if err == nil || strings.Contains(err.Error(), "not manual") {
		return nil
	}
	return err
}

// instanceMetricsTimeout bounds collection under flynn-host's 2s scrape timeout.
const instanceMetricsTimeout = 1500 * time.Millisecond

var instancePsql = localPsqlContext
var instanceMetricsLog = func(line string) {
	// stdout so flynn-host can promote the line to StreamTypeSystem
	// (flynn[postgres.N], white). stderr is painted red as app errors.
	fmt.Fprintln(os.Stdout, line)
}

// serveInstanceMetrics serves GET /metrics/log on :$FLYNN_METRICS_PORT.
// flynn-host scrapes it once a minute; tenant jobs cannot reach the port.
func serveInstanceMetrics() {
	addr := ":" + postgres.InstanceMetricsPort()
	srv := &http.Server{
		Addr:              addr,
		Handler:           instanceMetricsHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "metrics listener %s: %s\n", addr, err)
	}
}

func instanceMetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+postgres.MetricsLogPath, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), instanceMetricsTimeout)
		defer cancel()
		line := instanceMetricLine(ctx)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, line)
	})
	return mux
}

func instanceMetricLine(ctx context.Context) string {
	series := collectInstanceSeries(ctx)
	source, addon := postgres.InstanceMetricIDs()
	return postgres.FormatFlynnPostgresLine(source, addon, series)
}

func logInstanceTopology() {
	source, _ := postgres.InstanceMetricIDs()
	role := strings.ToLower(strings.TrimSpace(firstSet(os.Getenv("FLYNN_POSTGRES_ROLE"), os.Getenv("POSTGRES_ROLE"))))
	leader := strings.TrimSpace(firstSet(os.Getenv("FLYNN_POSTGRES_LEADER"), os.Getenv("POSTGRES_LEADER")))
	event := "primary"
	msg := "role=primary"
	switch role {
	case "follower":
		event = "follower"
		msg = "role=follower"
		if leader != "" {
			msg += " leader=" + leader
		}
	case "deposed":
		event = "deposed"
		msg = "role=deposed"
		if leader != "" {
			msg += " leader=" + leader
		}
	}
	instanceMetricsLog(postgres.FormatTopologyLine(source, event, msg))
}

func collectInstanceSeries(ctx context.Context) map[string]float64 {
	series := map[string]float64{"service_available": 0, "errors": 1}
	raw, err := instancePsql(ctx, postgres.SnapshotSQL)
	if err != nil {
		return series
	}
	parsed, ok := postgres.ParsePostgresSnapshot(raw)
	if !ok {
		return series
	}
	for k, v := range parsed {
		series[k] = v
	}
	series["service_available"] = 1
	series["errors"] = 0
	if n, ok := countLocalSlowQueries(ctx); ok {
		series["slow_query_count"] = float64(n)
	}
	return series
}

func countLocalSlowQueries(ctx context.Context) (int, bool) {
	query := postgres.ActivitySlowSQL
	if exists, err := instancePsql(ctx, postgres.StatStatementsExistsSQL); err == nil {
		v := strings.ToLower(strings.TrimSpace(exists))
		if i := strings.IndexByte(v, '\n'); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if v == "t" || v == "true" {
			query = postgres.StatStatementsSQL
		}
	}
	raw, err := instancePsql(ctx, query)
	if err != nil {
		return 0, false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return 0, true
	}
	return strings.Count(raw, `"query"`), true
}

func localPsql(query string) (string, error) {
	return localPsqlContext(context.Background(), query)
}

func localPsqlContext(ctx context.Context, query string) (string, error) {
	bin := os.Getenv("POSTGRES_BIN")
	if bin == "" || strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("missing POSTGRES_BIN or query")
	}
	psql := filepath.Join(filepath.Dir(bin), "psql")
	db := os.Getenv("POSTGRES_DB")
	if db == "" {
		db = "postgres"
	}
	cmd := exec.CommandContext(ctx, "setpriv", "--reuid=postgres", "--regid=postgres", "--init-groups", "--inh-caps=-all",
		psql, "-h", "/tmp", "-d", db, "-v", "ON_ERROR_STOP=1", "-t", "-A", "-c", query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("psql: %s: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func ensureWalKeepSize() {
	if _, err := os.Stat("/data/standby.signal"); err == nil {
		return
	}
	// Own statements: ALTER SYSTEM cannot run in a multi-command transaction.
	_, _ = localPsql("ALTER SYSTEM SET wal_keep_size = '1GB'")
	_, _ = localPsql("SELECT pg_reload_conf()")
}

func ensurePgStatStatements(postgresBin string) {
	psql := filepath.Join(filepath.Dir(postgresBin), "psql")
	asPostgres := []string{"setpriv", "--reuid=postgres", "--regid=postgres", "--init-groups", "--inh-caps=-all", psql, "-h", "/tmp"}
	names := map[string]bool{"template1": true}
	if db := os.Getenv("POSTGRES_DB"); postgresIdentOK(db) {
		names[db] = true
	}
	list := append(append([]string{}, asPostgres...), "-d", "postgres", "-At", "-c",
		"SELECT datname FROM pg_database WHERE datallowconn AND datname <> 'template0'")
	if out, err := exec.Command(list[0], list[1:]...).Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if postgresIdentOK(line) {
				names[line] = true
			}
		}
	}
	for db := range names {
		cmd := exec.Command(asPostgres[0], append(asPostgres[1:], "-d", db, "-c", "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;")...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
	}
}

func postgresIdentOK(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func ensureConnectIsolation(postgresBin string) error {
	sql, err := postgres.ConnectIsolationSQL(os.Getenv("POSTGRES_USER"), os.Getenv("POSTGRES_DB"))
	if err != nil {
		return err
	}
	admin, err := postgres.AdminPrivilegesSQL(os.Getenv("POSTGRES_USER"))
	if err != nil {
		return err
	}
	sql = sql + admin
	psql := filepath.Join(filepath.Dir(postgresBin), "psql")
	cmd := exec.Command("setpriv", "--reuid=postgres", "--regid=postgres", "--init-groups", "--inh-caps=-all",
		psql, "-h", "/tmp", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", sql)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("postgres CONNECT isolation: %w", err)
	}
	return nil
}

func firstSet(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func waitLocalPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres did not listen on %s: %w", addr, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
