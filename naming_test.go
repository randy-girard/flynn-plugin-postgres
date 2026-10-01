package postgres

import (
	"regexp"
	"strings"
	"testing"
)

func TestDefaultDatabaseName(t *testing.T) {
	got := DefaultDatabaseName("postgresql-concave-48291")
	if got != "db_postgresql_concave_48291" {
		t.Fatalf("got %q", got)
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
	re := regexp.MustCompile(`^postgresql-[a-z]+-[0-9]{5}$`)
	s := NewStore()
	s.NameTaken = func(name string) bool { return strings.HasSuffix(name, "-00000") }
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		name := s.uniqueApp()
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

func TestAttachmentKeysUsesColorNotResourceName(t *testing.T) {
	env := AttachmentKeys("", "DATABASE_URL", "postgresql-concave-48291", "postgres://db", nil)
	if env["DATABASE_URL"] != "" {
		t.Fatalf("must not set DATABASE_URL: %#v", env)
	}
	if env["POSTGRESQL_CONCAVE_48291_DATABASE_URL"] != "" || env["FLYNN_POSTGRESQL_CONCAVE_48291_URL"] != "" {
		t.Fatalf("must not use instance name as env stem: %#v", env)
	}
	color := ""
	for k, v := range env {
		if postgresColorURLKey(k) && v == "postgres://db" {
			color = k
		}
	}
	if color == "" {
		t.Fatalf("missing color URL: %#v", env)
	}
	taken := AttachmentKeys("", "", "postgresql-concave-48291", "postgres://other", func(k string) bool {
		return k == color
	})
	other := ""
	for k := range taken {
		if postgresColorURLKey(k) {
			other = k
		}
	}
	if other == "" || other == color {
		t.Fatalf("second attach must pick another color: %#v vs %s", taken, color)
	}
}
