package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

func TestDefaultDatabaseName(t *testing.T) {
	re := regexp.MustCompile(`^[a-z][a-z0-9]{11}$`)
	got := DefaultDatabaseName()
	if !re.MatchString(got) {
		t.Fatalf("got %q", got)
	}
	if err := ValidDatabaseName(got); err != nil {
		t.Fatal(err)
	}
	other := DefaultDatabaseName()
	if other == got {
		t.Fatal("expected a new random name")
	}
	s := NewStore()
	inst, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inst.Databases) != 1 || !re.MatchString(inst.Databases[0].Name) {
		t.Fatalf("provision db %#v", inst.Databases)
	}
	if strings.HasPrefix(inst.Databases[0].Name, "db_") || strings.Contains(inst.Databases[0].Name, "postgresql") {
		t.Fatalf("must not derive the database name from the instance: %#v", inst.Databases)
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

func TestAttachmentKeysFirstUsesDatabaseURL(t *testing.T) {
	env := AttachmentKeys("", "DATABASE_URL", "postgresql-concave-48291", "postgres://db", nil, true)
	if env["DATABASE_URL"] != "postgres://db" {
		t.Fatalf("new provision must set DATABASE_URL: %#v", env)
	}
	if env["POSTGRESQL_CONCAVE_48291_DATABASE_URL"] != "" || env["FLYNN_POSTGRESQL_CONCAVE_48291_URL"] != "" {
		t.Fatalf("must not use instance name as env stem: %#v", env)
	}
	color := ""
	for k := range env {
		if postgresColorURLKey(k) {
			color = k
		}
	}
	if color == "" {
		t.Fatalf("provision must also set a color URL: %#v", env)
	}
	if len(env) != 2 {
		t.Fatalf("DATABASE_URL plus one color: %#v", env)
	}
	attach := AttachmentKeys("", "", "postgresql-concave-48291", "postgres://db", nil, false)
	if attach["DATABASE_URL"] != "" {
		t.Fatalf("attach of existing must not set DATABASE_URL: %#v", attach)
	}
	attachColor := ""
	for k := range attach {
		if postgresColorURLKey(k) {
			attachColor = k
		}
	}
	if attachColor == "" {
		t.Fatalf("attach must set a color URL: %#v", attach)
	}
	taken := AttachmentKeys("", "", "postgresql-concave-48291", "postgres://other", func(k string) bool {
		return k == "DATABASE_URL"
	}, true)
	if taken["DATABASE_URL"] != "" {
		t.Fatalf("second provision must not steal DATABASE_URL: %#v", taken)
	}
	other := ""
	for k := range taken {
		if postgresColorURLKey(k) {
			other = k
		}
	}
	if other == "" {
		t.Fatalf("second provision must pick a color: %#v", taken)
	}
}

func TestIsolatedInstanceApp(t *testing.T) {
	keep := []string{"postgresql-upland-88340", "postgresql-concave-48291", "pg-harbor-kxmnpq", "postgres-upland-88340"}
	for _, name := range keep {
		if !IsolatedInstanceApp(name) {
			t.Fatalf("%s should be an isolated instance", name)
		}
	}
	skip := []string{"", "postgres", "postgres-plugin", "postgres-api", "shop", "app-one", "controller", "pg-missing"}
	for _, name := range skip {
		if IsolatedInstanceApp(name) {
			t.Fatalf("%s must not be treated as an isolated instance", name)
		}
	}
}

// attachmentColors is duplicated in the engine plugins and in
// flynn/pkg/resname. The digest freezes the list so a copy cannot drift
// silently; when a color is added or removed, update the digest in all seven
// places at once.
const attachmentColorsDigest = "66cf0e3f123af773c2080403799064d0957d8df68ec028e915de613f87da63a4"

func TestAttachmentColorsFrozen(t *testing.T) {
	sum := sha256.Sum256([]byte(strings.Join(attachmentColors, ",")))
	if got := hex.EncodeToString(sum[:]); got != attachmentColorsDigest {
		t.Fatalf("attachment palette changed (%d colors): digest %s != %s; update this digest in flynn/pkg/resname and every engine plugin", len(attachmentColors), got, attachmentColorsDigest)
	}
}
