// Command plugin-build turns a Flynn plugin repo into the squashfs layers and
// Artifact JSON that flynn-host plugin install will pull from GitHub Releases.
//
// The OS rootfs is Flynn's ubuntu-noble layer from a Flynn GitHub Release
// (images.json.gz). This tool overlays img/packages.sh + gobuild/copy and
// mksquashfs only the delta. No Docker; no Ubuntu cloudimg download.
//
// Output (under -out, default dist/):
//
//	image.json              Flynn Artifact (type=flynn) with HTTPS or file:// URLs
//	<manifest-id>.json      ImageManifest bytes that Artifact.URI points at
//	layers/<layer-id>.squashfs  Flynn ubuntu-noble plus the plugin delta
//	flynn-plugin.json       copy of the plugin manifest with artifacts.image filled in
//	script-uninstall.sh     declared hooks (script/uninstall.sh flattened for GitHub)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	ct "github.com/randy-girard/flynn/controller/types"
)

type pluginManifest struct {
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	Provider  json.RawMessage   `json:"provider,omitempty"`
	App       json.RawMessage   `json:"app,omitempty"`
	InjectEnv []string          `json:"inject_env,omitempty"`
	ImageEnv  map[string]string `json:"image_env,omitempty"`
	CLI       json.RawMessage   `json:"cli,omitempty"`
	Hooks     pluginHooks       `json:"hooks,omitempty"`
	Build     pluginBuild       `json:"build"`
	Artifacts pluginArtifacts   `json:"artifacts"`
}

type pluginHooks struct {
	Install   string `json:"install,omitempty"`
	Upgrade   string `json:"upgrade,omitempty"`
	Uninstall string `json:"uninstall,omitempty"`
}

// pluginBuild is the Flynn-builder fragment: overlay packages.sh + gobuild + copy
// on Flynn's published ubuntu-noble layer.
type pluginBuild struct {
	Base       pluginBase        `json:"base"`
	Entrypoint []string          `json:"entrypoint"`
	Packages   string            `json:"packages"`
	Setup      string            `json:"setup"`
	Go         map[string]string `json:"go"`
	Copy       map[string]string `json:"copy"`
}

