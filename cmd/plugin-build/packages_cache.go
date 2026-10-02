package main

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ct "github.com/randy-girard/flynn/controller/types"
)

const packagesCacheVersion = "flynn-plugin-packages-v1"

type packagesCacheMeta struct {
	Layer  *ct.ImageLayer `json:"layer"`
	Goarch string         `json:"goarch"`
}

func needsPackagesLayer(plugin *pluginManifest) bool {
	if plugin == nil {
		return false
	}
	return plugin.Build.Packages != "" || plugin.Build.Setup != ""
}

func packagesCacheDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PLUGIN_BUILD_NO_PACKAGES_CACHE"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func packagesCacheID(repo string, plugin *pluginManifest, base *resolvedBase) (string, error) {
	h := sha512.New512_256()
	if _, err := io.WriteString(h, packagesCacheVersion+"\n"); err != nil {
		return "", err
	}
	if base != nil {
		for _, layer := range base.Layers {
			if layer == nil {
				continue
			}
			sum := ""
			if layer.Hashes != nil {
				sum = layer.Hashes["sha512_256"]
			}
			if _, err := fmt.Fprintf(h, "base %s %s %d\n", layer.ID, sum, layer.Length); err != nil {
				return "", err
			}
		}
	}
	if plugin.Build.Setup != "" {
		if err := hashNamedFile(h, "setup", filepath.Join(repo, plugin.Build.Setup)); err != nil {
			return "", err
		}
	}
	if plugin.Build.Packages != "" {
		pkgdir := filepath.Join(repo, filepath.Dir(plugin.Build.Packages))
		if err := hashNamedTree(h, "packages", pkgdir); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func packagesCachePath(id string) (string, error) {
	cache, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "packages", id), nil
}

func loadPackagesCache(id, outDir string) (*ct.ImageLayer, string, error) {
	dir, err := packagesCachePath(id)
	if err != nil {
		return nil, "", err
	}
	metaPath := filepath.Join(dir, "layer.json")
	squash := filepath.Join(dir, "layer.squashfs")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	if _, err := os.Stat(squash); err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	var meta packagesCacheMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		fmt.Fprintf(os.Stderr, "note: ignoring packages cache %s (%v)\n", dir, err)
		return nil, "", nil
	}
	if meta.Layer == nil || meta.Layer.ID == "" {
		return nil, "", nil
	}
	if err := verifyLayerFile(squash, meta.Layer); err != nil {
		fmt.Fprintf(os.Stderr, "note: ignoring packages cache %s (%v)\n", dir, err)
		return nil, "", nil
	}
	dst := filepath.Join(outDir, "layers", meta.Layer.ID+".squashfs")
	if err := copyFile(squash, dst); err != nil {
		return nil, "", err
	}
	return meta.Layer, meta.Goarch, nil
}

func savePackagesCache(id, outDir string, layer *ct.ImageLayer, goarch string) error {
	if layer == nil || layer.ID == "" {
		return fmt.Errorf("packages cache: missing layer")
	}
	dir, err := packagesCachePath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	src := filepath.Join(outDir, "layers", layer.ID+".squashfs")
	if err := copyFile(src, filepath.Join(dir, "layer.squashfs")); err != nil {
		return err
	}
	raw, err := json.Marshal(packagesCacheMeta{Layer: layer, Goarch: goarch})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "layer.json"), raw, 0644)
}

func hashNamedFile(h io.Writer, label, path string) error {
	if _, err := fmt.Fprintf(h, "%s\n", label); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(h, f)
	if err != nil {
		return err
	}
	_, err = io.WriteString(h, "\n")
	return err
}

func hashNamedTree(h io.Writer, label, dir string) error {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Name() == ".DS_Store" {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	if _, err := fmt.Fprintf(h, "%s %d\n", label, len(files)); err != nil {
		return err
	}
	for _, path := range files {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if err := hashNamedFile(h, label+"/"+filepath.ToSlash(rel), path); err != nil {
			return err
		}
	}
	return nil
}
