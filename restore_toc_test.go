package postgres

import (
	"strings"
	"testing"
)

// pg_restore -l of a dump of an instance database: start.sh creates these
// extensions as the OS postgres role, so the instance login cannot drop or
// comment on them.
const instanceDumpTOC = `;
; Archive created at 2026-11-19 23:00:00 UTC
;     dbname: appdb
;
4; 3079 16434 EXTENSION - pg_stat_statements 
3479; 0 0 COMMENT - EXTENSION pg_stat_statements 
3; 3079 16397 EXTENSION - pgcrypto 
3480; 0 0 COMMENT - EXTENSION pgcrypto 
2; 3079 16386 EXTENSION - uuid-ossp 
3481; 0 0 COMMENT - EXTENSION "uuid-ossp" 
220; 1259 16465 TABLE public r19 app
221; 1259 16470 TABLE public "EXTENSION - x" app
3472; 0 16465 TABLE DATA public r19 app
`

func TestRestoreTOCListSkipsExtensions(t *testing.T) {
	list, exts := RestoreTOCList(instanceDumpTOC)
	for _, line := range strings.Split(list, "\n") {
		if strings.Contains(line, "3079 ") || strings.Contains(line, "COMMENT - EXTENSION") {
			t.Errorf("extension entry kept: %q", line)
		}
	}
	for _, want := range []string{"220; 1259 16465 TABLE public r19 app", `221; 1259 16470 TABLE public "EXTENSION - x" app`, "3472; 0 16465 TABLE DATA public r19 app"} {
		if !strings.Contains(list, want+"\n") {
			t.Errorf("list lost %q:\n%s", want, list)
		}
	}
	if strings.Join(exts, ",") != "pg_stat_statements,pgcrypto,uuid-ossp" {
		t.Errorf("extensions %q", exts)
	}
}

func TestCreateExtensionSQLQuotesName(t *testing.T) {
	sql, err := CreateExtensionSQL("uuid-ossp")
	if err != nil || sql != `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"` {
		t.Fatalf("%q %v", sql, err)
	}
	for _, bad := range []string{"", `x"; DROP TABLE t; --`, "a b"} {
		if _, err := CreateExtensionSQL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPgRestoreListCommandNeedsNoConnection(t *testing.T) {
	cmd := PgRestoreListCommand("/tmp/x/dump")
	if strings.Join(cmd.Args, " ") != "pg_restore -l /tmp/x/dump" || cmd.Password != "" {
		t.Fatalf("%#v", cmd)
	}
}
