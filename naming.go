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

// AttachmentURLKey is the env var for a resource on an app.
// The first one uses conventional (DATABASE_URL, REDIS_URL, ...).
// Another one is PREFIX_WORD_DATABASE_URL from the resource app name.
func AttachmentURLKey(conventional, resourceApp string, taken func(string) bool) string {
	if taken == nil || !taken(conventional) {
		return conventional
	}
	prefix, word, suffix, ok := splitResourceApp(resourceApp)
	if !ok {
		return conventional
	}
	key := strings.ToUpper(prefix+"_"+word) + "_DATABASE_URL"
	if taken(key) {
		key = strings.ToUpper(prefix+"_"+word+"_"+suffix) + "_DATABASE_URL"
	}
	return key
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
