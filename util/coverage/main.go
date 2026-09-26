package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "coverage-html: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	profile := flag.String("profile", "", "go coverprofile")
	out := flag.String("out", "coverage", "output directory")
	root := flag.String("root", ".", "repository root for source files")
	flag.Parse()
	if *profile == "" {
		return fmt.Errorf("-profile is required")
	}
	files, err := parseProfile(*profile, *root)
	if err != nil {
		return err
	}
	r := buildReport(files)
	if err := writeHTML(r, *out); err != nil {
		return err
	}
	fmt.Printf("==> Coverage HTML: %s/index.html (%d files, %.1f%%)\n", *out, len(r.Files), r.Percent())
	return nil
}
