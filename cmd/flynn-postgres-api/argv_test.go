package main

import (
	"bytes"
	"context"
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

// fakeRestoreTools: pg_restore -l prints an instance dump TOC; a real restore
// records its argv, its -L list and its stdin; psql records its argv.
func fakeRestoreTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	toc := "4; 3079 16434 EXTENSION - pg_stat_statements \n" +
		"3479; 0 0 COMMENT - EXTENSION pg_stat_statements \n" +
		"2; 3079 16386 EXTENSION - uuid-ossp \n" +
		"3481; 0 0 COMMENT - EXTENSION \"uuid-ossp\" \n" +
		"220; 1259 16465 TABLE public r19 app\n" +
		"3472; 0 16465 TABLE DATA public r19 app\n"
	if err := os.WriteFile(filepath.Join(dir, "toc"), []byte(toc), 0600); err != nil {
		t.Fatal(err)
	}
	restore := "#!/bin/sh\n" +
		"if [ \"$1\" = -l ]; then cat '" + dir + "/toc'; exit 0; fi\n" +
		"printf '%s\\n' \"$@\" > '" + dir + "/restore.argv'\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = -L ]; then cp \"$2\" '" + dir + "/restore.list'; fi; shift; done\n" +
		"cat > '" + dir + "/restore.stdin'\n"
	psql := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + dir + "/psql.argv'\n"
	for name, body := range map[string]string{"pg_restore": restore, "psql": psql} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func checkRestoreSkipsExtensions(t *testing.T, dir string) {
	t.Helper()
	argv, err := os.ReadFile(filepath.Join(dir, "restore.argv"))
	if err != nil {
		t.Fatalf("pg_restore never ran a restore: %v", err)
	}
	if !strings.Contains(string(argv), "-L\n") || !strings.Contains(string(argv), "--exit-on-error\n") {
		t.Errorf("restore without a filtered list: %q", argv)
	}
	list, _ := os.ReadFile(filepath.Join(dir, "restore.list"))
	if strings.Contains(string(list), "EXTENSION") || !strings.Contains(string(list), "TABLE DATA public r19 app") {
		t.Errorf("list %q", list)
	}
	if in, _ := os.ReadFile(filepath.Join(dir, "restore.stdin")); string(in) != "PGDMP-archive" {
		t.Errorf("restore stdin %q", in)
	}
	psql, _ := os.ReadFile(filepath.Join(dir, "psql.argv"))
	for _, want := range []string{`CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`, `CREATE EXTENSION IF NOT EXISTS "pg_stat_statements"`} {
		if !strings.Contains(string(psql), want) {
			t.Errorf("psql never ran %s: %q", want, psql)
		}
	}
}

func TestRunRestoreSkipsInstanceExtensions(t *testing.T) {
	dir := fakeRestoreTools(t)
	if err := runRestore(argvURL("appdb"), strings.NewReader("PGDMP-archive")); err != nil {
		t.Fatal(err)
	}
	checkRestoreSkipsExtensions(t, dir)
}

func TestRestoreDatabaseSkipsInstanceExtensions(t *testing.T) {
	dir := fakeRestoreTools(t)
	if err := (execDump{}).RestoreDatabase(context.Background(), argvURL("appdb"), "shop", strings.NewReader("PGDMP-archive")); err != nil {
		t.Fatal(err)
	}
	checkRestoreSkipsExtensions(t, dir)
}
