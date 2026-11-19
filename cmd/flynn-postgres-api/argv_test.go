package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const argvTestPassword = "s3cret-argv-pw"

// fakePGTools puts psql, pg_dump and pg_restore shims first on PATH. Each shim
// records its argv (one per line) and the PGPASSWORD it saw, then prints "ok".
func fakePGTools(t *testing.T) (argvFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	envFile = filepath.Join(dir, "env")
	script := "#!/bin/sh\n" +
		"for a in \"$0\" \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argvFile + "'\n" +
		"printf '%s' \"$PGPASSWORD\" > '" + envFile + "'\n" +
		"cat >/dev/null\n" +
		"echo ok\n"
	for _, name := range []string{"psql", "pg_dump", "pg_restore"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PGPASSWORD", "")
	return argvFile, envFile
}

func readArgv(t *testing.T, argvFile, envFile string) ([]string, string) {
	t.Helper()
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("tool not run: %v", err)
	}
	env, _ := os.ReadFile(envFile)
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n"), string(env)
}

func checkOffArgv(t *testing.T, argv []string, env, wantDB string) {
	t.Helper()
	line := strings.Join(argv, " ")
	if strings.Contains(line, argvTestPassword) {
		t.Errorf("password on argv: %s", strings.ReplaceAll(line, argvTestPassword, "<PASSWORD>"))
	}
	if env != argvTestPassword {
		t.Errorf("PGPASSWORD not set from the URL (len %d)", len(env))
	}
	if !strings.Contains(line, "postgres://flynn@pg-a.discoverd:5432/"+wantDB+"?") {
		t.Errorf("URL user, host or database lost: %s", strings.ReplaceAll(line, argvTestPassword, "<PASSWORD>"))
	}
	if !strings.Contains(line, "sslmode=verify-full") {
		t.Errorf("TLS query lost: %s", line)
	}
}

func argvURL(db string) string {
	return "postgres://flynn:" + argvTestPassword + "@pg-a.discoverd:5432/" + db + "?sslmode=verify-full&sslrootcert=%2Ftmp%2Fca.pem"
}

func TestRunSQLKeepsPasswordOffArgv(t *testing.T) {
	argvFile, envFile := fakePGTools(t)
	// The catalog database "postgres" is a legal target for maintenance SQL.
	for _, db := range []string{"postgres", "appdb"} {
		out, err := runSQL(argvURL(db), "SELECT 1")
		if err != nil {
			t.Fatalf("%s: %v", db, err)
		}
		if strings.TrimSpace(out) != "ok" {
			t.Errorf("%s: stdout %q", db, out)
		}
		argv, env := readArgv(t, argvFile, envFile)
		checkOffArgv(t, argv, env, db)
		if argv[len(argv)-1] != "SELECT 1" {
			t.Errorf("%s: query not last: %v", db, argv)
		}
	}
}

func TestRunDumpKeepsPasswordOffArgv(t *testing.T) {
	argvFile, envFile := fakePGTools(t)
	var buf bytes.Buffer
	if err := runDump(argvURL("appdb"), &buf); err != nil {
		t.Fatal(err)
	}
	argv, env := readArgv(t, argvFile, envFile)
	checkOffArgv(t, argv, env, "appdb")
	if filepath.Base(argv[0]) != "pg_dump" {
		t.Errorf("tool %s", argv[0])
	}
	if strings.TrimSpace(buf.String()) != "ok" {
		t.Errorf("dump body %q", buf.String())
	}
}

func TestRunRestoreKeepsPasswordOffArgv(t *testing.T) {
	argvFile, envFile := fakePGTools(t)
	if err := runRestore(argvURL("appdb"), strings.NewReader("DUMP")); err != nil {
		t.Fatal(err)
	}
	argv, env := readArgv(t, argvFile, envFile)
	checkOffArgv(t, argv, env, "appdb")
	if filepath.Base(argv[0]) != "pg_restore" {
		t.Errorf("tool %s", argv[0])
	}
}

func TestRunSQLRedactsPasswordInErrors(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"connection to $1 failed: $PGPASSWORD\" >&2\nexit 2\n"
	if err := os.WriteFile(filepath.Join(dir, "psql"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := runSQL(argvURL("appdb"), "SELECT 1")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), argvTestPassword) {
		t.Error("password in error text")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error lost psql output: %v", err)
	}
}