type pluginArtifacts struct {
	Image string `json:"image"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "plugin-build: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	var (
		dir          = flag.String("dir", root, "plugin repository root")
		out          = flag.String("out", "dist", "output directory (relative to -dir unless absolute)")
		version      = flag.String("version", envOr("PLUGIN_VERSION", envOr("VERSION", "dev")), "plugin version / git tag (e.g. v20260914.0.0)")
		githubRepo   = flag.String("github-repo", envOr("GITHUB_REPOSITORY", ""), "GitHub owner/repo for this plugin's release download URLs")
		flynnRepo    = flag.String("flynn-repo", envOr("FLYNN_GITHUB_REPO", ""), "Flynn GitHub repo that publishes ubuntu-noble")
		flynnVersion = flag.String("flynn-version", envOr("FLYNN_VERSION", envOr("PLUGIN_FLYNN_VERSION", "")), "Flynn release tag, or latest")
		flynnBaseImg = flag.String("flynn-base-image", envOr("FLYNN_BASE_IMAGE", ""), "images.json key whose first layer is ubuntu-noble")
		skipImage    = flag.Bool("skip-image", false, "validate flynn-plugin.json only; do not build squashfs")
	)
	flag.Parse()

	dirAbs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	outDir := *out
	if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(dirAbs, outDir)
	}

	manifestPath := filepath.Join(dirAbs, "flynn-plugin.json")
	plugin, rawPlugin, err := loadPlugin(manifestPath)
	if err != nil {
		return err
	}
	if plugin.Name == "" {
		return fmt.Errorf("%s: missing name", manifestPath)
	}
	applyBuildDefaults(dirAbs, plugin)
	if err := validatePlugin(dirAbs, plugin); err != nil {
		return fmt.Errorf("%s: %w", manifestPath, err)
	}
	if len(plugin.Build.Entrypoint) == 0 {
		return fmt.Errorf("%s: build.entrypoint is required", manifestPath)
	}

	baseRepo := firstNonEmpty(*flynnRepo, plugin.Build.Base.Repo, defaultFlynnRepo)
	baseVersion := firstNonEmpty(*flynnVersion, plugin.Build.Base.Version, "latest")
	baseImage := firstNonEmpty(*flynnBaseImg, plugin.Build.Base.Image, defaultBaseImage)

	if *skipImage {
		fmt.Printf("ok: %s (%s) base %s@%s\n", plugin.Name, plugin.Kind, baseRepo, baseVersion)
		return nil
	}

	if runtime.GOOS != "linux" {
		return fmt.Errorf("image builds require Linux (Flynn squashfs layers); this host is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if !supportedGoarch(runtime.GOARCH) {
		return fmt.Errorf("image builds require linux/amd64 or linux/arm64; this host is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		return fmt.Errorf("mksquashfs is required (apt install squashfs-tools): %w", err)
	}
	if _, err := exec.LookPath("unsquashfs"); err != nil {
		return fmt.Errorf("unsquashfs is required (apt install squashfs-tools): %w", err)
	}

	cache, err := cacheDir()
	if err != nil {
		return err
	}
	base, err := resolveFlynnBase(baseRepo, baseVersion, baseImage, cache)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(outDir, "layers"), 0755); err != nil {
		return err
	}
	for i, src := range base.Files {
		dst := filepath.Join(outDir, "layers", base.Layers[i].ID+".squashfs")
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copy Flynn base layer: %w", err)
		}
	}

	layerFile, pluginLayer, goarch, err := buildPluginLayers(dirAbs, outDir, plugin, base)
	if err != nil {
		return err
	}

	layers := append(append([]*ct.ImageLayer{}, base.Layers...), pluginLayer)
	filesMeta := strings.Join(imageDests(plugin), ",")
	platform := &ct.ImagePlatform{OS: "linux", Architecture: goarch}

	manifest := &ct.ImageManifest{
		Type: ct.ImageManifestTypeV1,
		Meta: map[string]string{
			"flynn.component":         plugin.Name,
			"flynn.plugin":            "true",
			"flynn.plugin.base":       base.Repo + "@" + base.Version,
			"flynn.plugin.base.image": base.Image,
			"flynn.plugin.files":      filesMeta,
			"flynn.plugin.arch":       goarch,
		},
		Entrypoints: map[string]*ct.ImageEntrypoint{
			"_default": {Args: plugin.Build.Entrypoint},
		},
		Rootfs: []*ct.ImageRootfs{{
			Platform: platform,
			Layers:   layers,
		}},
	}
	rawManifest := manifest.RawManifest()
	manifestID := manifest.ID()

	layerURLTemplate, imageURI, artifactURL := publicURLs(*githubRepo, *version, outDir, manifestID)

	artifact := &ct.Artifact{
		Type:             ct.ArtifactTypeFlynn,
		URI:              imageURI,
		RawManifest:      rawManifest,
		Hashes:           manifest.Hashes(),
		Size:             int64(len(rawManifest)),
		LayerURLTemplate: layerURLTemplate,
		Meta: map[string]string{
			"manifest.id":             manifestID,
			"flynn.component":         plugin.Name,
			"flynn.plugin":            "true",
			"flynn.plugin.version":    *version,
			"flynn.plugin.base":       base.Repo + "@" + base.Version,
			"flynn.plugin.base.image": base.Image,
			"flynn.plugin.files":      filesMeta,
			"flynn.plugin.arch":       goarch,
		},
	}

	imageJSONPath := filepath.Join(outDir, "image.json")
	if err := writeCompactJSON(imageJSONPath, artifact); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, manifestID+".json"), rawManifest, 0644); err != nil {
		return err
	}
	if err := writePluginManifest(filepath.Join(outDir, "flynn-plugin.json"), rawPlugin, artifactURL, base); err != nil {
		return err
	}
	if err := copyHookAssets(dirAbs, outDir, plugin.Hooks); err != nil {
		return err
	}

	fmt.Printf("plugin:     %s\n", plugin.Name)
	fmt.Printf("version:    %s\n", *version)
	fmt.Printf("arch:       linux/%s\n", goarch)
	fmt.Printf("flynn_base: %s@%s (%s)\n", base.Repo, base.Version, base.Image)
	fmt.Printf("image:      %s\n", imageJSONPath)
	fmt.Printf("manifest:   %s\n", manifestID)
	fmt.Printf("layers:     %d (1 plugin delta)\n", len(layers))
	fmt.Printf("delta:      %s (%d bytes)\n", pluginLayer.ID, pluginLayer.Length)
	fmt.Printf("delta_file: %s\n", layerFile)
	fmt.Printf("layer_url:  %s\n", layerURLTemplate)
	fmt.Printf("artifact:   %s\n", artifactURL)
	return nil
}

func applyBuildDefaults(root string, plugin *pluginManifest) {
	if plugin.Build.Packages == "" {
		if _, err := os.Stat(filepath.Join(root, "img/packages.sh")); err == nil {
			plugin.Build.Packages = "img/packages.sh"
		}
	}
}

func validatePlugin(root string, plugin *pluginManifest) error {
	switch plugin.Kind {
	case "resource-provider":
		if len(plugin.Provider) == 0 || string(plugin.Provider) == "null" {
			return fmt.Errorf("kind %q requires provider", plugin.Kind)
		}
	case "app":
	case "":
		return fmt.Errorf("missing kind (resource-provider or app)")
	default:
		return fmt.Errorf("unknown kind %q (resource-provider or app)", plugin.Kind)
	}
	for _, spec := range []struct {
		name string
		rel  string
	}{
		{"hooks.install", plugin.Hooks.Install},
		{"hooks.upgrade", plugin.Hooks.Upgrade},
		{"hooks.uninstall", plugin.Hooks.Uninstall},
		{"build.packages", plugin.Build.Packages},
		{"build.setup", plugin.Build.Setup},
	} {
		if spec.rel == "" {
			continue
		}
		if err := repoFile(root, spec.name, spec.rel); err != nil {
			return err
		}
	}
	for src := range plugin.Build.Copy {
		if err := repoFile(root, "build.copy", src); err != nil {
			return err
		}
	}
	return nil
}

func copyHookAssets(root, outDir string, hooks pluginHooks) error {
	for _, rel := range []string{hooks.Install, hooks.Upgrade, hooks.Uninstall} {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		name := hookAssetName(rel)
		if name == "" || name == "." {
			return fmt.Errorf("hooks path %q is invalid", rel)
		}
		src := filepath.Join(root, filepath.FromSlash(rel))
		dst := filepath.Join(outDir, name)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("hooks %s: %w", rel, err)
		}
		if err := os.Chmod(dst, 0755); err != nil {
			return fmt.Errorf("hooks %s: %w", rel, err)
		}
	}
	return nil
}

// hookAssetName matches Flynn pkg/plugin.HookAssetNames: GitHub assets are a
// flat list, so script/uninstall.sh is published as script-uninstall.sh.
func hookAssetName(rel string) string {
	rel = filepath.ToSlash(filepath.Clean(strings.TrimSpace(rel)))
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, "../") {
		return ""
	}
	return strings.ReplaceAll(rel, "/", "-")
}

func repoFile(root, field, rel string) error {
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return fmt.Errorf("%s must be a path inside the plugin repo", field)
	}
	st, err := os.Stat(filepath.Join(root, rel))
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if st.IsDir() {
		return fmt.Errorf("%s %q is a directory", field, rel)
	}
	return nil
}

func loadPlugin(path string) (*pluginManifest, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var p pluginManifest
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &p, data, nil
}

func writePluginManifest(path string, raw []byte, imageURL string, base *resolvedBase) error {
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	artifacts, _ := obj["artifacts"].(map[string]interface{})
	if artifacts == nil {
		artifacts = map[string]interface{}{}
		obj["artifacts"] = artifacts
	}
	artifacts["image"] = imageURL
	if base != nil {
		build, _ := obj["build"].(map[string]interface{})
		if build == nil {
			build = map[string]interface{}{}
			obj["build"] = build
		}
		b, _ := build["base"].(map[string]interface{})
		if b == nil {
			b = map[string]interface{}{}
			build["base"] = b
		}
		b["repo"] = base.Repo
		b["version"] = base.Version
		b["image"] = base.Image
	}
	return writeJSON(path, obj)
}

func writeJSON(path string, v interface{}) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeCompactJSON(path string, v interface{}) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func publicURLs(githubRepo, version, outDir, manifestID string) (layerURLTemplate, imageURI, artifactURL string) {
	if githubRepo != "" && version != "" && version != "dev" {
		base := fmt.Sprintf("https://github.com/%s/releases/download/%s", githubRepo, version)
		return base + "/{id}.squashfs", base + "/" + manifestID + ".json", base + "/image.json"
	}
	abs, err := filepath.Abs(outDir)
	if err != nil {
		abs = outDir
	}
	fileBase := "file://" + abs
	return fileBase + "/layers/{id}.squashfs", fileBase + "/" + manifestID + ".json", fileBase + "/image.json"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
