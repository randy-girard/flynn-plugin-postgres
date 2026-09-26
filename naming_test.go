package postgres

import (
	"regexp"
	"testing"
)

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
