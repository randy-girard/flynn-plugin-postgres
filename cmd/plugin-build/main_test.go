package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePluginKindsAndRepoFiles(t *testing.T) {
	root := t.TempDir()
	if err := validatePlugin(root, &pluginManifest{}); err == nil {
		t.Fatal("missing kind")
	}
	if err := validatePlugin(root, &pluginManifest{Kind: "service"}); err == nil {
		t.Fatal("unknown kind")
	}
	if err := validatePlugin(root, &pluginManifest{Kind: "resource-provider"}); err == nil {
		t.Fatal("resource-provider needs provider")
	}
	if err := validatePlugin(root, &pluginManifest{Kind: "resource-provider", Provider: json.RawMessage("null")}); err == nil {
		t.Fatal("null provider")
	}
	ok := &pluginManifest{
		Kind:     "resource-provider",
		Provider: json.RawMessage(`{"name":"redis","url":"http://redis-api.discoverd/clusters"}`),
	}
	if err := validatePlugin(root, ok); err != nil {
		t.Fatal(err)
	}
	if err := validatePlugin(root, &pluginManifest{Kind: "app"}); err != nil {
		t.Fatal(err)
	}

	ok.Hooks.Install = "hooks/install.sh"
	if err := validatePlugin(root, ok); err == nil {
		t.Fatal("missing hook file")
	}
	if err := os.MkdirAll(filepath.Join(root, "hooks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hooks", "install.sh"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := validatePlugin(root, ok); err != nil {
		t.Fatal(err)
	}

	ok.Build.Copy = map[string]string{"../secret": "/secret"}
	if err := validatePlugin(root, ok); err == nil || !strings.Contains(err.Error(), "inside the plugin repo") {
		t.Fatalf("path escape: %v", err)
	}
	ok.Build.Copy = map[string]string{"/etc/passwd": "/passwd"}
	if err := validatePlugin(root, ok); err == nil {
		t.Fatal("absolute copy")
	}
	ok.Build.Copy = map[string]string{"hooks": "/hooks"}
	if err := validatePlugin(root, ok); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory copy: %v", err)
	}
	ok.Build.Copy = map[string]string{"foo/../secret": "/secret"}
	if err := validatePlugin(root, ok); err == nil || !strings.Contains(err.Error(), "inside the plugin repo") {
		t.Fatalf("dot-dot copy: %v", err)
	}
}

func TestApplyBuildDefaults(t *testing.T) {
	root := t.TempDir()
	plugin := &pluginManifest{}
	applyBuildDefaults(root, plugin)
	if plugin.Build.Packages != "" {
		t.Fatalf("missing packages.sh: %s", plugin.Build.Packages)
	}
	if err := os.MkdirAll(filepath.Join(root, "img"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "img", "packages.sh"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	applyBuildDefaults(root, plugin)
	if plugin.Build.Packages != "img/packages.sh" {
		t.Fatalf("got %s", plugin.Build.Packages)
	}
	plugin.Build.Packages = "custom.sh"
	applyBuildDefaults(root, plugin)
	if plugin.Build.Packages != "custom.sh" {
		t.Fatal("must not overwrite an explicit packages path")
	}
}

func TestPublicURLs(t *testing.T) {
	layer, image, artifact := publicURLs("acme/plug", "v1", "dist", "manif")
	if layer != "https://github.com/acme/plug/releases/download/v1/{id}.squashfs" {
		t.Fatalf("layer=%s", layer)
	}
	if image != "https://github.com/acme/plug/releases/download/v1/manif.json" {
		t.Fatalf("image=%s", image)
	}
	if artifact != "https://github.com/acme/plug/releases/download/v1/image.json" {
		t.Fatalf("artifact=%s", artifact)
	}
	layer, _, _ = publicURLs("acme/plug", "dev", "/tmp/out", "manif")
	if !strings.HasPrefix(layer, "file://") || !strings.Contains(layer, "/layers/{id}.squashfs") {
		t.Fatalf("dev build must use file URLs: %s", layer)
	}
	layer, _, _ = publicURLs("", "v1", "/tmp/out", "manif")
	if !strings.HasPrefix(layer, "file://") {
		t.Fatalf("missing github repo: %s", layer)
	}
}

func TestWritePluginManifestAndLoad(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "flynn-plugin.json")
	raw := []byte(`{"name":"widget","kind":"app"}`)
	if err := os.WriteFile(src, raw, 0644); err != nil {
		t.Fatal(err)
	}
	got, data, err := loadPlugin(src)
	if err != nil || got.Name != "widget" || got.Kind != "app" || len(data) == 0 {
		t.Fatalf("%+v %v", got, err)
	}

	out := filepath.Join(root, "dist", "flynn-plugin.json")
	if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		t.Fatal(err)
	}
	if err := writePluginManifest(out, raw, "https://example/image.json", &resolvedBase{
		Repo:    "acme/flynn",
		Version: "v1",
		Image:   "ubuntu-noble",
	}); err != nil {
		t.Fatal(err)
	}
	var obj map[string]interface{}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	arts := obj["artifacts"].(map[string]interface{})
	if arts["image"] != "https://example/image.json" {
		t.Fatalf("artifacts=%v", arts)
	}
	build := obj["build"].(map[string]interface{})
	base := build["base"].(map[string]interface{})
	if base["repo"] != "acme/flynn" || base["image"] != "ubuntu-noble" {
		t.Fatalf("base=%v", base)
	}
}

func TestEnvOrAndFirstNonEmpty(t *testing.T) {
	t.Setenv("PLUGIN_BUILD_TEST_KEY", "")
	if envOr("PLUGIN_BUILD_TEST_KEY", "fallback") != "fallback" {
		t.Fatal("empty env")
	}
	t.Setenv("PLUGIN_BUILD_TEST_KEY", "set")
	if envOr("PLUGIN_BUILD_TEST_KEY", "fallback") != "set" {
		t.Fatal("set env")
	}
	if firstNonEmpty("", " ", "b") != " " {
		t.Fatalf("plugin-build firstNonEmpty keeps spaces: %q", firstNonEmpty("", " ", "b"))
	}
	if firstNonEmpty("", "", "b") != "b" || firstNonEmpty() != "" {
		t.Fatal("firstNonEmpty")
	}
}

func TestWriteCompactJSONDoesNotEscapeHTML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.json")
	if err := writeCompactJSON(path, map[string]string{"sha": "abc<def>"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "abc<def>") {
		t.Fatalf("layer hashes must not be HTML-escaped: %s", got)
	}
	if strings.Contains(got, "\\u003c") {
		t.Fatalf("unexpected unicode escape: %s", got)
	}
}

func TestCopyHookAssets(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "dist")
	if err := os.MkdirAll(filepath.Join(root, "script"), 0755); err != nil {
		t.Fatal(err)
	}
	body := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(root, "script", "uninstall.sh"), body, 0755); err != nil {
		t.Fatal(err)
	}
	if err := copyHookAssets(root, out, pluginHooks{Uninstall: "script/uninstall.sh"}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(out, "script-uninstall.sh")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("copied=%q", got)
	}
	st, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0111 == 0 {
		t.Fatalf("hook asset must be executable, mode=%s", st.Mode())
	}
	if hookAssetName("script/uninstall.sh") != "script-uninstall.sh" {
		t.Fatal(hookAssetName("script/uninstall.sh"))
	}
	if err := copyHookAssets(root, out, pluginHooks{Uninstall: "../secret"}); err == nil {
		t.Fatal("path escape")
	}
}

