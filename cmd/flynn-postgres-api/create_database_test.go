package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func TestCreateDatabaseOnInstanceRevokesPublicConnect(t *testing.T) {
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	var queries []string
	runSQL = func(connURL, query string) (string, error) {
		queries = append(queries, query)
		return "", nil
	}
	if err := createDatabaseOnInstance(inst, "analytics"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CREATE DATABASE analytics",
		"REVOKE CONNECT ON DATABASE analytics FROM PUBLIC",
		"GRANT CONNECT ON DATABASE analytics TO CURRENT_USER",
	}
	if strings.Join(queries, "\n") != strings.Join(want, "\n") {
		t.Fatalf("queries:\n%s\nwant:\n%s", strings.Join(queries, "\n"), strings.Join(want, "\n"))
	}
}

func TestCreateDatabaseOnInstanceDropsDatabaseWhenRevokeFails(t *testing.T) {
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	var queries []string
	runSQL = func(connURL, query string) (string, error) {
		queries = append(queries, query)
		if strings.HasPrefix(query, "REVOKE") {
			return "", fmt.Errorf("permission denied")
		}
		return "", nil
	}
	if err := createDatabaseOnInstance(inst, "analytics"); err == nil {
		t.Fatal("expected an error when PUBLIC CONNECT cannot be revoked")
	}
	if last := queries[len(queries)-1]; last != "DROP DATABASE IF EXISTS analytics" {
		t.Fatalf("a database left open to PUBLIC must be dropped; queries %q", queries)
	}
}

func TestRestoreDatabaseRevokesPublicConnect(t *testing.T) {
	for _, createErr := range []error{nil, fmt.Errorf(`psql: exit status 1: ERROR:  database "shop" already exists`)} {
		orig := runPG
		var ran [][]string
		runPG = func(_ context.Context, cmd postgres.PGCommand, _ io.Reader, _ io.Writer) error {
			ran = append(ran, cmd.Args)
			if len(ran) == 1 {
				return createErr
			}
			return nil
		}
		err := execDump{}.RestoreDatabase(context.Background(), "postgres://u:p@db.example:5432/appdb", "shop", strings.NewReader(""))
		runPG = orig
		if err != nil {
			t.Fatal(err)
		}
		var sawRevoke bool
		for _, args := range ran {
			joined := strings.Join(args, " ")
			if args[0] == "pg_restore" && !sawRevoke {
				t.Fatalf("pg_restore ran before PUBLIC CONNECT was revoked: %q", ran)
			}
			if args[0] == "psql" && strings.Contains(joined, "REVOKE CONNECT ON DATABASE shop FROM PUBLIC") &&
				strings.Contains(joined, "GRANT CONNECT ON DATABASE shop TO CURRENT_USER") {
				sawRevoke = true
			}
		}
		if !sawRevoke {
			t.Fatalf("restore (create error %v) never revoked PUBLIC CONNECT: %q", createErr, ran)
		}
	}
}
