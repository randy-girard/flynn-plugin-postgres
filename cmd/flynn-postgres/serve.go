package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	if _, err := os.Stat("/data/standby.signal"); err != nil && os.Getenv("POSTGRES_PRIMARY_URL") == "" {
		if err := ensureConnectIsolation(bin); err != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			return err
		}
	}
	disc := postgres.NewDiscoverdClient()
	if err := disc.AddService(service, nil); err != nil && !httphelper.IsObjectExistsError(err) {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return postgres.WrapDiscoverdAuth(err)
	}
	hb, err := disc.RegisterInstance(service, &discoverd.Instance{
		Addr: ":5432",
		Meta: map[string]string{"flynn-datastore": "true"},
	})
	if err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return postgres.WrapDiscoverdAuth(err)
	}
	shutdown.BeforeExit(func() { hb.Close() })

	return <-exited
}

func ensurePgStatStatements(postgresBin string) {
	db := os.Getenv("POSTGRES_DB")
	if db == "" {
		db = "postgres"
	}
	for _, c := range db {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return
		}
	}
	psql := filepath.Join(filepath.Dir(postgresBin), "psql")
	cmd := exec.Command("setpriv", "--reuid=postgres", "--regid=postgres", "--init-groups", "--inh-caps=-all",
		psql, "-h", "/tmp", "-d", db, "-c", "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
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
