package main

import (
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/pkg/squashfs"
)

func buildPluginLayers(repo, outDir string, plugin *pluginManifest, base *resolvedBase) (string, *ct.ImageLayer, string, error) {
	if len(base.Files) != 1 {
		return "", nil, "", fmt.Errorf("plugin-build currently overlays a single ubuntu-noble layer (got %d)", len(base.Files))
	}

	root, err := os.MkdirTemp("", "flynn-plugin-overlay-")
	if err != nil {
		return "", nil, "", err
	}
	defer func() {
		_ = sudoCommand("umount", root).Run()
		_ = sudoCommand("rm", "-rf", root).Run()
	}()

	// Overlay cannot nest on Docker's overlay rootfs ("wrong fs type").
	// Keep lower/upper/work on tmpfs.
	if err := sudoCommand("mount", "-t", "tmpfs", "-o", "size=6G", "tmpfs", root).Run(); err != nil {
		if err := sudoCommand("mount", "-t", "tmpfs", "tmpfs", root).Run(); err != nil {
			return "", nil, "", fmt.Errorf("tmpfs for overlay workspace: %w", err)
		}
	}

	lower := filepath.Join(root, "lower")
	upper := filepath.Join(root, "upper")
	work := filepath.Join(root, "work")
	merged := filepath.Join(root, "merged")
	for _, d := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return "", nil, "", err
		}
	}

	fmt.Fprintf(os.Stderr, "==> unsquashfs Flynn ubuntu-noble\n")
	if err := sudoCommand("unsquashfs", "-f", "-d", lower, base.Files[0]).Run(); err != nil {
		return "", nil, "", fmt.Errorf("unsquashfs ubuntu-noble: %w", err)
	}
	if err := requireChrootBash(lower); err != nil {
		return "", nil, "", err
	}
	goarch, err := detectRootfsGoarch(lower)
	if err != nil {
		return "", nil, "", err
	}
	fmt.Fprintf(os.Stderr, "==> OS layer arch %s\n", goarch)

	fmt.Fprintf(os.Stderr, "==> overlay packages + binaries\n")
	if err := overlayChroot(repo, lower, upper, work, merged, plugin); err != nil {
		return "", nil, "", err
	}
	if err := installBinaries(repo, upper, plugin, goarch); err != nil {
		return "", nil, "", err
	}

	tmpLayer := filepath.Join(outDir, "layer.squashfs.tmp")
	_ = os.Remove(tmpLayer)
	fmt.Fprintf(os.Stderr, "==> mksquashfs plugin delta (zstd/%s)\n", squashfs.CompressionLevel)
	if err := mksquashfs(upper, tmpLayer); err != nil {
		return "", nil, "", err
	}
	if err := requireSquashfsFiles(tmpLayer, imageDests(plugin)); err != nil {
		return "", nil, "", err
	}

	layer, err := hashLayer(tmpLayer)
	if err != nil {
		return "", nil, "", err
	}
	layerPath := filepath.Join(outDir, "layers", layer.ID+".squashfs")
	if err := os.Rename(tmpLayer, layerPath); err != nil {
		if err := sudoCommand("mv", "-f", tmpLayer, layerPath).Run(); err != nil {
			return "", nil, "", err
		}
		_ = sudoCommand("chown", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), layerPath).Run()
	}
	return layerPath, layer, goarch, nil
}

func requireChrootBash(root string) error {
	for _, rel := range []string{"bin/bash", "usr/bin/bash"} {
		if sudoCommand("test", "-e", filepath.Join(root, rel)).Run() == nil {
			return nil
		}
	}
	return fmt.Errorf("Flynn base layer has no /bin/bash after unsquashfs (busybox or a delta, not ubuntu-noble). Set build.base.image to ubuntu-noble or postgres")
}

