package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
)

// runSQL runs a single statement against a postgres URL. The password goes
// to psql in PGPASSWORD, never on argv. Tests replace it.
var runSQL = func(connURL, query string) (string, error) {
	connURL = strings.TrimSpace(connURL)
	query = strings.TrimSpace(query)
	if connURL == "" || query == "" {
		return "", fmt.Errorf("missing postgres URL or query")
	}
	cmd, err := postgres.PsqlURLStatement(connURL, "-t", "-A", "-c", query)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := runPG(context.Background(), cmd, nil, &out); err != nil {
		return "", err
	}
	return out.String(), nil
}

// runDump is the dashboard dump of the instance URL's database
// (PgDumpCommand: password in PGPASSWORD, not on argv).
var runDump = func(connURL string, w io.Writer) error {
	db, err := connDatabase(connURL)
	if err != nil {
		return err
	}
	cmd, err := postgres.PgDumpCommand(connURL, db)
	if err != nil {
		return err
	}
	return runPG(context.Background(), cmd, nil, w)
}

// runRestore is the dashboard restore into the instance URL's database
// (restoreArchive: PgRestoreCommand, password in PGPASSWORD, not on argv).
var runRestore = func(connURL string, r io.Reader) error {
	db, err := connDatabase(connURL)
	if err != nil {
		return err
	}
	return restoreArchive(context.Background(), connURL, db, r)
}

func createDatabaseOnInstance(inst *postgres.Instance, name string) error {
	stmts, err := postgres.CreateDatabaseSQL(name)
	if err != nil {
		return err
	}
	if inst == nil {
		return postgres.ErrNotFound
	}
	if _, err := runSQL(inst.MaintenanceURL(), stmts[0]); err != nil {
		return err
	}
	// A database still open to PUBLIC is worse than no database: drop it.
	for _, sql := range stmts[1:] {
		if _, err := runSQL(inst.MaintenanceURL(), sql); err != nil {
			_, _ = runSQL(inst.MaintenanceURL(), "DROP DATABASE IF EXISTS "+name)
			return err
		}
	}
	return nil
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
	names, _ := queryDatabasesOnInstance(inst)
	return names
}

