package postgres

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/flynn/go-docopt"
)

func TestPluginDocParsesPsql(t *testing.T) {
	b, err := os.ReadFile("flynn-plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		CLI struct {
			Doc string `json:"doc"`
		} `json:"cli"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(manifest.CLI.Doc, "pg:psql") {
		t.Fatal("docopt argv is \"pg psql\" after flynn pg:psql is expanded; colon usage does not parse")
	}
	args, err := docopt.Parse(manifest.CLI.Doc, []string{"pg", "psql", "--", "-c", "SELECT 1"}, true, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !args.Bool["psql"] {
		t.Fatalf("psql not selected: %#v", args)
	}
	named, err := docopt.Parse(manifest.CLI.Doc, []string{"pg", "psql", "harbor-kxmnpq"}, true, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if named.String["<name>"] != "harbor-kxmnpq" {
		t.Fatalf("name: %#v", named.String)
	}
	var cli struct {
		CLI struct {
			ResourceEnv string `json:"resource_env"`
			Actions     []struct {
				Args []string `json:"args"`
			} `json:"actions"`
		} `json:"cli"`
	}
	if err := json.Unmarshal(b, &cli); err != nil {
		t.Fatal(err)
	}
	if cli.CLI.ResourceEnv != "FLYNN_POSTGRES" {
		t.Fatalf("resource_env=%q, want the instance app name", cli.CLI.ResourceEnv)
	}
	if len(cli.CLI.Actions) == 0 || !strings.Contains(strings.Join(cli.CLI.Actions[0].Args, " "), "POSTGRES_URL") {
		t.Fatalf("psql args %#v", cli.CLI.Actions)
	}
}

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
	if !strings.Contains(src, "ssl = on") || !strings.Contains(src, "ssl_cert_file") {
		t.Fatal("connection strings use sslmode=require, so the server must speak TLS")
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
