package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func installBinaries(repo, rootfs string, plugin *pluginManifest, goarch string) error {
	if !supportedGoarch(goarch) {
		return fmt.Errorf("unsupported GOARCH %s (amd64 or arm64)", goarch)
	}
	binDir, err := os.MkdirTemp("", "flynn-plugin-bin-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(binDir)

	for pkg, dest := range plugin.Build.Go {
		if dest == "" {
			return fmt.Errorf("build.go[%q]: empty destination", pkg)
		}
		out := filepath.Join(binDir, filepath.Base(dest))
		fmt.Fprintf(os.Stderr, "go build %s GOARCH=%s -> %s\n", pkg, goarch, dest)
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, pkg)
		cmd.Dir = repo
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Env = goBuildEnv(goarch)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go build %s: %w", pkg, err)
		}
		got, err := elfGoarch(out)
		if err != nil {
			return fmt.Errorf("go build %s: %w", pkg, err)
		}
		if got != goarch {
			return fmt.Errorf("go build %s produced %s, want %s (match the Flynn OS layer)", pkg, got, goarch)
		}
		into, err := overlayDest(rootfs, dest)
		if err != nil {
			return fmt.Errorf("build.go[%q]: %w", pkg, err)
		}
		if err := installFile(out, into, 0755); err != nil {
			return err
		}
	}

	for src, dest := range plugin.Build.Copy {
		if dest == "" {
			return fmt.Errorf("build.copy[%q]: empty destination", src)
		}
		into, err := overlayDest(rootfs, dest)
		if err != nil {
			return fmt.Errorf("build.copy[%q]: %w", src, err)
		}
		if err := installFile(filepath.Join(repo, src), into, 0755); err != nil {
			return err
		}
	}
	return nil
}

// overlayDest maps an image path into the overlay upper dir.
//
// filepath.Join(rootfs, "/bin/foo") is unsafe on some Go versions (absolute
// dests drop rootfs). More importantly, Ubuntu noble is usr-merged: /bin is a
// symlink to usr/bin. Creating upper/bin/ as a real directory hides that
// symlink, so /bin/bash vanishes and Flynn reports
// fork/exec /bin/start-flynn-postgres: no such file or directory (missing
// shebang interpreter). Always store /bin/* as usr/bin/* in the delta.
func overlayDest(rootfs, dest string) (string, error) {
	dest = filepath.Clean(dest)
	if dest == "." || dest == string(filepath.Separator) {
		return "", fmt.Errorf("destination %q is not a file path inside the image", dest)
	}
	rel := dest
	if filepath.IsAbs(dest) {
		rel = dest[len(string(filepath.Separator)):]
	}
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	if rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("destination %q escapes the overlay", dest)
	}
	relSlash := filepath.ToSlash(rel)
	if relSlash == "bin" || strings.HasPrefix(relSlash, "bin/") {
		rel = filepath.FromSlash("usr/" + relSlash)
	}
	return filepath.Join(rootfs, rel), nil
}

func imageDests(plugin *pluginManifest) []string {
	seen := map[string]struct{}{}
	var dests []string
	add := func(d string) {
		d = filepath.Clean(d)
		if d == "" || d == "." {
			return
		}
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		dests = append(dests, d)
	}
	for _, d := range plugin.Build.Entrypoint {
		add(d)
	}
	for _, d := range plugin.Build.Go {
		add(d)
	}
	for _, d := range plugin.Build.Copy {
		add(d)
	}
	return dests
}

func goBuildEnv(goarch string) []string {
	var env []string
	for _, e := range os.Environ() {
		if e == "GOCACHE=" {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+goarch,
		"GOFLAGS=-mod=mod -buildvcs=false",
	)
	if strings.TrimSpace(os.Getenv("GOCACHE")) != "" {
		return env
	}
	dir, err := cacheDir()
	if err != nil {
		return env
	}
	gocache := filepath.Join(dir, "go-build")
	if err := os.MkdirAll(gocache, 0755); err != nil {
		return env
	}
	return append(env, "GOCACHE="+gocache)
}

func installFile(src, dest string, mode os.FileMode) error {
	if err := sudoCommand("mkdir", "-p", filepath.Dir(dest)).Run(); err != nil {
		return err
	}
	if err := sudoCommand("install", "-m", fmt.Sprintf("%04o", mode), src, dest).Run(); err != nil {
		return fmt.Errorf("install %s -> %s: %w", src, dest, err)
	}
	return nil
}
