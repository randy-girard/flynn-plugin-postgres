//go:build cluster

package cli_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func scopedDatabaseURLKey(name string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), "-", "_")) + "_DATABASE_URL"
}

func postgresColorURLKeys(env string) []string {
	var keys []string
	for _, line := range strings.Split(env, "\n") {
		k, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "FLYNN_POSTGRESQL_") && strings.HasSuffix(k, "_URL") {
			keys = append(keys, k)
		}
	}
	return keys
}

func envHasKey(env, key string) bool {
	prefix := key + "="
	return strings.HasPrefix(env, prefix) || strings.Contains(env, "\n"+prefix)
}

func envValue(env, key string) string {
	prefix := key + "="
	for _, line := range strings.Split(env, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

func assertPostgresAppEnvCommon(t *testing.T, env, resource string) {
	t.Helper()
	if resource != "" && envHasKey(env, scopedDatabaseURLKey(resource)) {
		t.Fatalf("postgres must not set instance-named URL %s:\n%s", scopedDatabaseURLKey(resource), env)
	}
	for _, k := range []string{
		"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE",
		"POSTGRES_URL", "POSTGRES_DB", "POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_HOST",
		"FLYNN_POSTGRES", "POSTGRES_ROLE",
	} {
		if envHasKey(env, k) {
			t.Fatalf("app must not set %s:\n%s", k, env)
		}
	}
}

func assertPostgresAppEnv(t *testing.T, env, resource string) {
	t.Helper()
	if !envHasKey(env, "DATABASE_URL") {
		t.Fatalf("new provision must set DATABASE_URL:\n%s", env)
	}
	if keys := postgresColorURLKeys(env); len(keys) != 1 {
		t.Fatalf("provision must set exactly one color URL:\n%s", env)
	}
	assertPostgresAppEnvCommon(t, env, resource)
}

func assertPostgresAttachEnv(t *testing.T, env, resource string) {
	t.Helper()
	if envHasKey(env, "DATABASE_URL") {
		t.Fatalf("attach of existing resource must not set DATABASE_URL:\n%s", env)
	}
	if keys := postgresColorURLKeys(env); len(keys) != 1 {
		t.Fatalf("attach must set exactly one color URL:\n%s", env)
	}
	assertPostgresAppEnvCommon(t, env, resource)
}

func assertPostgresFollowerAppEnv(t *testing.T, env, resource string) {
	t.Helper()
	if !envHasKey(env, "DATABASE_URL") {
		t.Fatalf("primary DATABASE_URL missing after follower:\n%s", env)
	}
	if keys := postgresColorURLKeys(env); len(keys) != 1 {
		t.Fatalf("follower must add exactly one color URL:\n%s", env)
	}
	assertPostgresAppEnvCommon(t, env, resource)
}

var randomPostgresDB = regexp.MustCompile(`^[a-z][a-z0-9]{11}$`)

func assertOneRandomPostgresDatabase(t *testing.T, listed string) {
	t.Helper()
	var names []string
	for _, line := range strings.Split(listed, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	if len(names) != 1 {
		t.Fatalf("expected one application database, got %v", names)
	}
	if !randomPostgresDB.MatchString(names[0]) {
		t.Fatalf("first database name must be random alphanumeric, got %q", names[0])
	}
}

var pgResourceName = regexp.MustCompile(`\b(postgresql-[a-z0-9]+(?:-[a-z0-9]+)*-[0-9]{5,8}|pg-[a-z]+-[a-z]{6,8})\b`)

const (
	cmdQuick     = 20 * time.Second
	cmdProvision = 5 * time.Minute // matches plugin instanceReadyTimeout (initdb + TLS)
	cmdFollow    = 5 * time.Minute
	cmdWait      = 45 * time.Second
	cmdDump      = 20 * time.Second
	cmdDestroy   = 45 * time.Second
	pollInterval = time.Second
)

func flynnBin() string {
	if v := strings.TrimSpace(os.Getenv("FLYNN")); v != "" {
		return v
	}
	return "flynn"
}

func randomSuffix() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	}
	return hex.EncodeToString(b)
}

type pgRow struct {
	Name string
	Role string
}

type harness struct {
	root *testing.T
	t    *testing.T
	app  string
	peer string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{root: t, t: t}
	t.Cleanup(func() {
		h.t = h.root
		h.destroyBestEffort()
	})
	return h
}

func (h *harness) use(t *testing.T) {
	h.t = t
}

func (h *harness) cmd(timeout time.Duration, args ...string) (string, string, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, flynnBin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

func (h *harness) must(timeout time.Duration, args ...string) string {
	h.t.Helper()
	out, errOut, err := h.cmd(timeout, args...)
	if err != nil {
		h.t.Fatalf("flynn %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, out, errOut)
	}
	if errOut != "" {
		h.t.Logf("flynn %s stderr:\n%s", strings.Join(args, " "), errOut)
	}
	// Flynn CLI status lines (Created, Deleted, …) go to stderr via log.Printf.
	return strings.TrimSpace(out + "\n" + errOut)
}

func (h *harness) appMust(timeout time.Duration, args ...string) string {
	h.t.Helper()
	all := append([]string{"-a", h.app}, args...)
	return h.must(timeout, all...)
}

func (h *harness) appCmd(timeout time.Duration, args ...string) (string, string, error) {
	h.t.Helper()
	all := append([]string{"-a", h.app}, args...)
	return h.cmd(timeout, all...)
}

func parseNewFollower(out, leader string) string {
	leader = strings.TrimSpace(leader)
	for _, m := range pgResourceName.FindAllStringSubmatch(out, -1) {
		if len(m) > 1 && m[1] != "" && m[1] != leader {
			return m[1]
		}
	}
	return ""
}

func restoreIgnoredExtensionErrors(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "errors ignored on restore") &&
		strings.Contains(lower, "must be owner of extension")
}

func parsePgRows(out string) []pgRow {
	var rows []pgRow
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		m := pgResourceName.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		role := "primary"
		lower := strings.ToLower(line)
		if strings.Contains(lower, "follower") {
			role = "follower"
		}
		rows = append(rows, pgRow{Name: name, Role: role})
	}
	return rows
}

func (h *harness) pgList() []pgRow {
	h.t.Helper()
	out := h.appMust(cmdQuick, "pg")
	return parsePgRows(out)
}

func (h *harness) primary() string {
	h.t.Helper()
	for _, r := range h.pgList() {
		if r.Role != "follower" {
			return r.Name
		}
	}
	h.t.Fatal("no primary postgres resource")
	return ""
}

func (h *harness) followers() []string {
	h.t.Helper()
	var names []string
	for _, r := range h.pgList() {
		if r.Role == "follower" {
			names = append(names, r.Name)
		}
	}
	return names
}

func (h *harness) psql(timeout time.Duration, extra ...string) string {
	h.t.Helper()
	if name := h.psqlDefaultResource(); name != "" {
		return h.psqlOn(timeout, name, extra...)
	}
	return h.psqlStdout(timeout, append([]string{"pg", "psql", "--"}, extra...)...)
}

// psqlDefaultResource is the instance to pass to pg:psql when more than one
// resource is attached (the CLI requires the name). Empty means omit it.
func (h *harness) psqlDefaultResource() string {
	h.t.Helper()
	rows := h.pgList()
	if len(rows) < 2 {
		return ""
	}
	for _, r := range rows {
		if r.Role != "follower" {
			return r.Name
		}
	}
	return ""
}

func (h *harness) psqlOn(timeout time.Duration, resource string, extra ...string) string {
	h.t.Helper()
	return h.psqlStdout(timeout, append([]string{"pg", "psql", resource, "--"}, extra...)...)
}

func (h *harness) psqlStdout(timeout time.Duration, args ...string) string {
	h.t.Helper()
	out, errOut, err := h.appCmd(timeout, args...)
	if err != nil {
		h.t.Fatalf("flynn %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, out, errOut)
	}
	if errOut != "" {
		h.t.Logf("flynn %s stderr:\n%s", strings.Join(args, " "), errOut)
	}
	return out
}

func (h *harness) waitReady(resource string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		args := []string{"pg", "psql", "--", "-Atc", "SELECT 1"}
		if resource != "" {
			args = []string{"pg", "psql", resource, "--", "-Atc", "SELECT 1"}
		}
		out, errOut, err := h.appCmd(cmdQuick, args...)
		if err == nil && strings.Contains(out, "1") {
			return
		}
		last = fmt.Errorf("%v\nstdout:%s\nstderr:%s", err, out, errOut)
		h.t.Logf("waiting for postgres %s: %v", resource, last)
		time.Sleep(pollInterval)
	}
	h.t.Fatalf("postgres %s on %s not ready: %v", resource, h.app, last)
}

func (h *harness) destroyBestEffort() {
	if h.peer != "" {
		peer := h.peer
		h.peer = ""
		if _, errOut, err := h.cmd(cmdDestroy, "-a", peer, "apps:destroy", "-y"); err != nil {
			h.t.Logf("cleanup destroy peer %s: %v %s", peer, err, errOut)
		}
	}
	if h.app == "" {
		return
	}
	app := h.app
	rows := parsePgRows(func() string {
		out, _, _ := h.cmd(cmdQuick, "-a", app, "pg")
		return out
	}())
	var followers, primaries []string
	for _, r := range rows {
		if r.Role == "follower" {
			followers = append(followers, r.Name)
		} else {
			primaries = append(primaries, r.Name)
		}
	}
	for _, name := range followers {
		_, errOut, err := h.cmd(cmdDestroy, "-a", app, "resource:remove", name)
		if err != nil {
			h.t.Logf("cleanup remove follower %s: %v %s", name, err, errOut)
		}
	}
	for _, name := range primaries {
		_, errOut, err := h.cmd(cmdDestroy, "-a", app, "resource:remove", name)
		if err != nil {
			h.t.Logf("cleanup remove primary %s: %v %s", name, err, errOut)
		}
	}
	_, errOut, err := h.cmd(cmdDestroy, "-a", app, "apps:destroy", "-y")
	if err != nil {
		h.t.Logf("cleanup destroy %s: %v %s", app, err, errOut)
	}
}
