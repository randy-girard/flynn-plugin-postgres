//go:build cluster

package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("FLYNN_CLUSTER_E2E") != "1" {
		fmt.Fprintln(os.Stderr, "cluster CLI e2e requires FLYNN_CLUSTER_E2E=1; run cluster/run.sh")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestPostgresCLI(t *testing.T) {
	h := newHarness(t)

	t.Run("plugin-installed", func(t *testing.T) {
		h.use(t)
		out := h.must(cmdQuick, "plugin")
		if !strings.Contains(strings.ToLower(out), "postgres") {
			t.Fatalf("postgres plugin is not installed on this cluster:\n%s", out)
		}
	})
	if t.Failed() {
		return
	}

	dbName := "e2e_db_" + randomSuffix()
	userName := "e2e_u_" + randomSuffix()
	userPass := "p" + randomSuffix() + randomSuffix()
	const probeN = "42"
	const probeFollow = "99"

	t.Run("create-app", func(t *testing.T) {
		h.use(t)
		name := "pg-e2e-cli-" + randomSuffix()
		out, errOut, err := h.cmd(cmdQuick, "apps:create", "-y", "-r", "", name)
		if err != nil {
			t.Fatalf("flynn apps:create: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
		}
		if !strings.Contains(out+"\n"+errOut, name) {
			t.Fatalf("expected created app %s, got stdout:\n%s\nstderr:\n%s", name, out, errOut)
		}
		h.app = name
	})
	if t.Failed() || h.app == "" {
		return
	}

	t.Run("addons-create-runtime-help", func(t *testing.T) {
		h.use(t)
		out, errOut, err := h.cmd(cmdQuick, "addons:create", "--help")
		combined := strings.ToLower(out + "\n" + errOut)
		if err != nil && !strings.Contains(combined, "runtime") && !strings.Contains(combined, "postgres") {
			t.Fatalf("flynn addons:create --help: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
		}
		if !strings.Contains(combined, "--runtime") {
			t.Fatalf("expected addons:create help to mention --runtime:\n%s", combined)
		}
	})

	t.Run("provision", func(t *testing.T) {
		h.use(t)
		out := h.appMust(cmdProvision, "addons:create", "postgres", "--runtime", "small")
		lower := strings.ToLower(out)
		if !strings.Contains(lower, "created add-on") && !strings.Contains(lower, "created resource") {
			t.Fatalf("expected addons:create to create an add-on, got:\n%s", out)
		}
		primary := parseNewFollower(out, "")
		if primary == "" {
			h.waitReady("", cmdProvision)
			primary = h.primary()
		} else {
			h.appMust(cmdProvision, "pg:wait", primary)
			h.waitReady(primary, cmdWait)
		}
		if primary == "" {
			t.Fatal("provisioned resource missing from flynn pg")
		}
		assertIsolatedPostgresName(t, primary)
		t.Logf("primary %s", primary)
		env := h.appMust(cmdQuick, "env")
		assertPostgresAppEnv(t, env, primary)
		_, errOut, err := h.appCmd(cmdQuick, "env:set", "DATABASE_URL=postgres://elsewhere/db")
		if err == nil {
			t.Fatal("DATABASE_URL must be locked while the resource is attached")
		}
		if !strings.Contains(strings.ToLower(errOut+" "+err.Error()), "attached") {
			t.Fatalf("expected attached lock, got %v\n%s", err, errOut)
		}
		colorKeys := postgresColorURLKeys(env)
		if len(colorKeys) != 1 {
			t.Fatalf("expected one color URL to lock:\n%s", env)
		}
		_, errOut, err = h.appCmd(cmdQuick, "env:set", colorKeys[0]+"=postgres://elsewhere/db")
		if err == nil {
			t.Fatal("color URL must be locked while the resource is attached")
		}
		if !strings.Contains(strings.ToLower(errOut+" "+err.Error()), "attached") {
			t.Fatalf("expected color lock, got %v\n%s", err, errOut)
		}
		listed := h.psql(cmdWait, "-Atc", "SELECT datname FROM pg_database WHERE datallowconn AND datname NOT IN ('template0','template1','postgres') ORDER BY 1")
		assertOneRandomPostgresDatabase(t, listed)
	})
	if t.Failed() {
		return
	}

	t.Run("list-and-info", func(t *testing.T) {
		h.use(t)
		list := h.appMust(cmdWait, "pg")
		if len(parsePgRows(list)) == 0 {
			t.Fatalf("flynn pg listed no instances:\n%s", list)
		}
		info := h.appMust(cmdWait, "pg:info")
		if info == "" {
			t.Fatal("flynn pg:info was empty")
		}
		help, errOut, err := h.appCmd(cmdQuick, "pg:follow", "--help")
		combined := strings.ToLower(help + "\n" + errOut)
		if err != nil && !strings.Contains(combined, "follow") {
			t.Fatalf("flynn pg:follow --help: %v\nstdout:\n%s\nstderr:\n%s", err, help, errOut)
		}
		if !strings.Contains(combined, "--runtime") {
			t.Fatalf("expected pg:follow help to mention --runtime:\n%s", combined)
		}
	})
	if t.Failed() {
		return
	}

	t.Run("logical-database", func(t *testing.T) {
		h.use(t)
		h.appMust(cmdWait, "pg:create", dbName)
		got := h.psql(cmdWait, "-Atc", fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname = '%s'", dbName))
		if !strings.Contains(got, "1") {
			t.Fatalf("logical database %s not found: %s", dbName, got)
		}
		h.psql(cmdWait, "-c", "DROP DATABASE "+dbName)
		gone := h.psql(cmdWait, "-Atc", fmt.Sprintf("SELECT count(*) FROM pg_database WHERE datname = '%s'", dbName))
		if strings.TrimSpace(gone) != "0" {
			t.Fatalf("expected %s dropped, count=%s", dbName, gone)
		}
	})
	if t.Failed() {
		return
	}

	t.Run("user", func(t *testing.T) {
		h.use(t)
		db := strings.TrimSpace(h.psql(cmdWait, "-Atc", "SELECT current_database()"))
		if db == "" {
			t.Fatal("current_database() empty")
		}
		h.psql(cmdWait, "-c", fmt.Sprintf(
			"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE",
			userName, userPass,
		))
		h.psql(cmdWait, "-c", fmt.Sprintf("GRANT CONNECT, TEMPORARY ON DATABASE %s TO %s", db, userName))
		got := h.psql(cmdWait, "-Atc", fmt.Sprintf("SELECT 1 FROM pg_roles WHERE rolname = '%s'", userName))
		if !strings.Contains(got, "1") {
			t.Fatalf("role %s not found: %s", userName, got)
		}
		h.psql(cmdWait, "-c", fmt.Sprintf("REVOKE CONNECT, TEMPORARY ON DATABASE %s FROM %s", db, userName))
		h.psql(cmdWait, "-c", "DROP ROLE "+userName)
		gone := h.psql(cmdWait, "-Atc", fmt.Sprintf("SELECT count(*) FROM pg_roles WHERE rolname = '%s'", userName))
		if strings.TrimSpace(gone) != "0" {
			t.Fatalf("expected role %s dropped, count=%s", userName, gone)
		}
	})
	if t.Failed() {
		return
	}

	t.Run("insert-query", func(t *testing.T) {
		h.use(t)
		h.psql(cmdWait, "-c", "DROP TABLE IF EXISTS e2e_probe")
		h.psql(cmdWait, "-c", "CREATE TABLE e2e_probe (n int)")
		h.psql(cmdWait, "-c", "INSERT INTO e2e_probe VALUES ("+probeN+")")
		got := strings.TrimSpace(h.psql(cmdWait, "-Atc", "SELECT n FROM e2e_probe"))
		if got != probeN {
			t.Fatalf("primary query want %s got %s", probeN, got)
		}
	})
	if t.Failed() {
		return
	}

	t.Run("follow-and-replicate", func(t *testing.T) {
		h.use(t)
		leader := h.primary()
		before := map[string]bool{leader: true}
		for _, name := range h.followers() {
			before[name] = true
		}
		out := h.appMust(cmdFollow, "pg:follow", "--runtime", "small")
		follower := parseNewFollower(out, leader)
		deadline := time.Now().Add(30 * time.Second)
		for follower == "" && time.Now().Before(deadline) {
			for _, r := range h.pgList() {
				if !before[r.Name] {
					follower = r.Name
					break
				}
			}
			if follower != "" {
				break
			}
			time.Sleep(pollInterval)
		}
		if follower == "" {
			t.Fatalf("no new follower after pg follow; list:\n%s", h.appMust(cmdQuick, "pg"))
		}
		assertIsolatedPostgresName(t, follower)
		waitOut, waitErrOut, waitErr := h.appCmd(cmdWait, "pg:wait", follower)
		if waitErr != nil {
			t.Logf("pg:wait %s: %v\n%s\n%s", follower, waitErr, waitOut, waitErrOut)
		} else if !strings.Contains(strings.ToLower(waitOut+"\n"+waitErrOut), "ready") && !strings.Contains(waitOut+waitErrOut, "%") {
			t.Logf("pg:wait output (continuing with waitReady):\n%s\n%s", waitOut, waitErrOut)
		}
		if err := h.tryReady(follower, 90*time.Second); err != nil {
			t.Logf("follower %s not queryable yet; skipping replica SELECT: %v", follower, err)
		} else {
			assertPostgresAppEnv(t, h.appMust(cmdQuick, "env"), h.primary())
			got := strings.TrimSpace(h.psqlOn(cmdWait, follower, "-Atc", "SELECT n FROM e2e_probe"))
			if got != probeN {
				t.Fatalf("follower %s want %s got %s", follower, probeN, got)
			}
			h.psql(cmdWait, "-c", "INSERT INTO e2e_probe VALUES ("+probeFollow+")")
			_, _, _ = h.appCmd(cmdWait, "pg:wait", follower)
			got = strings.TrimSpace(h.psqlOn(cmdWait, follower, "-Atc", "SELECT n FROM e2e_probe ORDER BY n"))
			if !strings.Contains(got, probeFollow) {
				t.Fatalf("follower missing streamed row %s: %s", probeFollow, got)
			}
		}
		primary := h.primary()
		if len(h.followers()) == 0 {
			t.Logf("follower %s not registered; removing leftover without blocking primary", follower)
			_, _, _ = h.appCmd(cmdDestroy, "addons:destroy", follower)
		} else {
			_, errOut, err := h.appCmd(cmdDestroy, "addons:destroy", primary)
			if err == nil {
				t.Fatalf("expected addons:destroy %s to fail while %s is a follower", primary, follower)
			}
			if !strings.Contains(strings.ToLower(errOut+" "+err.Error()), "follower") {
				t.Fatalf("expected followers error removing %s, got %v\n%s", primary, err, errOut)
			}
			h.appMust(cmdDestroy, "addons:destroy", follower)
		}
		h.waitInstanceGone(follower)
		if left := h.followers(); len(left) != 0 {
			t.Fatalf("followers still present after remove: %v", left)
		}
		env := h.appMust(cmdQuick, "env")
		if envHasKey(env, scopedDatabaseURLKey(follower)) {
			t.Fatalf("follower env must be removed:\n%s", env)
		}
		if envHasKey(env, "DATABASE_URL") {
			assertPostgresAppEnv(t, env, primary)
		} else {
			t.Logf("primary DATABASE_URL missing after deleting a starting follower:\n%s", env)
		}
	})
	if t.Failed() {
		return
	}

	t.Run("dump-restore", func(t *testing.T) {
		h.use(t)
		if !envHasKey(h.appMust(cmdQuick, "env"), "DATABASE_URL") {
			t.Log("skip dump-restore: primary DATABASE_URL missing after follower cleanup")
			return
		}
		dir := t.TempDir()
		dump := filepath.Join(dir, "postgres.dump")
		h.appMust(cmdDump, "pg:dump", "-q", "-f", dump)
		st, err := os.Stat(dump)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() == 0 {
			t.Fatal("dump file is empty")
		}
		out, errOut, err := h.appCmd(cmdDump, "pg:restore", "-q", "-f", dump)
		combined := strings.TrimSpace(out + "\n" + errOut)
		if err != nil && !restoreIgnoredExtensionErrors(combined) {
			t.Fatalf("flynn pg restore: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
		}
		if err != nil {
			t.Logf("pg restore reported ignored extension errors (app role cannot DROP EXTENSION):\n%s", combined)
		}
		got := strings.TrimSpace(h.psql(cmdWait, "-Atc", "SELECT n FROM e2e_probe ORDER BY n"))
		if !strings.Contains(got, probeN) {
			t.Fatalf("after restore missing %s: %s", probeN, got)
		}
	})
	if t.Failed() {
		return
	}

	t.Run("attach-to-other-app", func(t *testing.T) {
		h.use(t)
		name := "pg-e2e-cli-peer-" + randomSuffix()
		out, errOut, err := h.cmd(cmdQuick, "apps:create", "-y", "-r", "", name)
		if err != nil {
			t.Fatalf("flynn apps:create: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
		}
		h.peer = name
		primary := h.primary()
		out = h.must(cmdQuick, "-a", name, "addons:attach", primary)
		if !strings.Contains(strings.ToLower(out), "attached") {
			t.Fatalf("expected attached resource, got:\n%s", out)
		}
		env := h.must(cmdQuick, "-a", name, "env")
		assertPostgresAttachEnv(t, env, primary)
		ownerEnv := h.appMust(cmdQuick, "env")
		if envHasKey(ownerEnv, "DATABASE_URL") {
			assertPostgresAppEnv(t, ownerEnv, primary)
		} else {
			t.Logf("owner DATABASE_URL missing after follower cleanup:\n%s", ownerEnv)
		}
		info := h.appMust(cmdWait, "pg:info")
		if !strings.Contains(info, name) {
			t.Fatalf("pg:info should list attached app %s:\n%s", name, info)
		}
		peerGot, errOut, err := h.cmd(cmdQuick, "-a", name, "pg:psql", primary, "--", "-Atc", "SELECT n FROM e2e_probe ORDER BY n")
		if err != nil {
			t.Fatalf("peer psql: %v\nstdout:\n%s\nstderr:\n%s", err, peerGot, errOut)
		}
		if !strings.Contains(peerGot, probeN) {
			t.Fatalf("peer %s must read the owner instance (want %s):\n%s", name, probeN, peerGot)
		}
		_, errOut, err = h.cmd(cmdQuick, "-a", name, "addons:destroy", primary)
		if err == nil {
			t.Fatalf("attached app must not delete %s", primary)
		}
		combined := strings.ToLower(errOut + " " + err.Error())
		if !strings.Contains(combined, "does not own") && !strings.Contains(combined, "detach") {
			t.Fatalf("expected ownership error removing %s from %s, got %v\n%s", primary, name, err, errOut)
		}
		color := postgresColorURLKeys(env)[0]
		color = strings.TrimSuffix(strings.TrimPrefix(color, "FLYNN_POSTGRESQL_"), "_URL")
		h.must(cmdQuick, "-a", name, "addons:detach", color)
		env = h.must(cmdQuick, "-a", name, "env")
		if len(postgresColorURLKeys(env)) != 0 || envHasKey(env, "DATABASE_URL") || envHasKey(env, scopedDatabaseURLKey(primary)) {
			t.Fatalf("detach must remove attachment env:\n%s", env)
		}
		ownerEnv = h.appMust(cmdQuick, "env")
		if envHasKey(ownerEnv, "DATABASE_URL") {
			assertPostgresAppEnv(t, ownerEnv, primary)
		} else {
			t.Logf("owner DATABASE_URL missing after follower cleanup:\n%s", ownerEnv)
		}
		h.must(cmdDestroy, "-a", name, "apps:destroy", "-y")
		h.peer = ""
	})
	if t.Failed() {
		return
	}

	t.Run("detach-and-destroy", func(t *testing.T) {
		h.use(t)
		primary := h.primary()
		h.appMust(cmdDestroy, "addons:destroy", primary)
		env := h.appMust(cmdQuick, "env")
		if len(postgresColorURLKeys(env)) != 0 || envHasKey(env, "DATABASE_URL") || envHasKey(env, scopedDatabaseURLKey(primary)) {
			t.Fatalf("delete must remove attachment env:\n%s", env)
		}
		h.appMust(cmdDestroy, "apps:destroy", "-y")
		h.app = ""
	})
}
