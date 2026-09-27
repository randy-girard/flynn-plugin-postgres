package postgres

import (
	"fmt"
	"strings"
)

func isolationIdentOK(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// ConnectIsolationSQL revokes PUBLIC CONNECT on the instance, then grants the
// app role CONNECT only on its database. postgres and template1 stay reachable
// for the OS superuser (CONNECT is not checked for superusers).
func ConnectIsolationSQL(user, db string) (string, error) {
	user = strings.TrimSpace(user)
	db = strings.TrimSpace(db)
	if !isolationIdentOK(user) || !isolationIdentOK(db) {
		return "", fmt.Errorf("postgres isolation identifiers must be alphanumeric")
	}
	return fmt.Sprintf(
		"REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;\n"+
			"REVOKE CONNECT ON DATABASE template1 FROM PUBLIC;\n"+
			"REVOKE CONNECT ON DATABASE %s FROM PUBLIC;\n"+
			"GRANT CONNECT ON DATABASE %s TO %s;\n",
		db, db, user,
	), nil
}
