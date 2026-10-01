package main

import (
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"

	"github.com/randy-girard/flynn-plugin-postgres"
)

// runSQL runs a single statement against a postgres URL. Tests replace it.
var runSQL = func(connURL, query string) (string, error) {
	connURL = strings.TrimSpace(connURL)
	query = strings.TrimSpace(query)
	if connURL == "" || query == "" {
		return "", fmt.Errorf("missing postgres URL or query")
	}
	cmd := exec.Command("psql", connURL, "-v", "ON_ERROR_STOP=1", "-t", "-A", "-c", query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("psql: %s: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

var runDump = func(connURL string, w io.Writer) error {
	connURL = strings.TrimSpace(connURL)
	if connURL == "" {
		return fmt.Errorf("missing postgres URL")
	}
	cmd := exec.Command("pg_dump", "--no-owner", "--format=custom", connURL)
	cmd.Stdout = w
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump: %s: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

var runRestore = func(connURL string, r io.Reader) error {
	connURL = strings.TrimSpace(connURL)
	if connURL == "" {
		return fmt.Errorf("missing postgres URL")
	}
	cmd := exec.Command("pg_restore", "--no-owner", "--clean", "--if-exists", "--dbname", connURL)
	cmd.Stdin = r
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore: %s: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func createDatabaseOnInstance(inst *postgres.Instance, name string) error {
	sql, err := postgres.CreateDatabaseSQL(name)
	if err != nil {
		return err
	}
	if inst == nil {
		return postgres.ErrNotFound
	}
	_, err = runSQL(inst.MaintenanceURL(), sql)
	return err
}

func (h *handler) createLogicalDatabase(id, name string) error {
	name = strings.TrimSpace(name)
	if err := postgres.ValidDatabaseName(name); err != nil {
		return err
	}
	inst, err := h.store.Get(id)
	if err != nil {
		return err
	}
	if inst.ReadOnly || inst.Role == postgres.RoleFollower {
		return postgres.ErrReadOnly
	}
	if h.live() {
		if err := createDatabaseOnInstance(inst, name); err != nil {
			return err
		}
	}
	return h.store.AddDatabase(id, name)
}

func instanceDatabaseURL(inst *postgres.Instance, db string) string {
	if inst == nil {
		return ""
	}
	raw := inst.MaintenanceURL()
	if strings.TrimSpace(db) == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Path = "/" + db
	return u.String()
}

func createUserOnInstance(inst *postgres.Instance, name, password, database string) error {
	sql, err := postgres.CreateUserSQL(name, password)
	if err != nil {
		return err
	}
	if inst == nil {
		return postgres.ErrNotFound
	}
	if _, err := runSQL(inst.MaintenanceURL(), sql); err != nil {
		return err
	}
	database = strings.TrimSpace(database)
	if database == "" {
		return nil
	}
	grant, err := postgres.GrantUserDatabaseSQL(name, database)
	if err != nil {
		return err
	}
	if _, err := runSQL(inst.MaintenanceURL(), grant); err != nil {
		return err
	}
	schema, err := postgres.GrantUserSchemaSQL(name)
	if err != nil {
		return err
	}
	_, err = runSQL(instanceDatabaseURL(inst, database), schema)
	return err
}

func dropUserOnInstance(inst *postgres.Instance, name, database string) error {
	if inst == nil {
		return postgres.ErrNotFound
	}
	cleanup, err := postgres.CleanupUserSQL(name)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var targets []string
	add := func(db string) {
		db = strings.TrimSpace(db)
		if seen[db] {
			return
		}
		seen[db] = true
		targets = append(targets, db)
	}
	add(database)
	for _, db := range inst.Databases {
		add(db.Name)
	}
	for _, db := range listDatabasesOnInstance(inst) {
		add(db)
	}
	if len(targets) == 0 {
		targets = append(targets, "")
	}
	for _, db := range targets {
		if db != "" {
			if rev, err := postgres.RevokeUserDatabaseSQL(name, db); err == nil {
				_, _ = runSQL(inst.MaintenanceURL(), rev)
			}
		}
		_, _ = runSQL(instanceDatabaseURL(inst, db), cleanup)
	}
	sql, err := postgres.DropUserSQL(name)
	if err != nil {
		return err
	}
	_, err = runSQL(inst.MaintenanceURL(), sql)
	return err
}

func listDatabasesOnInstance(inst *postgres.Instance) []string {
	if inst == nil {
		return nil
	}
	out, err := runSQL(inst.MaintenanceURL(),
		`SELECT datname FROM pg_database
WHERE datallowconn AND datname NOT IN ('template0','template1','postgres')
ORDER BY 1`)
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

func listUsersOnInstance(inst *postgres.Instance) []postgres.User {
	if inst == nil {
		return nil
	}
	// Non-superusers cannot use has_database_privilege() on other roles;
	// that JOIN fails the whole query after CREATE ROLE, so the dashboard
	// would only show the instance PGUSER. List login roles, then attach
	// the tenant database name we already know.
	out, err := runSQL(inst.MaintenanceURL(),
		`SELECT rolname FROM pg_roles
WHERE rolcanlogin AND NOT rolsuper AND rolname NOT LIKE 'pg\_%'
ORDER BY 1`)
	if err != nil {
		return nil
	}
	db := ""
	if len(inst.Databases) > 0 {
		db = inst.Databases[0].Name
	}
	var users []postgres.User
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		users = append(users, postgres.User{Name: name, Database: db})
	}
	return users
}

func (h *handler) createUserOnInstance(id, name, password, database string) error {
	name = strings.TrimSpace(name)
	if err := postgres.ValidUserName(name); err != nil {
		return err
	}
	inst, err := h.store.Get(id)
	if err != nil {
		return err
	}
	if inst.ReadOnly || inst.Role == postgres.RoleFollower {
		return postgres.ErrReadOnly
	}
	if inst.AppUser != "" && strings.EqualFold(inst.AppUser, name) {
		return fmt.Errorf("user %s already exists", name)
	}
	if h.live() {
		if err := createUserOnInstance(inst, name, password, database); err != nil {
			return err
		}
	}
	return h.store.AddUser(id, name, password, database)
}

func (h *handler) dropUserOnInstance(id, name, database string) error {
	name = strings.TrimSpace(name)
	if err := postgres.ValidUserName(name); err != nil {
		return err
	}
	inst, err := h.store.Get(id)
	if err != nil {
		return err
	}
	if inst.ReadOnly || inst.Role == postgres.RoleFollower {
		return postgres.ErrReadOnly
	}
	if h.live() {
		if err := dropUserOnInstance(inst, name, database); err != nil {
			return err
		}
		if err := h.store.DropUser(id, name); err != nil && !strings.Contains(err.Error(), "not found") {
			return err
		}
		return nil
	}
	return h.store.DropUser(id, name)
}
