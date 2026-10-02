package main

import (
	"os"
	"path/filepath"
	"testing"

	ct "github.com/randy-girard/flynn/controller/types"
)

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
