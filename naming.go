package postgres

import (
	"crypto/rand"
	"strings"

	"github.com/randy-girard/flynn/pkg/resname"
)

const postgresAttachPrefix = "FLYNN_POSTGRESQL_"

// attachmentColors are FLYNN_POSTGRESQL_<COLOR>_URL stems. Keep in sync with
// flynn/pkg/resname and the dashboard mergeAttachment helper.
var attachmentColors = []string{
	"AMBER", "AQUA", "AZURE", "BEIGE", "BLACK", "BLUE", "BRASS", "BRONZE",
	"BROWN", "BURGUNDY", "COBALT", "COPPER", "CORAL", "CREAM", "CRIMSON",
	"CYAN", "EMERALD", "FUCHSIA", "GOLD", "GRAY", "GREEN", "INDIGO", "IVORY",
	"JADE", "LAVENDER", "LIME", "MAGENTA", "MAROON", "MINT", "NAVY", "OLIVE",
	"ORANGE", "PEACH", "PEARL", "PINK", "PLATINUM", "PLUM", "PURPLE", "RED",
	"ROSE", "RUBY", "RUST", "SAGE", "SALMON", "SAND", "SCARLET", "SILVER",
	"SLATE", "TAN", "TEAL", "TOMATO", "TURQUOISE", "VIOLET", "WHITE", "WINE",
	"YELLOW",
}

const digitAlphabet = "0123456789"

// postgresWords are FLYNN_POSTGRES instance stems. Keep in sync with
// flynn/pkg/resname words.
var postgresWords = []string{
	"amber", "basin", "cedar", "concave", "delta", "ember", "fjord", "grove", "harbor",
	"inlet", "juniper", "kelp", "lagoon", "meadow", "north", "orchid", "prairie",
	"quartz", "ridge", "spruce", "timber", "upland", "valley", "willow", "yarrow",
}

// IsolatedInstanceApp is a tenant Postgres Flynn app created for one resource
// (postgresql-concave-48291, older pg-harbor-kxmnpq, or postgres-<word>-<digits>).
// The plugin API, platform appliance, and postgres-plugin app are not instances.
func IsolatedInstanceApp(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "", "postgres", "postgres-plugin", "postgres-api":
		return false
	}
	if strings.HasPrefix(name, "postgresql-") || strings.HasPrefix(name, "pg-") {
		return resname.IsolatedService(name)
	}
	const head = "postgres-"
	if !strings.HasPrefix(name, head) {
		return false
	}
	rest := name[len(head):]
	if rest == "plugin" || rest == "api" || strings.HasPrefix(rest, "api-") {
		return false
	}
	return resname.IsolatedService("postgresql-"+rest) || resname.IsolatedService("pg-"+rest)
}

// UniquePostgresApp returns postgresql-<word>-<5 digits>, for example
// postgresql-concave-48291. taken reports names already used.
func UniquePostgresApp(taken func(string) bool) string {
	const head = "postgresql-"
	if len(postgresWords) == 0 {
		return head + "app-" + nameDigits(5)
	}
	for i := 0; i < 32; i++ {
		name := head + postgresWords[nameIndex(len(postgresWords))] + "-" + nameDigits(5)
		if taken == nil || !taken(name) {
			return name
		}
	}
	return head + postgresWords[nameIndex(len(postgresWords))] + "-" + nameDigits(8)
}

// DefaultDatabaseName is the first application database on a new instance.
// It is a random alphanumeric name (letter first) so it is not the instance
// app name and is not `postgres`.
func DefaultDatabaseName() string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	const rest = "abcdefghijklmnopqrstuvwxyz0123456789"
	const n = 12
	buf := make([]byte, n)
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	buf[0] = letters[int(raw[0])%len(letters)]
	for i := 1; i < n; i++ {
		buf[i] = rest[int(raw[i])%len(rest)]
	}
	return string(buf)
}

func (s *Store) uniqueApp() string {
	return UniquePostgresApp(func(name string) bool {
		for _, inst := range s.byID {
			if inst != nil && inst.App == name {
				return true
			}
		}
		return s.NameTaken != nil && s.NameTaken(name)
	})
}

func nameDigits(n int) string {
	buf := make([]byte, n)
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	for i := range buf {
		buf[i] = digitAlphabet[int(raw[i])%len(digitAlphabet)]
	}
	return string(buf)
}

// AttachmentKeys is every *_URL injected for one postgres resource. Every
// provision and attach sets FLYNN_POSTGRESQL_<COLOR>_URL unless --as names
// the attachment. A new provision also sets DATABASE_URL when that key is
// free. Attaching an existing resource never sets DATABASE_URL.
func AttachmentKeys(as, _, resourceApp, rawURL string, taken func(string) bool, newProvision bool) map[string]string {
	out := map[string]string{}
	busy := func(k string) bool {
		if _, ok := out[k]; ok {
			return true
		}
		return taken != nil && taken(k)
	}
	if key := postgresAppURLKey(as, busy); key != "" {
		out[key] = rawURL
	}
	if newProvision && rawURL != "" && !busy("DATABASE_URL") {
		out["DATABASE_URL"] = rawURL
	}
	_ = resourceApp
	return out
}

func postgresAppURLKey(as string, taken func(string) bool) string {
	as = strings.ToUpper(strings.TrimSpace(as))
	as = strings.TrimSuffix(as, "_URL")
	as = strings.Trim(as, "_")
	if as != "" {
		return postgresAttachmentURLKey(as, taken)
	}
	return colorDatabaseURL(taken)
}

func postgresAttachmentURLKey(as string, taken func(string) bool) string {
	as = strings.ToUpper(strings.TrimSpace(as))
	as = strings.TrimSuffix(as, "_URL")
	as = strings.Trim(as, "_")
	if as == "" {
		return colorDatabaseURL(taken)
	}
	if strings.HasPrefix(as, postgresAttachPrefix) {
		return as + "_URL"
	}
	if isAttachmentColor(as) {
		return postgresAttachPrefix + as + "_URL"
	}
	return as + "_URL"
}

func colorDatabaseURL(taken func(string) bool) string {
	if len(attachmentColors) == 0 {
		return postgresAttachPrefix + "AMBER_URL"
	}
	start := nameIndex(len(attachmentColors))
	for i := 0; i < len(attachmentColors); i++ {
		color := attachmentColors[(start+i)%len(attachmentColors)]
		key := postgresAttachPrefix + color + "_URL"
		if taken == nil || !taken(key) {
			return key
		}
	}
	return postgresAttachPrefix + attachmentColors[start] + "_X_URL"
}

func postgresColorURLKey(k string) bool {
	k = strings.TrimSpace(k)
	if !strings.HasPrefix(k, postgresAttachPrefix) || !strings.HasSuffix(k, "_URL") {
		return false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(k, postgresAttachPrefix), "_URL")
	if mid == "" {
		return false
	}
	for _, c := range mid {
		if c >= 'A' && c <= 'Z' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func isAttachmentColor(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	for _, c := range attachmentColors {
		if c == name {
			return true
		}
	}
	return false
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
