package postgres

import (
	"crypto/rand"
	"strings"
)

// words are the readable middle of a resource app name. The six-letter
// suffix is what keeps two resources from sharing a name.
var nameWords = []string{
	"amber", "basin", "cedar", "delta", "ember", "fjord", "grove", "harbor",
	"inlet", "juniper", "kelp", "lagoon", "meadow", "north", "orchid", "prairie",
	"quartz", "ridge", "spruce", "timber", "upland", "valley", "willow", "yarrow",
}

const nameAlphabet = "abcdefghijklmnopqrstuvwxyz"

// UniqueApp returns <prefix>-<word>-<6 letters>. taken reports names already used.
func UniqueApp(prefix string, taken func(string) bool) string {
	prefix = strings.ToLower(strings.Trim(strings.TrimSpace(prefix), "-"))
	if prefix == "" {
		prefix = "db"
	}
	for i := 0; i < 32; i++ {
		name := prefix + "-" + nameWords[nameIndex(len(nameWords))] + "-" + nameLetters(6)
		if taken == nil || !taken(name) {
			return name
		}
	}
	return prefix + "-" + nameWords[nameIndex(len(nameWords))] + "-" + nameLetters(8)
}

// DefaultDatabaseName is the first application database on a new instance.
// pg-harbor-kxmnpq becomes db_pg_harbor_kxmnpq so the name is unique and
// longer than db_ plus eight hex characters.
func DefaultDatabaseName(app string) string {
	app = strings.ToLower(strings.TrimSpace(app))
	app = strings.ReplaceAll(app, "-", "_")
	if app == "" {
		return "db_app"
	}
	name := "db_" + app
	if len(name) > 63 {
		return name[:63]
	}
	return name
}

func (s *Store) uniqueApp(prefix string) string {
	return UniqueApp(prefix, func(name string) bool {
		for _, inst := range s.byID {
			if inst != nil && inst.App == name {
				return true
			}
		}
		return s.NameTaken != nil && s.NameTaken(name)
	})
}

func nameLetters(n int) string {
	buf := make([]byte, n)
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	for i := range buf {
		buf[i] = nameAlphabet[int(raw[i])%len(nameAlphabet)]
	}
	return string(buf)
}

// AttachmentKeys is every *_URL injected for one resource. The resource name
// is always PREFIX_WORD_SUFFIX_DATABASE_URL. --as NAME also sets NAME_URL. The
// engine's usual variable is set only when the app does not already have it.
// Every returned key is locked against env:set.
func AttachmentKeys(as, conventional, resourceApp, rawURL string, taken func(string) bool) map[string]string {
	out := map[string]string{}
	busy := func(k string) bool {
		if _, ok := out[k]; ok {
			return true
		}
		return taken != nil && taken(k)
	}
	prefix, word, suffix, ok := splitResourceApp(resourceApp)
	if ok {
		key := strings.ToUpper(prefix+"_"+word+"_"+suffix) + "_DATABASE_URL"
		if busy(key) {
			key = strings.ToUpper(prefix+"_"+word+"_"+suffix) + "_X_DATABASE_URL"
		}
		out[key] = rawURL
	}
	if as = strings.ToUpper(strings.TrimSpace(as)); as != "" {
		key := as + "_URL"
		if !busy(key) {
			out[key] = rawURL
		}
	}
	if conventional != "" && !busy(conventional) {
		out[conventional] = rawURL
	}
	if len(out) == 0 && conventional != "" {
		out[conventional] = rawURL
	}
	return out
}

func splitResourceApp(resourceApp string) (prefix, word, suffix string, ok bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(resourceApp)), "-")
	if len(parts) < 3 {
		return "", "", "", false
	}
	prefix, word, suffix = parts[0], parts[1], parts[len(parts)-1]
	if prefix == "" || word == "" || len(suffix) < 6 {
		return "", "", "", false
	}
	return prefix, word, suffix, true
}

func nameIndex(n int) int {
	if n <= 1 {
		return 0
	}
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return int(b[0]) % n
}
