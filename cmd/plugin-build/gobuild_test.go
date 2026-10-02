package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverlayDestMapsBinToUsrBin(t *testing.T) {
	root := "/tmp/flynn-plugin-upper"
	cases := []struct {
		dest, want string
	}{
		{"/bin/start-flynn-postgres", filepath.Join(root, "usr", "bin", "start-flynn-postgres")},
		{"bin/flynn-postgres", filepath.Join(root, "usr", "bin", "flynn-postgres")},
		{"/usr/bin/flynn-postgres-api", filepath.Join(root, "usr", "bin", "flynn-postgres-api")},
		{"/sbin/start", filepath.Join(root, "sbin", "start")},
	}
	for _, c := range cases {
		got, err := overlayDest(root, c.dest)
		if err != nil {
			t.Fatalf("%s: %v", c.dest, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %s want %s", c.dest, got, c.want)
		}
	}
}

func TestOverlayDestRejectsUnsafePaths(t *testing.T) {
	for _, dest := range []string{"/", ".", "..", "../etc/passwd", "/..", "foo/../../etc/passwd", "/usr/../../.."} {
		if _, err := overlayDest("/tmp/upper", dest); err == nil {
			t.Fatalf("expected error for %q", dest)
		}
	}
}

func TestImageDestsIncludesEntrypointAndCopy(t *testing.T) {
	p := &pluginManifest{
		Build: pluginBuild{
			Entrypoint: []string{"/bin/start-flynn-postgres"},
			Go:         map[string]string{"./cmd/flynn-postgres": "/bin/flynn-postgres"},
			Copy:       map[string]string{"start.sh": "/bin/start-flynn-postgres"},
		},
	}
	got := imageDests(p)
	joined := strings.Join(got, " ")
	for _, need := range []string{"/bin/start-flynn-postgres", "/bin/flynn-postgres"} {
		found := false
		for _, d := range got {
			if d == need {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %s in %s", need, joined)
		}
	}
}

func TestSquashfsHasFileAcceptsUsrMerge(t *testing.T) {
	usrMerge := "squashfs-root/usr/bin/start-flynn-postgres\nsquashfs-root/usr/bin/flynn-postgres\n"
	if !squashfsHasFile(usrMerge, "/bin/start-flynn-postgres") {
		t.Fatal("usr-merged /bin/start-flynn-postgres should match usr/bin in the delta")
	}
	if squashfsHasFile(usrMerge, "/bin/bash") {
		t.Fatal("missing paths must not match")
	}
	plain := "squashfs-root/bin/start-flynn-postgres\n"
	if !squashfsHasFile(plain, "/bin/start-flynn-postgres") {
		t.Fatal("non-usr-merged /bin/start-flynn-postgres should match bin/ in the delta")
	}
}

func TestElfGoarchFromHeader(t *testing.T) {
	amd64 := make([]byte, 20)
	copy(amd64, []byte{0x7f, 'E', 'L', 'F'})
	amd64[18] = 62 // EM_X86_64
	got, err := elfGoarchFromHeader(amd64)
	if err != nil || got != "amd64" {
		t.Fatalf("amd64: got %s %v", got, err)
	}
	arm64 := make([]byte, 20)
	copy(arm64, []byte{0x7f, 'E', 'L', 'F'})
	arm64[18] = 183 // EM_AARCH64
	got, err = elfGoarchFromHeader(arm64)
	if err != nil || got != "arm64" {
		t.Fatalf("arm64: got %s %v", got, err)
	}
}

func TestSupportedGoarch(t *testing.T) {
	if !supportedGoarch("amd64") || !supportedGoarch("arm64") || supportedGoarch("riscv64") {
		t.Fatal("supported GOARCH should be amd64 and arm64")
	}
}

func TestGoBuildEnvPersistsCache(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("FLYNN_PLUGIN_BUILD_CACHE", cache)
	t.Setenv("GOCACHE", "")
	env := goBuildEnv("arm64")
	want := filepath.Join(cache, "go-build")
	found := false
	for _, e := range env {
		if e == "GOCACHE="+want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("GOCACHE missing: %v", env)
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("cache dir: %v", err)
	}
}
