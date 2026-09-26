package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	ct "github.com/randy-girard/flynn/controller/types"
)

func TestPickBaseLayersPrefersUbuntuNoble(t *testing.T) {
	noble := layer("noble", 10)
	other := layer("other", 20)
	images := map[string]*ct.Artifact{
		"ubuntu-noble": artifact(noble, other),
		"blobstore":    artifact(layer("blob-os", 11), layer("blob-app", 12)),
	}
	got, donor, err := pickBaseLayers(images, "blobstore")
	if err != nil {
		t.Fatal(err)
	}
	if donor != "ubuntu-noble" || len(got) != 2 || got[0].ID != "noble" || got[1].ID != "other" {
		t.Fatalf("got %#v donor=%s, want ubuntu-noble's layers", ids(got), donor)
	}
}

func TestPickBaseLayersUsesFirstLayerOfDonor(t *testing.T) {
	osLayer := layer("os", 319729664)
	pkg := layer("pkg", 100)
	bin := layer("bin", 200)
	images := map[string]*ct.Artifact{
		"blobstore": artifact(osLayer, pkg, bin),
		"redis":     artifact(osLayer, layer("redis-pkg", 3)),
	}
	got, donor, err := pickBaseLayers(images, "blobstore")
	if err != nil {
		t.Fatal(err)
	}
	if donor != "blobstore" || len(got) != 1 || got[0].ID != "os" || got[0].Length != 319729664 {
		t.Fatalf("got %#v donor=%s, want first blobstore layer (ubuntu-noble)", ids(got), donor)
	}
}

func TestPickBaseLayersSkipsBusyboxBlobstore(t *testing.T) {
	busybox := layer("busybox", 1<<20)
	noble := layer("noble-os", 199<<20)
	images := map[string]*ct.Artifact{
		"blobstore": artifact(busybox, layer("blob-app", 40<<20)),
		"postgres":  artifact(noble, layer("pg-pkg", 300<<20)),
	}
	got, donor, err := pickBaseLayers(images, "blobstore")
	if err != nil {
		t.Fatal(err)
	}
	if donor != "postgres" || len(got) != 1 || got[0].ID != "noble-os" {
		t.Fatalf("got %#v donor=%s, want postgres ubuntu-noble layer", ids(got), donor)
	}
}

func TestPickBaseLayersMissing(t *testing.T) {
	_, _, err := pickBaseLayers(map[string]*ct.Artifact{"redis": artifact(layer("os", 1))}, "nope")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestVerifyLayerFileAcceptsFlynnBuildID(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/layer.squashfs"
	if err := os.WriteFile(path, []byte("squashfs-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := hashLayer(path)
	if err != nil {
		t.Fatal(err)
	}
	layer := &ct.ImageLayer{
		ID:     "flynn-build-input-id",
		Length: got.Length,
		Hashes: map[string]string{"sha512_256": got.Hashes["sha512_256"]},
	}
	if err := verifyLayerFile(path, layer); err != nil {
		t.Fatal(err)
	}
}

func TestLatestPublishedRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/randy-girard/flynn/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(githubRelease{TagName: "v20260911.0"})
	}))
	defer srv.Close()
	origBase, origHTTP := githubAPIBase, githubHTTP
	githubAPIBase, githubHTTP = srv.URL, srv.Client()
	defer func() {
		githubAPIBase, githubHTTP = origBase, origHTTP
	}()

	tag, err := latestPublishedRelease("randy-girard/flynn")
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v20260911.0" {
		t.Fatalf("tag %q", tag)
	}
}