func overlayChroot(repo, lower, upper, work, merged string, plugin *pluginManifest) error {
	script := merged + ".overlay.sh"
	defer os.Remove(script)

	setup := plugin.Build.Setup
	packages := plugin.Build.Packages
	body := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
LOWER=%q
UPPER=%q
WORK=%q
MERGED=%q
REPO=%q
SETUP=%q
PACKAGES=%q

cleanup() {
  umount "${MERGED}/proc" 2>/dev/null || true
  for d in null zero random urandom tty; do
    umount "${MERGED}/dev/${d}" 2>/dev/null || true
  done
  umount "${MERGED}" 2>/dev/null || true
}
trap cleanup EXIT

mount -t overlay overlay -o "lowerdir=${LOWER},upperdir=${UPPER},workdir=${WORK},metacopy=off" "${MERGED}"

rm -f "${MERGED}/etc/resolv.conf"
cp /etc/resolv.conf "${MERGED}/etc/resolv.conf"

mkdir -p "${MERGED}/dev" "${MERGED}/proc" "${MERGED}/tmp"
chmod 1777 "${MERGED}/tmp" || true
mount -t proc proc "${MERGED}/proc"
for d in null zero random urandom tty; do
  src="/dev/${d}"
  dest="${MERGED}/dev/${d}"
  rm -rf "${dest}"
  if [[ -c "${src}" ]]; then
    touch "${dest}"
    mount --bind "${src}" "${dest}"
  fi
done

if [[ -n "${SETUP}" ]]; then
  cp "${REPO}/${SETUP}" "${MERGED}/tmp/ubuntu-setup.sh"
  chroot "${MERGED}" bash -e /tmp/ubuntu-setup.sh
fi

if [[ -n "${PACKAGES}" ]]; then
  mkdir -p "${MERGED}/tmp/plugin-img"
  pkgdir="$(dirname "${REPO}/${PACKAGES}")"
  cp -a "${pkgdir}/." "${MERGED}/tmp/plugin-img/"
  chroot "${MERGED}" bash -e "/tmp/plugin-img/$(basename "${PACKAGES}")"
fi

rm -rf "${MERGED}/tmp/plugin-img" "${MERGED}/tmp/ubuntu-setup.sh"
: > "${MERGED}/etc/resolv.conf"
rm -rf "${MERGED}/var/cache/apt/archives"/* "${MERGED}/var/lib/apt/lists"/* 2>/dev/null || true
`, lower, upper, work, merged, repo, setup, packages)
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		return err
	}
	if err := sudoCommand("bash", script).Run(); err != nil {
		return fmt.Errorf("overlay chroot: %w", err)
	}
	return nil
}

func requireSquashfsFiles(squashfsPath string, dests []string) error {
	if len(dests) == 0 {
		return fmt.Errorf("plugin image has no entrypoint/go/copy destinations to verify")
	}
	cmd := exec.Command("unsquashfs", "-l", squashfsPath)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("unsquashfs -l %s: %w", squashfsPath, err)
	}
	listing := string(out)
	for _, dest := range dests {
		if !squashfsHasFile(listing, dest) {
			return fmt.Errorf("plugin delta squashfs missing %s (usr-merged /bin must be stored as usr/bin so /bin/bash stays visible)", dest)
		}
	}
	return nil
}

func squashfsHasFile(listing, dest string) bool {
	rel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(dest)), "/")
	needles := []string{"squashfs-root/" + rel}
	if rel == "bin" || strings.HasPrefix(rel, "bin/") {
		needles = append(needles, "squashfs-root/usr/"+rel)
	}
	for _, needle := range needles {
		if strings.Contains(listing, needle) {
			return true
		}
	}
	return false
}

func mksquashfs(src, dst string) error {
	args := squashfs.Args(src, dst, "-mem", squashfs.MemLimit)
	args = append(args, "-e")
	args = append(args, squashfs.DefaultExcludes()...)
	if err := sudoCommand("mksquashfs", args...).Run(); err != nil {
		return err
	}
	_ = sudoCommand("chown", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), dst).Run()
	return nil
}

func hashLayer(path string) (*ct.ImageLayer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	h := sha512.New512_256()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return &ct.ImageLayer{
		ID:     sum,
		Type:   ct.ImageLayerTypeSquashfs,
		Length: st.Size(),
		Hashes: map[string]string{"sha512_256": sum},
	}, nil
}

func sudoCommand(name string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command(name, args...)
	} else {
		cmd = exec.Command("sudo", append([]string{"-E", name}, args...)...)
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd
}

func curl(dst, url string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	cmd := exec.Command("curl", "-fSL",
		"--retry", "5",
		"--retry-delay", "5",
		"--retry-connrefused",
		"--retry-all-errors",
		"-o", dst,
		url,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("curl %s: %w", url, err)
	}
	return nil
}

func cacheDir() (string, error) {
	if d := os.Getenv("FLYNN_PLUGIN_BUILD_CACHE"); d != "" {
		return d, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "flynn-plugin-build"), nil
}