// queryDatabasesOnInstance lists the instance's tenant databases and reports
// whether the listing itself failed, so a caller can tell "none" from "unknown".
func queryDatabasesOnInstance(inst *postgres.Instance) ([]string, error) {
	if inst == nil {
		return nil, postgres.ErrNotFound
	}
	out, err := runSQL(inst.MaintenanceURL(),
		`SELECT datname FROM pg_database
WHERE datallowconn AND datname NOT IN ('template0','template1','postgres')
ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

func listUsersOnInstance(inst *postgres.Instance) []postgres.User {
	if inst == nil {
		return nil
	}
	// Non-superusers cannot use has_database_privilege() on other roles;
	// that JOIN fails the whole query after CREATE ROLE, so the dashboard
	// would only show the instance PGUSER. List login roles here; collectUsers
	// fills Database from the store grant so one username is not repeated
	// once per logical database.
	out, err := runSQL(inst.MaintenanceURL(),
		`SELECT rolname FROM pg_roles
WHERE rolcanlogin AND NOT rolsuper AND rolname NOT LIKE 'pg\_%'
ORDER BY 1`)
	if err != nil {
		return nil
	}
	var users []postgres.User
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		users = append(users, postgres.User{Name: name})
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
	if h.store.IsFollowerReader(name) {
		return fmt.Errorf("cannot drop %s: it is a follower's login", name)
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

// createFollowerReader creates the follower's own read-only login on its
// leader (NEW-06). Streaming replication copies pg_authid, so the role must
// exist on the leader; it is created before the follower's base backup so the
// copy already has it. It runs as the leader's app role (CREATEROLE), which
// then holds ADMIN on the new role and can drop it later.
//
// Every statement is idempotent, so a leader that cannot be reached (psql
// connection failure) is retried for followerReaderRetryTimeout; after that
// the error wraps errLeaderUnavailable and the API answers a retryable 503.
func createFollowerReader(leader, fol *postgres.Instance) error {
	if leader == nil || fol == nil {
		return postgres.ErrNotFound
	}
	deadline := time.Now().Add(followerReaderRetryTimeout)
	for {
		err := createFollowerReaderOnce(leader, fol)
		if err == nil || !pgConnectionFailed(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %w", errLeaderUnavailable, err)
		}
		time.Sleep(followerReaderRetryDelay)
	}
}

func createFollowerReaderOnce(leader, fol *postgres.Instance) error {
	role, err := postgres.FollowerReaderRoleSQL(fol.ReaderUser, fol.ReaderPassword)
	if err != nil {
		return err
	}
	if _, err := runSQL(leader.MaintenanceURL(), role); err != nil {
		return fmt.Errorf("create follower login on %s: %w", leader.App, err)
	}
	dbs, err := leaderDatabases(leader)
	if err != nil {
		return fmt.Errorf("list databases on %s: %w", leader.App, err)
	}
	for _, db := range dbs {
		connect, err := postgres.GrantReaderDatabaseSQL(fol.ReaderUser, db)
		if err != nil {
			return err
		}
		if _, err := runSQL(leader.MaintenanceURL(), connect); err != nil {
			if pgDatabaseMissing(err, db) {
				continue
			}
			return fmt.Errorf("grant follower login on %s/%s: %w", leader.App, db, err)
		}
		grant, err := postgres.FollowerReaderGrantSQL(fol.ReaderUser, leader.AppUser)
		if err != nil {
			return err
		}
		if _, err := runSQL(instanceDatabaseURL(leader, db), grant); err != nil {
			if pgDatabaseMissing(err, db) {
				continue
			}
			return fmt.Errorf("grant follower reads on %s/%s: %w", leader.App, db, err)
		}
	}
	return nil
}

// errLeaderUnavailable marks a leader that stayed unreachable; writeAPIError
// answers it with a retryable 503.
var errLeaderUnavailable = errors.New("leader unreachable")

var (
	followerReaderRetryDelay   = time.Second
	followerReaderRetryTimeout = 60 * time.Second
)

// pgConnectionFailed is a psql run that never reached a working session:
// psql exits 2 when the connection fails, and a server that is starting or
// stopping refuses with one of these messages. "database does not exist"
// also exits 2 and is not a connection problem.
func pgConnectionFailed(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "does not exist") || strings.Contains(msg, "password authentication failed") {
		return false
	}
	for _, s := range []string{
		"exit status 2",
		"connection refused",
		"could not connect",
		"no route to host",
		"timeout expired",
		"the database system is starting up",
		"the database system is shutting down",
		"not yet accepting connections",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// pgDatabaseMissing is postgres saying the named database is gone (dropped
// with plain SQL, or between the listing and the grant).
func pgDatabaseMissing(err error, db string) bool {
	return err != nil && strings.Contains(err.Error(), fmt.Sprintf("database \"%s\" does not exist", db))
}

// leaderDatabases is every tenant database the leader has now. The store's
// list is only a fallback for a leader that answered the role statement but
// not the listing: a database dropped with plain SQL (pg:psql) stays in the
// store, and granting on it fails the follow.
func leaderDatabases(leader *postgres.Instance) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] || postgres.ValidDatabaseName(name) != nil {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	live, err := queryDatabasesOnInstance(leader)
	if err != nil {
		if pgConnectionFailed(err) {
			return nil, err
		}
		for _, db := range leader.Databases {
			add(db.Name)
		}
		return out, nil
	}
	for _, db := range live {
		add(db)
	}
	return out, nil
}

// dropFollowerReader removes a deleted follower's login from its leader. It
// is best effort and never blocks the delete: the leader may be down.
func (h *handler) dropFollowerReader(fol, leader *postgres.Instance) {
	if fol == nil || leader == nil || strings.TrimSpace(fol.ReaderUser) == "" {
		return
	}
	if h != nil && h.store != nil {
		// A replacement follower may still use this login.
		for _, other := range h.store.All() {
			if other != nil && other.ID != fol.ID && other.App != fol.App && strings.EqualFold(other.ReaderUser, fol.ReaderUser) {
				return
			}
		}
	}
	leaderCopy := *leader
	name := fol.ReaderUser
	go func() {
		_ = dropUserOnInstance(&leaderCopy, name, "")
	}()
}
