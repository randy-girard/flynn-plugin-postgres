package postgres

import (
	"os"
	"strings"
	"testing"
)

func TestStartScriptDoesNotExecAShellFunction(t *testing.T) {
	b, err := os.ReadFile("start.sh")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if strings.Contains(src, "exec as_postgres") {
		t.Fatal("exec cannot invoke a shell function; the job exits 127 after initdb")
	}
	if !strings.Contains(src, "exec /bin/flynn-postgres serve") {
		t.Fatal("the postgres process must stay up under the discoverd supervisor")
	}
}

func TestServeRegistersAfterPostgresListens(t *testing.T) {
	b, err := os.ReadFile("cmd/flynn-postgres/serve.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	listen := strings.Index(src, "waitLocalPort(")
	reg := strings.Index(src, "RegisterInstance(")
	if listen < 0 || reg < 0 || reg < listen {
		t.Fatal("discoverd registration must follow a listening postgres")
	}
}