func TestReadImagesJSONAndLocalLayer(t *testing.T) {
	dir := t.TempDir()
	layerPath := dir + "/os.squashfs"
	if err := os.WriteFile(layerPath, []byte("squashfs-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := hashLayer(layerPath)
	if err != nil {
		t.Fatal(err)
	}
	images := map[string]*ct.Artifact{
		"ubuntu-noble": artifact(&ct.ImageLayer{
			ID:     "os",
			Type:   ct.ImageLayerTypeSquashfs,
			Length: got.Length,
			Hashes: map[string]string{"sha512_256": got.Hashes["sha512_256"]},
		}),
	}
	raw, err := json.Marshal(images)
	if err != nil {
		t.Fatal(err)
	}
	jsonPath := dir + "/images.json"
	if err := os.WriteFile(jsonPath, raw, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLYNN_IMAGES_JSON", jsonPath)
	t.Setenv("FLYNN_LAYERS_DIR", dir)
	loaded, err := loadFlynnImages("unused", "local", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layers, donor, err := pickBaseLayers(loaded, "ubuntu-noble")
	if err != nil {
		t.Fatal(err)
	}
	if donor != "ubuntu-noble" || len(layers) != 1 {
		t.Fatalf("donor=%s layers=%d", donor, len(layers))
	}
	path, err := fetchFlynnLayer("unused", "local", layers[0], t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if path != layerPath {
		t.Fatalf("path %s", path)
	}
}

func TestResolveFlynnBaseUsesLocalImages(t *testing.T) {
	dir := t.TempDir()
	layerPath := dir + "/os.squashfs"
	if err := os.WriteFile(layerPath, []byte("squashfs-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := hashLayer(layerPath)
	if err != nil {
		t.Fatal(err)
	}
	images := map[string]*ct.Artifact{
		"ubuntu-noble": artifact(&ct.ImageLayer{
			ID:     "os",
			Type:   ct.ImageLayerTypeSquashfs,
			Length: got.Length,
			Hashes: map[string]string{"sha512_256": got.Hashes["sha512_256"]},
		}),
	}
	raw, err := json.Marshal(images)
	if err != nil {
		t.Fatal(err)
	}
	jsonPath := dir + "/images.json"
	if err := os.WriteFile(jsonPath, raw, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLYNN_IMAGES_JSON", jsonPath)
	t.Setenv("FLYNN_LAYERS_DIR", dir)
	base, err := resolveFlynnBase("", "latest", "ubuntu-noble", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if base.Version != "local" || base.Image != "ubuntu-noble" {
		t.Fatalf("version=%s image=%s", base.Version, base.Image)
	}
	if len(base.Layers) != 1 || base.Layers[0].ID != "os" {
		t.Fatalf("layers=%v", ids(base.Layers))
	}
	if len(base.Files) != 1 || base.Files[0] != layerPath {
		t.Fatalf("files=%v", base.Files)
	}
}

func TestFetchFlynnLayerRejectsMismatchedLocalLayer(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/os.squashfs"
	if err := os.WriteFile(path, []byte("wrong-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLYNN_LAYERS_DIR", dir)
	_, err := fetchFlynnLayer("unused", "local", &ct.ImageLayer{
		ID:     "os",
		Length: 12,
		Hashes: map[string]string{"sha512_256": "deadbeef"},
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "local layer") {
		t.Fatalf("got %v", err)
	}
}

func TestFetchFlynnLayerMissingLocalLayer(t *testing.T) {
	t.Setenv("FLYNN_LAYERS_DIR", t.TempDir())
	_, err := fetchFlynnLayer("unused", "local", &ct.ImageLayer{
		ID:     "missing",
		Length: 1,
		Hashes: map[string]string{"sha512_256": "deadbeef"},
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "local layer") {
		t.Fatalf("got %v", err)
	}
}

func layer(id string, n int64) *ct.ImageLayer {
	return &ct.ImageLayer{
		ID:     id,
		Type:   ct.ImageLayerTypeSquashfs,
		Length: n,
		Hashes: map[string]string{"sha512_256": id},
	}
}

func artifact(layers ...*ct.ImageLayer) *ct.Artifact {
	man := &ct.ImageManifest{
		Type: ct.ImageManifestTypeV1,
		Rootfs: []*ct.ImageRootfs{{
			Platform: ct.DefaultImagePlatform,
			Layers:   layers,
		}},
	}
	return &ct.Artifact{
		Type:        ct.ArtifactTypeFlynn,
		RawManifest: man.RawManifest(),
	}
}

func ids(layers []*ct.ImageLayer) []string {
	out := make([]string, len(layers))
	for i, l := range layers {
		out[i] = l.ID
	}
	return out
}
