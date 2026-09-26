package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

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
	if err := discoverd.DefaultClient.AddService(service, nil); err != nil && !httphelper.IsObjectExistsError(err) {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return err
	}
	hb, err := discoverd.DefaultClient.RegisterInstance(service, &discoverd.Instance{Addr: ":5432"})
	if err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return err
	}
	shutdown.BeforeExit(func() { hb.Close() })

	return <-exited
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
