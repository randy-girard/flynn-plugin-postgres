package main

import (
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func TestDropUserSQLUsesTenantDatabase(t *testing.T) {
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	var urls []string
	runSQL = func(connURL, query string) (string, error) {
		urls = append(urls, connURL+"\x00"+query)
		return "", nil
	}
	if err := dropUserOnInstance(inst, "alice", "appdb"); err != nil {
		t.Fatal(err)
	}
	if len(urls) == 0 {
		t.Fatal("expected psql")
	}
	sawDropRole := false
	for _, row := range urls {
		connURL, query, _ := strings.Cut(row, "\x00")
		if strings.Contains(connURL, "/postgres?") || strings.Contains(connURL, "/postgres/") {
			t.Fatalf("admin SQL still used catalog database postgres: %s", connURL)
		}
		if strings.Contains(query, "DROP ROLE") {
			sawDropRole = true
			if !strings.Contains(connURL, inst.Databases[0].Name) {
				t.Fatalf("DROP ROLE URL %s missing tenant database", connURL)
			}
		}
		if strings.Contains(query, "REVOKE ALL ON DATABASE") && !strings.Contains(query, "FROM alice") {
			t.Fatalf("revoke: %s", query)
		}
	}
	if !sawDropRole {
		t.Fatal("missing DROP ROLE")
	}
}

func TestListUsersOnInstanceDoesNotCheckOtherRolePrivileges(t *testing.T) {
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	var query string
	runSQL = func(connURL, q string) (string, error) {
		query = q
		return inst.AppUser + "\nalice\n", nil
	}
	users := listUsersOnInstance(inst)
	if strings.Contains(query, "has_database_privilege") {
		t.Fatalf("list users must not check other roles' privileges: %s", query)
	}
	if len(users) != 2 || users[1].Name != "alice" {
		t.Fatalf("users %+v", users)
	}
}
