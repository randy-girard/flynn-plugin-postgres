package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ct "github.com/randy-girard/flynn/controller/types"
)

func TestRepoPinsEngineTarballForAmd64AndArm64(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fetch, err := os.ReadFile(filepath.Join(root, "img", "fetch-engine.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fetch), ".tar.gz") {
		t.Fatal("fetch-engine.sh must pin an upstream source tarball")
	}
	pkgs, err := os.ReadFile(filepath.Join(root, "img", "packages.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(pkgs)
	if !strings.Contains(src, "x86_64") || !strings.Contains(src, "aarch64") {
		t.Fatal("packages.sh must compile the vendored tarball on amd64 and arm64")
	}
	if !strings.Contains(src, "missing vendored") {
		t.Fatal("packages.sh must use the vendored dist in img/")
	}
	img := filepath.Join(root, "img")
	found := false
	ents, err := os.ReadDir(img)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		name := ent.Name()
		if strings.Contains(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") || strings.Contains(name, ".tgz.") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("img/ must store the engine tarball (or GitHub-safe split parts) in the repo")
	}
}

func TestNeedsPackagesLayer(t *testing.T) {
	if needsPackagesLayer(nil) || needsPackagesLayer(&pluginManifest{}) {
		t.Fatal("empty build must skip packages")
	}
	if !needsPackagesLayer(&pluginManifest{Build: pluginBuild{Packages: "img/packages.sh"}}) {
		t.Fatal("packages.sh")
	}
	if !needsPackagesLayer(&pluginManifest{Build: pluginBuild{Setup: "img/setup.sh"}}) {
		t.Fatal("setup")
	}
}

func TestPackagesCacheIDStableAndSensitive(t *testing.T) {
	root := t.TempDir()
	img := filepath.Join(root, "img")
	if err := os.MkdirAll(img, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(img, "packages.sh"), []byte("apt-get install curl\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(img, "apt-slim-finish.sh"), []byte("apt-get clean\n"), 0644); err != nil {
		t.Fatal(err)
	}
	plugin := &pluginManifest{Build: pluginBuild{Packages: "img/packages.sh"}}
	base := &resolvedBase{Layers: []*ct.ImageLayer{{
		ID:     "os",
		Length: 10,
		Hashes: map[string]string{"sha512_256": "abc"},
	}}}
	a, err := packagesCacheID(root, plugin, base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := packagesCacheID(root, plugin, base)
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || a != b {
		t.Fatalf("stable id: %q %q", a, b)
	}
	if err := os.WriteFile(filepath.Join(img, "packages.sh"), []byte("apt-get install wget\n"), 0644); err != nil {
		t.Fatal(err)
	}
	changed, err := packagesCacheID(root, plugin, base)
	if err != nil {
		t.Fatal(err)
	}
	if changed == a {
		t.Fatal("packages.sh change must bust the cache")
	}
	if err := os.WriteFile(filepath.Join(img, "packages.sh"), []byte("apt-get install curl\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(img, "apt-slim-finish.sh"), []byte("apt-get clean; true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	helper, err := packagesCacheID(root, plugin, base)
	if err != nil {
		t.Fatal(err)
	}
	if helper == a {
		t.Fatal("img helper change must bust the cache")
	}
	base.Layers[0].ID = "os-other"
	baseBusted, err := packagesCacheID(root, plugin, base)
	if err != nil {
		t.Fatal(err)
	}
	if baseBusted == helper {
		t.Fatal("ubuntu-noble layer id must be part of the cache key")
	}
}

func TestPackagesCacheRoundTrip(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("FLYNN_PLUGIN_BUILD_CACHE", cache)
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "layers"), 0755); err != nil {
		t.Fatal(err)
	}
	blob := []byte("delta\n")
	tmp := filepath.Join(out, "tmp.squashfs")
	if err := os.WriteFile(tmp, blob, 0644); err != nil {
		t.Fatal(err)
	}
	layer, err := hashLayer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(out, "layers", layer.ID+".squashfs")); err != nil {
		t.Fatal(err)
	}

	if err := savePackagesCache("inputhash", out, layer, "arm64"); err != nil {
		t.Fatal(err)
	}
	out2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out2, "layers"), 0755); err != nil {
		t.Fatal(err)
	}
	loaded, arch, err := loadPackagesCache("inputhash", out2)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.ID != layer.ID || arch != "arm64" {
		t.Fatalf("loaded=%+v arch=%q", loaded, arch)
	}
	copied, err := os.ReadFile(filepath.Join(out2, "layers", layer.ID+".squashfs"))
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != string(blob) {
		t.Fatalf("copied %q", copied)
	}
	miss, _, err := loadPackagesCache("missing", out2)
	if err != nil || miss != nil {
		t.Fatalf("miss=%+v err=%v", miss, err)
	}
}

func TestPackagesCacheDisabled(t *testing.T) {
	t.Setenv("PLUGIN_BUILD_NO_PACKAGES_CACHE", "")
	if packagesCacheDisabled() {
		t.Fatal("empty")
	}
	t.Setenv("PLUGIN_BUILD_NO_PACKAGES_CACHE", "1")
	if !packagesCacheDisabled() {
		t.Fatal("1")
	}
}

func TestFetchEngineTarballsNoopWhenMissing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "img"), 0755); err != nil {
		t.Fatal(err)
	}
	plugin := &pluginManifest{Build: pluginBuild{Packages: "img/packages.sh"}}
	if err := fetchEngineTarballs(root, plugin); err != nil {
		t.Fatal(err)
	}
	if err := fetchEngineTarballs(root, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFetchEngineTarballsRunsAndBustsCache(t *testing.T) {
	root := t.TempDir()
	img := filepath.Join(root, "img")
	if err := os.MkdirAll(img, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(img, "packages.sh"), []byte("#!/bin/bash\n"), 0644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "${HERE}/vendor"
echo engine > "${HERE}/vendor/engine.tar.gz"
`
	if err := os.WriteFile(filepath.Join(img, "fetch-engine.sh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	plugin := &pluginManifest{Build: pluginBuild{Packages: "img/packages.sh"}}
	base := &resolvedBase{Layers: []*ct.ImageLayer{{
		ID:     "os",
		Length: 10,
		Hashes: map[string]string{"sha512_256": "abc"},
	}}}
	before, err := packagesCacheID(root, plugin, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := fetchEngineTarballs(root, plugin); err != nil {
		t.Fatal(err)
	}
	after, err := packagesCacheID(root, plugin, base)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("vendored tarball must be part of the packages cache key")
	}
	if err := os.WriteFile(filepath.Join(img, "vendor", "engine.tar.gz.partial"), []byte("tmp"), 0644); err != nil {
		t.Fatal(err)
	}
	partial, err := packagesCacheID(root, plugin, base)
	if err != nil {
		t.Fatal(err)
	}
	if partial != after {
		t.Fatal("in-progress .partial downloads must not bust the packages cache")
	}
}

func TestOverlayPackagesTmpfsDefaultAndOverride(t *testing.T) {
	t.Setenv("PLUGIN_BUILD_OVERLAY_TMPFS", "")
	if overlayPackagesTmpfs() != overlayPackagesTmpfsDefault {
		t.Fatalf("default %s", overlayPackagesTmpfs())
	}
	t.Setenv("PLUGIN_BUILD_OVERLAY_TMPFS", "12G")
	if overlayPackagesTmpfs() != "12G" {
		t.Fatalf("override %s", overlayPackagesTmpfs())
	}
}

func TestAptCacheDirsHonorsEnv(t *testing.T) {
	t.Setenv("FLYNN_NO_APT_ARCHIVE_CACHE", "1")
	t.Setenv("FLYNN_APT_ARCHIVE_CACHE", "")
	t.Setenv("FLYNN_DIR", "")
	archives, lists := aptCacheDirs()
	if archives != "" || lists != "" {
		t.Fatalf("disabled cache: %q %q", archives, lists)
	}
	dir := t.TempDir()
	t.Setenv("FLYNN_NO_APT_ARCHIVE_CACHE", "")
	t.Setenv("FLYNN_APT_ARCHIVE_CACHE", dir)
	archives, lists = aptCacheDirs()
	if archives != dir || lists != filepath.Join(dir, "_apt_lists") {
		t.Fatalf("explicit cache: %q %q", archives, lists)
	}
}

func TestHostGoarchUsesPluginGoarch(t *testing.T) {
	t.Setenv("PLUGIN_GOARCH", "arm64")
	got, err := hostGoarch()
	if err != nil || got != "arm64" {
		t.Fatalf("got %q %v", got, err)
	}
	t.Setenv("PLUGIN_GOARCH", "riscv64")
	if _, err := hostGoarch(); err == nil {
		t.Fatal("unsupported PLUGIN_GOARCH")
	}
}
