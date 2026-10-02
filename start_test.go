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
	if !strings.Contains(strings.ToLower(manifest.CLI.Doc), "canonical") || !strings.Contains(strings.ToLower(manifest.CLI.Doc), "fallback") {
		t.Fatal("cli.doc must say colon form is canonical and space form is a fallback")
	}
	usage, _, ok := strings.Cut(manifest.CLI.Doc, "\n\n")
	if !ok {
		t.Fatal("cli.doc must have a usage block followed by a blank line")
	}
	if !strings.Contains(usage, "flynn pg:psql") {
		t.Fatal("docopt usage must list colon form (canonical)")
	}
	if !strings.Contains(usage, "flynn pg psql") {
		t.Fatal("docopt usage must list space form as a fallback")
	}
	if i, j := strings.Index(usage, "flynn pg:psql"), strings.Index(usage, "flynn pg psql"); i > j {
		t.Fatal("colon form must be listed before the space fallback")
	}
	args, err := docopt.Parse(manifest.CLI.Doc, []string{"pg:psql", "--", "-c", "SELECT 1"}, true, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !args.Bool["pg:psql"] && !args.Bool["psql"] {
		t.Fatalf("colon argv did not select psql: %#v", args.Bool)
	}
	args, err = docopt.Parse(manifest.CLI.Doc, []string{"pg", "psql", "--", "-c", "SELECT 1"}, true, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !args.Bool["psql"] {
		t.Fatalf("psql not selected: %#v", args)
	}
	if strings.Contains(manifest.CLI.Doc, "psql [<name>]") {
		t.Fatal("a bare <name> before psql arguments captures redis-cli PING and other console args")
	}
	if !strings.Contains(manifest.CLI.Doc, "postgresql-<word>-<5 digits>") {
		t.Fatal("psql help should describe the resource app name")
	}
	var cli struct {
		CLI struct {
			ResourceEnv string `json:"resource_env"`
			Actions     []struct {
				Name string   `json:"name"`
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
	var dash struct {
		Dashboard struct {
			Routes []struct {
				Path  string `json:"path"`
				Title string `json:"title"`
			} `json:"routes"`
		} `json:"dashboard"`
	}
	if err := json.Unmarshal(b, &dash); err != nil {
		t.Fatal(err)
	}
	hasSettings := false
	for _, r := range dash.Dashboard.Routes {
		if r.Path == "/settings" && strings.EqualFold(r.Title, "Settings") {
			hasSettings = true
		}
	}
	if !hasSettings {
		t.Fatal("dashboard must declare a Settings route for resource delete")
	}
	var psqlArgs []string
	var hasUpgrade, hasCreate, hasDump, hasFollow, hasInfo bool
	for _, a := range cli.CLI.Actions {
		joined := strings.Join(a.Args, " ")
		if strings.Contains(joined, "DATABASE_URL") && a.Name == "psql" {
			psqlArgs = a.Args
		}
		if strings.Contains(joined, "task upgrade") {
			hasUpgrade = true
		}
		if a.Name == "create" && strings.Contains(joined, "task create-db") {
			hasCreate = true
		}
		if strings.Contains(joined, "pg_dump") {
			hasDump = true
		}
		if strings.Contains(joined, "task follow") {
			hasFollow = true
		}
		if a.Name == "info" && strings.Contains(joined, "task info") {
			hasInfo = true
		}
	}
	if len(psqlArgs) == 0 {
		t.Fatal("psql action missing DATABASE_URL")
	}
	if !hasUpgrade {
		t.Fatal("upgrade action must run flynn-postgres-api task upgrade")
	}
	if !hasCreate {
		t.Fatal("create action must run flynn-postgres-api task create-db")
	}
	if !hasDump {
		t.Fatal("dump action must run pg_dump")
	}
	if !hasFollow {
		t.Fatal("follow action must run flynn-postgres-api task follow")
	}
	if !hasInfo {
		t.Fatal("info action must run flynn-postgres-api task info")
	}
	if _, err := docopt.Parse(manifest.CLI.Doc, []string{"pg", "upgrade"}, true, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := docopt.Parse(manifest.CLI.Doc, []string{"pg", "create", "shop_analytics"}, true, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := docopt.Parse(manifest.CLI.Doc, []string{"pg", "dump", "-f", "postgres.dump"}, true, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := docopt.Parse(manifest.CLI.Doc, []string{"pg", "follow"}, true, "", false); err != nil {
		t.Fatal(err)
	}
	task, err := os.ReadFile("cmd/flynn-postgres-api/task.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(task), `/databases/`) || !strings.Contains(string(task), `/follow`) {
		t.Fatal("pg follow must POST /databases/<leader>/follow so the API can attach via the controller")
	}
	main, err := os.ReadFile("cmd/flynn-postgres-api/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "ProvisionResource") {
		t.Fatal("follow must attach the replica through controller ProvisionResource")
	}
	if strings.Contains(manifest.CLI.Doc, "[--replication") {
		t.Fatal("followers always stream; replication mode is not a follow flag")
	}
	commands := manifest.CLI.Doc
	if i := strings.Index(commands, "Commands:"); i >= 0 {
		commands = commands[i:]
	} else {
		t.Fatal("cli.doc must include a Commands: section")
	}
	if j := strings.Index(commands, "\nExamples:"); j >= 0 {
		commands = commands[:j]
	}
	for _, verb := range []string{"list", "help", "info", "create", "follow", "wait", "promote", "unfollow", "upgrade", "dump", "restore", "psql"} {
		if !strings.Contains(commands, "\t"+verb) {
			t.Fatalf("Commands: missing %s", verb)
		}
	}
	for _, phrase := range []string{
		"Show leader, followers, lag, and attached apps",
		"Create a logical database on this instance",
		"Create a streaming read-only follower",
		"Block until follower lag is zero",
		"Make a follower writable and rewrite the primary URL",
		"Stop replication and leave a standalone writable copy",
		"Follow, wait, promote, then recreate followers",
		"Dump this instance in custom format",
		"Restore a dump taken with pg dump",
		"Open psql against this instance",
	} {
		if !strings.Contains(commands, phrase) {
			t.Fatalf("Commands: missing description %q", phrase)
		}
	}
	examples := manifest.CLI.Doc
	if i := strings.Index(examples, "Examples:"); i >= 0 {
		examples = examples[i:]
	} else {
		t.Fatal("cli.doc must include Examples")
	}
	for _, ex := range []string{"pg:create shop_analytics", "pg:info", "pg:restore -f", "pg:unfollow", "pg:upgrade"} {
		if !strings.Contains(examples, ex) {
			t.Fatalf("Examples missing %q", ex)
		}
	}
	if !strings.Contains(manifest.CLI.Doc, "flynn-host pg:psql") || !strings.Contains(manifest.CLI.Doc, "flynn-host pg:dump") {
		t.Fatal("cli.doc must distinguish tenant pg from flynn-host pg")
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
	if !strings.Contains(src, "shared_preload_libraries = 'timescaledb'") {
		t.Fatal("timescaledb must be preloaded or CREATE EXTENSION fails")
	}
	if !strings.Contains(src, "pg_basebackup") || !strings.Contains(src, "POSTGRES_PRIMARY_URL") {
		t.Fatal("followers must pg_basebackup from POSTGRES_PRIMARY_URL")
	}
	if !strings.Contains(src, "host replication") || !strings.Contains(src, "REPLICATION;") {
		t.Fatal("primaries must allow streaming replication")
	}
	hook, err := os.ReadFile("script/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hook), "http://") || strings.Contains(string(hook), "curl") {
		t.Fatal("host hook cannot HTTP the plugin API; flynn-host and the API start upgrades")
	}
	mainSrc, err := os.ReadFile("cmd/flynn-postgres-api/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mainSrc), "autoStartClusterUpgrades") {
		t.Fatal("plugin API must start cluster upgrades on boot")
	}
	upgSrc, err := os.ReadFile("cmd/flynn-postgres-api/upgrade.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(upgSrc), "ArtifactIDs[0]") || strings.Contains(string(upgSrc), "rel.ArtifactIDs") {
		t.Fatal("boot cluster upgrades must not compare release image ids; plugin:update --rebuild would logical-upgrade every instance")
	}
	pkgs, err := os.ReadFile("img/packages.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"postgresql-16-postgis-3", "postgresql-16-pgrouting", "timescaledb-2-postgresql-16"} {
		if !strings.Contains(string(pkgs), name) {
			t.Fatalf("packages.sh missing %s", name)
		}
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
	if !strings.Contains(src, `"flynn-datastore": "true"`) {
		t.Fatal("isolated instances must advertise flynn-datastore so user jobs can resolve leader.<name>.discoverd")
	}
}

func TestStartInstanceDoesNotWaitOnScaleStallProbes(t *testing.T) {
	b, err := os.ReadFile("cmd/flynn-postgres-api/live.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, "NoWait:") || !strings.Contains(src, "NoWait") {
		t.Fatal("resource:add must scale with NoWait; a 5m wait enables 30s stall probes during initdb")
	}
	if !strings.Contains(src, "true") {
		t.Fatal("NoWait must be true")
	}
	if !strings.Contains(src, "GetInstances(") && !strings.Contains(src, "waitInstanceReady") {
		t.Fatal("provision must wait for discoverd, not job-up")
	}
}

func TestCIPushCoverageBadgeRetriesRejectedPush(t *testing.T) {
	b, err := os.ReadFile("script/ci-push-coverage-badge")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, need := range []string{
		"COVERAGE_BADGE_REMOTE",
		"git fetch",
		"git reset --hard FETCH_HEAD",
		"failed to push coverage badge after retries",
	} {
		if !strings.Contains(src, need) {
			t.Fatalf("ci-push-coverage-badge must %q so a concurrent main push does not fail CI", need)
		}
	}
}
