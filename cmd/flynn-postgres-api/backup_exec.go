package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	stmts, err := postgres.CreateDatabaseSQL(database)
	if err != nil {
		return err
	}
	create, err := postgres.PsqlStatement(connURL, db, "-c", stmts[0])
	if err != nil {
		return err
	}
	if err := runPG(ctx, create, nil, io.Discard); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}
	// Close the database to PUBLIC whether this call or an earlier one made it.
	var isolate []string
	for _, sql := range stmts[1:] {
		isolate = append(isolate, "-c", sql)
	}
	revoke, err := postgres.PsqlStatement(connURL, db, isolate...)
	if err != nil {
		return err
	}
	if err := runPG(ctx, revoke, nil, io.Discard); err != nil {
		return err
	}
	return restoreArchive(ctx, connURL, database, r)
}

// restoreArchive runs pg_restore (PgRestoreCommand) into database without the
// dump's EXTENSION entries, which the instance login cannot drop or comment
// on. The dump is spooled to a temporary file because it is read twice (TOC,
// then data). Skipped extensions are created IF NOT EXISTS first; one the
// login may not create is left to fail the restore if anything needs it.
func restoreArchive(ctx context.Context, connURL, database string, r io.Reader) error {
	restore, err := postgres.PgRestoreCommand(connURL, database)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "flynn-postgres-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	archive := filepath.Join(dir, "dump")
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if r != nil {
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	var toc bytes.Buffer
	if err := runPG(ctx, postgres.PgRestoreListCommand(archive), nil, &toc); err != nil {
		return err
	}
	list, exts := postgres.RestoreTOCList(toc.String())
	listPath := filepath.Join(dir, "list")
	if err := os.WriteFile(listPath, []byte(list), 0600); err != nil {
		return err
	}
	for _, name := range exts {
		sql, err := postgres.CreateExtensionSQL(name)
		if err != nil {
			continue
		}
		create, err := postgres.PsqlStatement(connURL, database, "-c", sql)
		if err != nil {
			return err
		}
		_ = runPG(ctx, create, nil, io.Discard)
	}
	in, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer in.Close()
	restore.Args = append(restore.Args, "-L", listPath)
	return runPG(ctx, restore, in, io.Discard)
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

// runPG runs one psql, pg_dump, or pg_restore command. Tests replace it.
var runPG = func(ctx context.Context, cmd postgres.PGCommand, stdin io.Reader, stdout io.Writer) error {
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
