package main

import (
	"fmt"
	"strings"
)

const mainUsage = "usage: flynn-postgres-api               (start the plugin API)\n       flynn-postgres-api task <name> ..."

// unknownSubcommand refuses any argument the API server does not take. The
// server takes none (configuration is env), and an unknown word used to
// start it, so a console job on an older image hung (NEW-54).
func unknownSubcommand(args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("unknown subcommand %q\n%s", strings.TrimSpace(args[0]), mainUsage)
}
