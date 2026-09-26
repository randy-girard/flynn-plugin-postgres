package main

import (
	"fmt"
	"os"

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
