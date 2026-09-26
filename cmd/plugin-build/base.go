package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	ct "github.com/randy-girard/flynn/controller/types"
)

const (
	defaultFlynnRepo  = "randy-girard/flynn"
	defaultBaseImage  = "ubuntu-noble"
	ubuntuNobleImage  = "ubuntu-noble"
	githubAPIDefault  = "https://api.github.com"
	githubDownloadFmt = "https://github.com/%s/releases/download/%s/%s.squashfs"
	// busybox is ~2MiB; ubuntu-noble is ~200MiB. blobstore's first layer is
	// busybox after Flynn image-slim.
	minOSRootfsBytes = 32 << 20
)

// Flynn GitHub images.json.gz is the release template, which omits ubuntu-noble
// as a named image. These still stack the shared ubuntu-noble squashfs first.
var ubuntuNobleDonors = []string{
	"postgres",
	"gitreceive",
	"tarreceive",
	"dockerbuilder-24",
	"host",
	"taffy",
	"slugrunner-24",
}

var (
	githubAPIBase = githubAPIDefault
	githubHTTP    = &http.Client{Timeout: 60 * time.Second}
)

type pluginBase struct {
	Repo    string `json:"repo,omitempty"`
	Version string `json:"version,omitempty"`
	Image   string `json:"image,omitempty"`
}

type resolvedBase struct {
	Repo    string
	Version string
	Image   string
	Layers  []*ct.ImageLayer
	Files   []string
}

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

func resolveFlynnBase(repo, version, image, cacheDir string) (*resolvedBase, error) {
	if repo == "" {
		repo = defaultFlynnRepo
	}
	if image == "" {
		image = defaultBaseImage
	}
	localImages := strings.TrimSpace(os.Getenv("FLYNN_IMAGES_JSON"))
	if localImages != "" {
		if version == "" || version == "latest" {
			version = "local"
		}
	} else if version == "" || version == "latest" {
		tag, err := latestPublishedRelease(repo)
		if err != nil {
			return nil, err
		}
		version = tag
	}

	fmt.Fprintf(os.Stderr, "==> Flynn base %s@%s (image %s)\n", repo, version, image)

	images, err := loadFlynnImages(repo, version, cacheDir)
	if err != nil {
		return nil, err
	}
	layers, donor, err := pickBaseLayers(images, image)
	if err != nil {
		return nil, err
	}
	if donor != image {
		fmt.Fprintf(os.Stderr, "note: images.json has no usable %q rootfs; using %s layer 0 as ubuntu-noble\n", image, donor)
	}
	fmt.Fprintf(os.Stderr, "==> OS layer %s (%d bytes)\n", layers[0].ID, layers[0].Length)

	files := make([]string, 0, len(layers))
	for _, layer := range layers {
		path, err := fetchFlynnLayer(repo, version, layer, cacheDir)
		if err != nil {
			return nil, err
		}
		files = append(files, path)
	}
	return &resolvedBase{
		Repo:    repo,
		Version: version,
		Image:   donor,
		Layers:  layers,
		Files:   files,
	}, nil
}

func pickBaseLayers(images map[string]*ct.Artifact, image string) ([]*ct.ImageLayer, string, error) {
	if layers := artifactSquashfsLayers(images[ubuntuNobleImage]); len(layers) > 0 {
		return cloneLayers(layers), ubuntuNobleImage, nil
	}

	if layers := osRootfsPrefix(images[image]); len(layers) > 0 {
		return cloneLayers(layers), image, nil
	}

	for _, name := range ubuntuNobleDonors {
		if layers := osRootfsPrefix(images[name]); len(layers) > 0 {
			return cloneLayers(layers), name, nil
		}
	}
	for name, art := range images {
		if layers := osRootfsPrefix(art); len(layers) > 0 {
			return cloneLayers(layers), name, nil
		}
	}

	names := make([]string, 0, len(images))
	for k := range images {
		names = append(names, k)
	}
	return nil, "", fmt.Errorf("no ubuntu-noble OS layer in Flynn images.json (requested %q); have %s. blobstore/controller are busybox after image-slim — pin FLYNN_VERSION or set build.base.image to postgres", image, strings.Join(names, ", "))
}

func osRootfsPrefix(art *ct.Artifact) []*ct.ImageLayer {
	layers := artifactSquashfsLayers(art)
	if len(layers) == 0 || !isOSRootfsLayer(layers[0]) {
		return nil
	}
	return layers[:1]
}

func isOSRootfsLayer(l *ct.ImageLayer) bool {
	return l != nil && l.Length >= minOSRootfsBytes
}