func TestRepoFlynnPluginManifestUninstallHook(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "flynn-plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Hooks struct {
			Uninstall string `json:"uninstall"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Hooks.Uninstall != "script/uninstall.sh" {
		t.Fatalf("hooks.uninstall=%q", m.Hooks.Uninstall)
	}
	if _, err := os.Stat(filepath.Join(root, "script", "uninstall.sh")); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseWorkflowUploadsHookScripts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "github_release_publish") {
		t.Fatal("Build and Release must upload assets one file at a time")
	}
	if !strings.Contains(body, "flatten-plugin-release.sh") {
		t.Fatal("Build and Release must flatten only the plugin delta")
	}
	if strings.Contains(body, "cp dist/layers/*.squashfs dist/") {
		t.Fatal("must not re-upload Flynn ubuntu-noble from dist/layers")
	}
}

func TestFlattenPluginReleaseCopiesOnlyDelta(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "script", "lib", "flatten-plugin-release.sh")
	dist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dist, "layers"), 0755); err != nil {
		t.Fatal(err)
	}
	osID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pkgID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	binID := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	image := map[string]interface{}{
		"manifest": map[string]interface{}{
			"rootfs": []interface{}{
				map[string]interface{}{
					"layers": []interface{}{
						map[string]interface{}{"id": osID},
						map[string]interface{}{"id": pkgID},
						map[string]interface{}{"id": binID},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "image.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "layers", osID+".squashfs"), []byte("os"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "layers", pkgID+".squashfs"), []byte("packages"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "layers", binID+".squashfs"), []byte("binaries"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, osID+".squashfs"), []byte("leftover-os"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, dist)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("flatten: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dist, osID+".squashfs")); !os.IsNotExist(err) {
		t.Fatal("Flynn ubuntu-noble must not be copied to the release dir")
	}
	got, err := os.ReadFile(filepath.Join(dist, pkgID+".squashfs"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "packages" {
		t.Fatalf("packages %q", got)
	}
	got, err = os.ReadFile(filepath.Join(dist, binID+".squashfs"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binaries" {
		t.Fatalf("binaries %q", got)
	}
	if _, err := os.Stat(filepath.Join(dist, "layers", osID+".squashfs")); err != nil {
		t.Fatal("local DistReady still needs ubuntu-noble under dist/layers")
	}
}

func TestFlynnGoModUsesRandyGirardRepo(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(mod)

	modLine := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "module ") {
			modLine = trimmed
			break
		}
	}
	if !strings.HasPrefix(modLine, "module github.com/randy-girard/") {
		t.Fatalf("go.mod must declare module github.com/randy-girard/..., got %q", modLine)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "flynn-plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pluginJSON struct {
		Build struct {
			Go map[string]string `json:"go"`
		} `json:"build"`
	}
	if err := json.Unmarshal(manifest, &pluginJSON); err != nil {
		t.Fatal(err)
	}
	if len(pluginJSON.Build.Go) == 0 {
		t.Fatal("flynn-plugin.json build.go must list plugin packages as ./cmd/...")
	}
	for pkg := range pluginJSON.Build.Go {
		if !strings.HasPrefix(pkg, "./") {
			t.Fatalf("build.go package %q must be a repo-relative ./cmd/... path", pkg)
		}
	}
	if !strings.Contains(body, "github.com/randy-girard/flynn ") {
		t.Fatal("go.mod must require github.com/randy-girard/flynn (the Flynn git repo)")
	}
	if strings.Contains(body, "github.com/flynn/flynn") {
		t.Fatal("go.mod must not require github.com/flynn/flynn")
	}
	if strings.Contains(body, "replace github.com/randy-girard/flynn => ../flynn") {
		t.Fatal("go.mod must not replace Flynn with a sibling checkout")
	}
	if _, err := os.Stat(filepath.Join(root, "vendor")); err == nil {
		t.Fatal("plugins must not vendor Flynn; the repo is declared in go.mod")
	}
	linux, err := os.ReadFile(filepath.Join(root, "script", "lib", "linux-container.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(linux)
	if strings.Contains(script, "plugin_flynn_root") || strings.Contains(script, ":/flynn") {
		t.Fatal("linux-container.sh must not require a sibling Flynn checkout")
	}
	if !strings.Contains(script, "-mod=mod") {
		t.Fatal("linux-container.sh must use -mod=mod")
	}
	if !strings.Contains(script, "GOPRIVATE") {
		t.Fatal("linux-container.sh must set GOPRIVATE for github.com/randy-girard")
	}
	gobuild, err := os.ReadFile(filepath.Join(root, "cmd", "plugin-build", "gobuild.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gobuild), `GOFLAGS=-mod=mod -buildvcs=false`) {
		t.Fatal("plugin-build must compile plugin binaries with -mod=mod")
	}
	for _, rel := range []string{
		filepath.Join(".github", "workflows", "ci.yml"),
		filepath.Join(".github", "workflows", "release.yml"),
	} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if strings.Contains(text, "Checkout Flynn sibling") {
			t.Fatalf("%s must not clone a Flynn sibling", rel)
		}
		if strings.Contains(text, "-mod=vendor") {
			t.Fatalf("%s must not use -mod=vendor", rel)
		}
		if !strings.Contains(text, "GOPRIVATE") || !strings.Contains(text, "github.com/randy-girard") {
			t.Fatalf("%s must set GOPRIVATE=github.com/randy-girard/*", rel)
		}
	}
	if strings.Contains(body, "\nreplace ") || strings.HasPrefix(strings.TrimSpace(body), "replace ") {
		t.Fatal("go.mod must not contain replace directives; each plugin repo builds independently")
	}
	if strings.Contains(body, "=> ../") {
		t.Fatal("go.mod must not replace modules with sibling checkouts")
	}
	if _, err := os.Stat(filepath.Join(root, "go.work")); err == nil {
		t.Fatal("plugins must not use go.work; each repo builds independently of Flynn and other plugins")
	}
}
