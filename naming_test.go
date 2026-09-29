package postgres

import (
	"regexp"
	"testing"
)

func TestDefaultDatabaseName(t *testing.T) {
	got := DefaultDatabaseName("pg-harbor-kxmnpq")
	if got != "db_pg_harbor_kxmnpq" {
		t.Fatalf("got %q", got)
	}
	if len(got) <= len("db_a803c7ba") {
		t.Fatalf("name should be longer than db_ plus 8 hex chars: %q", got)
	}
	if DefaultDatabaseName("") != "db_app" {
		t.Fatal("empty app")
	}
	s := NewStore()
	inst, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inst.Databases) != 1 || inst.Databases[0].Name != DefaultDatabaseName(inst.App) {
		t.Fatalf("provision db %#v app %q", inst.Databases, inst.App)
	}
}

func TestUniqueAppName(t *testing.T) {
	re := regexp.MustCompile(`^pg-[a-z]+-[a-z]{6}$`)
	s := NewStore()
	s.NameTaken = func(name string) bool { return name == "pg-harbor-aaaaaa" }
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		name := s.uniqueApp("pg")
		if !re.MatchString(name) {
			t.Fatalf("name %q", name)
		}
		if seen[name] {
			t.Fatalf("duplicate %q", name)
		}
		seen[name] = true
		s.byID[name] = &Instance{App: name}
	}
}
