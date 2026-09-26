package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBlockLineAndAreas(t *testing.T) {
	line := modulePath + "process.go:1.1,10.2 2 1"
	name, b, err := parseBlockLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if name != modulePath+"process.go" || b.StartLine != 1 || b.EndLine != 10 || b.NumStmt != 2 || b.Count != 1 {
		t.Fatalf("%s %+v", name, b)
	}
	area, pkg := splitAreaPackage("process.go")
	if area != "root" || pkg != "." {
		t.Fatalf("area=%s pkg=%s", area, pkg)
	}
	area, pkg = splitAreaPackage("cmd/plugin-build/main.go")
	if area != "cmd" || pkg != "cmd/plugin-build" {
		t.Fatalf("area=%s pkg=%s", area, pkg)
	}
}

func TestWriteHTMLPerFileAndIndexSections(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	profile := filepath.Join(dir, "coverage.out")
	data := "mode: atomic\n" +
		modulePath + "process.go:1.1,20.2 8 1\n" +
		modulePath + "cmd/plugin-build/main.go:1.1,10.2 2 0\n"
	if err := os.WriteFile(profile, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "html")
	files, err := parseProfile(profile, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%d", len(files))
	}
	r := buildReport(files)
	if err := writeHTML(r, out); err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	for _, want := range []string{
		"Overall by package area",
		`id="cmd"`,
		"process.go",
		"files/process.go.html",
		"files/cmd/plugin-build/main.go.html",
		reportTitle,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index missing %q", want)
		}
	}
	page := filepath.Join(out, "files", filepath.FromSlash("process.go"+".html"))
	body, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	if !strings.Contains(src, "package postgres") {
		t.Fatalf("file page missing source: %s", page)
	}
	if !strings.Contains(src, `class="cov"`) {
		t.Fatal("covered lines must be marked")
	}
	mainPage, err := os.ReadFile(filepath.Join(out, "files", "cmd", "plugin-build", "main.go.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mainPage), `class="uncov"`) {
		t.Fatal("uncovered lines must be marked")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
