package main

import (
	"fmt"
	"os"
	"os/user"
	"strconv"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: flynn-postgres info|follow|wait|promote|unfollow|psql")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := servePostgres(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "install-tls":
		dir := "/data"
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		installed, err := postgres.InstallTLSFiles(dir, os.Getenv, chownPostgres)
		if err != nil {
			fmt.Fprintln(os.Stderr, "install-tls:", err)
			os.Exit(1)
		}
		if installed {
			fmt.Println("installed the release TLS certificate")
		}
	case "psql":
		args, err := postgres.PsqlCommand(os.Getenv("DATABASE_URL"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(stringsJoin(args))
	default:
		fmt.Printf("pg:%s is served by the postgres plugin API at %s\n", os.Args[1], postgres.ProviderURL())
	}
}

// chownPostgres gives path to the postgres OS user (the server refuses a key
// it does not own).
func chownPostgres(path string) error {
	u, err := user.Lookup("postgres")
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

func stringsJoin(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
