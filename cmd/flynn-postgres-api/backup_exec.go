package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/randy-girard/flynn-plugin-postgres"
)

const listBackupDatabasesSQL = `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate AND datname <> 'postgres' ORDER BY datname`

type execDump struct{}

func (execDump) ListDatabases(ctx context.Context, connURL string) ([]string, error) {
	db, err := connDatabase(connURL)
	if err != nil {
		return nil, err
	}
	cmd, err := postgres.PsqlStatement(connURL, db, "-t", "-A", "-c", listBackupDatabasesSQL)
	if err != nil {
		return nil, err
	}
	var stdout bytes.Buffer
	if err := runPG(ctx, cmd, nil, &stdout); err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

func (execDump) DumpDatabase(ctx context.Context, connURL, database string, w io.Writer) error {
	cmd, err := postgres.PgDumpCommand(connURL, database)
	if err != nil {
		return err
	}
	return runPG(ctx, cmd, nil, w)
}

func (execDump) RestoreDatabase(ctx context.Context, connURL, database string, r io.Reader) error {
	db, err := connDatabase(connURL)
	if err != nil {
		return err
	}
	sql, err := postgres.CreateDatabaseSQL(database)
	if err != nil {
		return err
	}
	create, err := postgres.PsqlStatement(connURL, db, "-c", sql)
	if err != nil {
		return err
	}
	if err := runPG(ctx, create, nil, io.Discard); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}
	cmd, err := postgres.PgRestoreCommand(connURL, database)
	if err != nil {
		return err
	}
	return runPG(ctx, cmd, r, io.Discard)
}

func connDatabase(connURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(connURL))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("missing postgres URL")
	}
	db := strings.Trim(u.Path, "/")
	if db == "" {
		return "", fmt.Errorf("missing postgres database")
	}
	return db, nil
}

func runPG(ctx context.Context, cmd postgres.PGCommand, stdin io.Reader, stdout io.Writer) error {
	if len(cmd.Args) == 0 {
		return fmt.Errorf("missing postgres command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c := exec.CommandContext(ctx, cmd.Args[0], cmd.Args[1:]...)
	c.Env = withoutEnvKey(os.Environ(), "PGPASSWORD")
	if cmd.Password != "" {
		c.Env = append(c.Env, "PGPASSWORD="+cmd.Password)
	}
	c.Stdin = stdin
	c.Stdout = stdout
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		msg := postgres.RedactSecret(strings.TrimSpace(stderr.String()), cmd.Password)
		return fmt.Errorf("%s: %s: %s", cmd.Args[0], err, msg)
	}
	return nil
}

func withoutEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}