func artifactSquashfsLayers(art *ct.Artifact) []*ct.ImageLayer {
	if art == nil {
		return nil
	}
	man := art.Manifest()
	if man == nil {
		return nil
	}
	var out []*ct.ImageLayer
	for _, rf := range man.Rootfs {
		if rf == nil {
			continue
		}
		for _, l := range rf.Layers {
			if l != nil {
				out = append(out, l)
			}
		}
	}
	return out
}

func cloneLayers(in []*ct.ImageLayer) []*ct.ImageLayer {
	out := make([]*ct.ImageLayer, len(in))
	for i, l := range in {
		cp := *l
		if l.Hashes != nil {
			cp.Hashes = make(map[string]string, len(l.Hashes))
			for k, v := range l.Hashes {
				cp.Hashes[k] = v
			}
		}
		if l.Meta != nil {
			cp.Meta = make(map[string]string, len(l.Meta))
			for k, v := range l.Meta {
				cp.Meta[k] = v
			}
		}
		out[i] = &cp
	}
	return out
}

func latestPublishedRelease(repo string) (string, error) {
	url := githubAPIBase + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "flynn-plugin-build")
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := githubHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("GitHub latest release for %s: %w", repo, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub latest release for %s: HTTP %d: %s", repo, res.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel githubRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("parse GitHub latest release: %w", err)
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("GitHub latest release for %s has no tag_name", repo)
	}
	return rel.TagName, nil
}

func loadFlynnImages(repo, version, cacheDir string) (map[string]*ct.Artifact, error) {
	if p := strings.TrimSpace(os.Getenv("FLYNN_IMAGES_JSON")); p != "" {
		fmt.Fprintf(os.Stderr, "using local images %s\n", p)
		return readImagesJSON(p)
	}
	dir := filepath.Join(cacheDir, "flynn", version)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	gzPath := filepath.Join(dir, "images.json.gz")
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/images.json.gz", repo, version)
	if _, err := os.Stat(gzPath); err != nil {
		fmt.Fprintf(os.Stderr, "downloading %s\n", url)
		if err := curl(gzPath+".tmp", url); err != nil {
			return nil, err
		}
		if err := os.Rename(gzPath+".tmp", gzPath); err != nil {
			return nil, err
		}
	}
	return readImagesJSON(gzPath)
}

func readImagesJSON(path string) (map[string]*ct.Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defer gz.Close()
		r = gz
	}
	var images map[string]*ct.Artifact
	if err := json.NewDecoder(r).Decode(&images); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return images, nil
}

func fetchFlynnLayer(repo, version string, layer *ct.ImageLayer, cacheDir string) (string, error) {
	if layer == nil || layer.ID == "" {
		return "", fmt.Errorf("Flynn base layer is missing an id")
	}
	if dir := strings.TrimSpace(os.Getenv("FLYNN_LAYERS_DIR")); dir != "" {
		path := filepath.Join(dir, layer.ID+".squashfs")
		if err := verifyLayerFile(path, layer); err != nil {
			return "", fmt.Errorf("local layer %s: %w", path, err)
		}
		fmt.Fprintf(os.Stderr, "using local layer %s\n", path)
		return path, nil
	}
	dir := filepath.Join(cacheDir, "flynn", version, "layers")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, layer.ID+".squashfs")
	if st, err := os.Stat(path); err == nil {
		if layer.Length == 0 || st.Size() == layer.Length {
			if err := verifyLayerFile(path, layer); err == nil {
				return path, nil
			}
			_ = os.Remove(path)
		} else {
			_ = os.Remove(path)
		}
	}
	url := fmt.Sprintf(githubDownloadFmt, repo, version, layer.ID)
	fmt.Fprintf(os.Stderr, "downloading %s (%d bytes)\n", url, layer.Length)
	tmp := path + ".tmp"
	if err := curl(tmp, url); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	if err := verifyLayerFile(path, layer); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func verifyLayerFile(path string, layer *ct.ImageLayer) error {
	got, err := hashLayer(path)
	if err != nil {
		return err
	}
	if layer.Length > 0 && got.Length != layer.Length {
		return fmt.Errorf("layer %s size %d, want %d", layer.ID, got.Length, layer.Length)
	}
	want := layer.Hashes["sha512_256"]
	if want == "" {
		return fmt.Errorf("layer %s missing sha512_256", layer.ID)
	}
	if got.Hashes["sha512_256"] != want {
		return fmt.Errorf("layer %s sha512_256 %s, want %s", layer.ID, got.Hashes["sha512_256"], want)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
