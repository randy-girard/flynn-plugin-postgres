package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeELF(t *testing.T, path, goarch string) {
	t.Helper()
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F'})
	b[4] = 2 // ELFCLASS64
	b[5] = 1 // ELFDATA2LSB
	switch goarch {
	case "amd64":
		b[18] = 62 // EM_X86_64
	case "arm64":
		b[18] = 183 // EM_AARCH64
	default:
		t.Fatalf("writeFakeELF: unsupported %s", goarch)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0755); err != nil {
		t.Fatal(err)
	}
}

func TestElfGoarchFromHeaderErrors(t *testing.T) {
	if _, err := elfGoarchFromHeader([]byte{0x7f, 'E', 'L'}); err == nil {
		t.Fatal("short header must fail")
	}
	b := make([]byte, 20)
	copy(b, []byte{'n', 'o', 't', 'f'})
	if _, err := elfGoarchFromHeader(b); err == nil {
		t.Fatal("non-ELF must fail")
	}
	copy(b, []byte{0x7f, 'E', 'L', 'F'})
	b[18] = 3 // EM_386
	if _, err := elfGoarchFromHeader(b); err == nil {
		t.Fatal("unsupported machine must fail")
	}
}

func TestElfGoarchReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bash")
	writeFakeELF(t, path, "arm64")
	got, err := elfGoarch(path)
	if err != nil || got != "arm64" {
		t.Fatalf("got %s %v", got, err)
	}
}

func TestGoarchFromRootfsPrefersUsrBinBash(t *testing.T) {
	root := t.TempDir()
	writeFakeELF(t, filepath.Join(root, "usr", "bin", "bash"), "arm64")
	writeFakeELF(t, filepath.Join(root, "bin", "bash"), "amd64")
	got, err := goarchFromRootfs(root)
	if err != nil || got != "arm64" {
		t.Fatalf("got %s %v", got, err)
	}
}

func TestGoarchFromRootfsFallsBackToBinBash(t *testing.T) {
	root := t.TempDir()
	writeFakeELF(t, filepath.Join(root, "bin", "bash"), "amd64")
	got, err := goarchFromRootfs(root)
	if err != nil || got != "amd64" {
		t.Fatalf("got %s %v", got, err)
	}
}

func TestDetectRootfsGoarchUsesOSLayer(t *testing.T) {
	t.Setenv("PLUGIN_GOARCH", "")
	root := t.TempDir()
	writeFakeELF(t, filepath.Join(root, "usr", "bin", "bash"), "arm64")
	got, err := detectRootfsGoarch(root)
	if err != nil || got != "arm64" {
		t.Fatalf("got %s %v", got, err)
	}
}

func TestDetectRootfsGoarchEnvMustMatchLayer(t *testing.T) {
	root := t.TempDir()
	writeFakeELF(t, filepath.Join(root, "usr", "bin", "bash"), "arm64")
	t.Setenv("PLUGIN_GOARCH", "amd64")
	_, err := detectRootfsGoarch(root)
	if err == nil || !strings.Contains(err.Error(), "does not match Flynn OS layer") {
		t.Fatalf("got %v", err)
	}
}

func TestDetectRootfsGoarchEnvMatchingLayer(t *testing.T) {
	root := t.TempDir()
	writeFakeELF(t, filepath.Join(root, "usr", "bin", "bash"), "arm64")
	t.Setenv("PLUGIN_GOARCH", "arm64")
	got, err := detectRootfsGoarch(root)
	if err != nil || got != "arm64" {
		t.Fatalf("got %s %v", got, err)
	}
}

func TestDetectRootfsGoarchRejectsUnsupportedEnv(t *testing.T) {
	t.Setenv("PLUGIN_GOARCH", "riscv64")
	_, err := detectRootfsGoarch(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("got %v", err)
	}
}

func TestDetectRootfsGoarchEnvFillsInWhenBashMissing(t *testing.T) {
	t.Setenv("PLUGIN_GOARCH", "amd64")
	got, err := detectRootfsGoarch(t.TempDir())
	if err != nil || got != "amd64" {
		t.Fatalf("got %s %v", got, err)
	}
}

func TestGoarchFromRootfsMissingBash(t *testing.T) {
	_, err := goarchFromRootfs(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "cannot detect Flynn OS layer architecture") {
		t.Fatalf("got %v", err)
	}
}
