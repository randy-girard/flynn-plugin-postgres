package postgres

import (
	"fmt"
	"strings"
)

// ValidDatabaseName is a CREATE DATABASE name: letters, digits, underscore,
// not a reserved catalog name. This is a logical database on one instance,
// not a new Flynn resource.
func ValidDatabaseName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("database name is required")
	}
	if !isolationIdentOK(name) {
		return fmt.Errorf("database name must be letters, digits, and underscores")
	}
	if name[0] >= '0' && name[0] <= '9' {
		return fmt.Errorf("database name cannot start with a digit")
	}
	switch strings.ToLower(name) {
	case "postgres", "template0", "template1":
		return fmt.Errorf("%s is a reserved database name", name)
	}
	return nil
}

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
//
// A database made after bootstrap starts with the default ACL, which lets
// PUBLIC connect. Every start therefore sweeps pg_database: each other
// database that accepts connections loses PUBLIC CONNECT, and its owner and
// the instance login keep CONNECT so backups still reach it.
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
			"GRANT CONNECT ON DATABASE %s TO %s;\n"+
			"DO $flynn_isolation$\n"+
			"DECLARE d record;\n"+
			"BEGIN\n"+
			"  FOR d IN SELECT datname, pg_get_userbyid(datdba) AS owner FROM pg_database\n"+
			"      WHERE datallowconn AND datname NOT IN ('postgres', 'template1') LOOP\n"+
			"    EXECUTE format('REVOKE CONNECT ON DATABASE %%I FROM PUBLIC', d.datname);\n"+
			"    EXECUTE format('GRANT CONNECT ON DATABASE %%I TO %%I', d.datname, d.owner);\n"+
			"    EXECUTE format('GRANT CONNECT ON DATABASE %%I TO %s', d.datname);\n"+
			"  END LOOP;\n"+
			"END\n"+
			"$flynn_isolation$;\n",
		db, db, user, user,
	), nil
}

// CreateDatabaseSQL is CREATE DATABASE for a validated logical database name,
// followed by the statements that close it to PUBLIC. A new database gets the
// default ACL, which lets every role on the instance connect. Run each
// statement on its own: CREATE DATABASE cannot share a multi-statement string.
// The creating login owns the database and keeps CONNECT.
func CreateDatabaseSQL(name string) ([]string, error) {
	if err := ValidDatabaseName(name); err != nil {
		return nil, err
	}
	return []string{
		"CREATE DATABASE " + name,
		"REVOKE CONNECT ON DATABASE " + name + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + name + " TO CURRENT_USER",
	}, nil
}

// ValidUserName is a CREATE ROLE name: letters, digits, underscore.
func ValidUserName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("user name is required")
	}
	if !isolationIdentOK(name) {
		return fmt.Errorf("user name must be letters, digits, and underscores")
	}
	if name[0] >= '0' && name[0] <= '9' {
		return fmt.Errorf("user name cannot start with a digit")
	}
	lower := strings.ToLower(name)
	if lower == "postgres" || strings.HasPrefix(lower, "pg_") {
		return fmt.Errorf("%s is a reserved role name", name)
	}
	return nil
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// CreateUserSQL is CREATE ROLE LOGIN for a validated application user.
func CreateUserSQL(name, password string) (string, error) {
	if err := ValidUserName(name); err != nil {
		return "", err
	}
	if strings.TrimSpace(password) == "" {
		return "", fmt.Errorf("password is required")
	}
	if strings.ContainsRune(password, 0) {
		return "", fmt.Errorf("invalid password")
	}
	return fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD %s CONNECTION LIMIT 20 NOSUPERUSER NOCREATEDB NOCREATEROLE",
		name, quoteLiteral(password),
	), nil
}

// GrantUserDatabaseSQL lets the role connect to one logical database.
func GrantUserDatabaseSQL(name, database string) (string, error) {
	if err := ValidUserName(name); err != nil {
		return "", err
	}
	if err := ValidDatabaseName(database); err != nil {
		return "", err
	}
	return fmt.Sprintf("GRANT CONNECT, TEMPORARY ON DATABASE %s TO %s", database, name), nil
}

// GrantUserSchemaSQL is run inside the target database after CREATE ROLE.
func GrantUserSchemaSQL(name string) (string, error) {
	if err := ValidUserName(name); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"GRANT USAGE, CREATE ON SCHEMA public TO %s;\n"+
			"GRANT ALL ON ALL TABLES IN SCHEMA public TO %s;\n"+
			"GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO %s;\n"+
			"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO %s;\n"+
			"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO %s",
		name, name, name, name, name,
	), nil
}

// DropUserSQL removes a login role created from the dashboard.
func DropUserSQL(name string) (string, error) {
	if err := ValidUserName(name); err != nil {
		return "", err
	}
	return "DROP ROLE IF EXISTS " + name, nil
}

// DropOwnedSQL drops objects and privileges the role holds in the current database.
func DropOwnedSQL(name string) (string, error) {
	if err := ValidUserName(name); err != nil {
		return "", err
	}
	return "DROP OWNED BY " + name + " CASCADE", nil
}

// RevokeUserDatabaseSQL removes CONNECT/TEMPORARY granted at CREATE user time.
func RevokeUserDatabaseSQL(name, database string) (string, error) {
	if err := ValidUserName(name); err != nil {
		return "", err
	}
	if err := ValidDatabaseName(database); err != nil {
		return "", err
	}
	return fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM %s", database, name), nil
}

// CleanupUserSQL revokes grants the instance login made to name, then drops
// objects that role owns. DROP OWNED BY another role requires membership, so
// this grants the role to CURRENT_USER first.
func CleanupUserSQL(name string) (string, error) {
	if err := ValidUserName(name); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM %s;\n"+
			"ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM %s;\n"+
			"REVOKE ALL ON SCHEMA public FROM %s;\n"+
			"REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %s;\n"+
			"REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %s;\n"+
			"GRANT %s TO CURRENT_USER;\n"+
			"DROP OWNED BY %s CASCADE",
		name, name, name, name, name, name, name), nil
}

// AdminPrivilegesSQL lets the instance login CREATE DATABASE and CREATE ROLE
// so the plugin API can manage logical databases and users over TCP. It also
// joins pg_read_all_data so the backup's pg_dump reads tables owned by other
// roles instead of failing on permission denied.
func AdminPrivilegesSQL(user string) (string, error) {
	user = strings.TrimSpace(user)
	if !isolationIdentOK(user) {
		return "", fmt.Errorf("postgres isolation identifiers must be alphanumeric")
	}
	return fmt.Sprintf("ALTER ROLE %s CREATEDB CREATEROLE; GRANT pg_read_all_data TO %s;", user, user), nil
}
