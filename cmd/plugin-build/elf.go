package main

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func supportedGoarch(arch string) bool {
	switch arch {
	case "amd64", "arm64":
		return true
	default:
		return false
	}
}

func elfGoarch(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hdr := make([]byte, 20)
	if _, err := f.Read(hdr); err != nil {
		return "", err
	}
	return elfGoarchFromHeader(hdr)
}

func elfGoarchFromHeader(b []byte) (string, error) {
	if len(b) < 20 {
		return "", fmt.Errorf("short ELF header")
	}
	if b[0] != 0x7f || b[1] != 'E' || b[2] != 'L' || b[3] != 'F' {
		return "", fmt.Errorf("not ELF")
	}
	machine := binary.LittleEndian.Uint16(b[18:20])
	switch elf.Machine(machine) {
	case elf.EM_X86_64:
		return "amd64", nil
	case elf.EM_AARCH64:
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported ELF machine %d", machine)
	}
}

func detectRootfsGoarch(root string) (string, error) {
	detected, detErr := goarchFromRootfs(root)
	if env := strings.TrimSpace(os.Getenv("PLUGIN_GOARCH")); env != "" {
		if !supportedGoarch(env) {
			return "", fmt.Errorf("PLUGIN_GOARCH=%s is not supported (amd64 or arm64)", env)
		}
		if detErr == nil && env != detected {
			return "", fmt.Errorf("PLUGIN_GOARCH=%s does not match Flynn OS layer (%s)", env, detected)
		}
		return env, nil
	}
	if detErr != nil {
		return "", detErr
	}
	return detected, nil
}

func goarchFromRootfs(root string) (string, error) {
	var last error
	for _, rel := range []string{"usr/bin/bash", "bin/bash"} {
		p := filepath.Join(root, rel)
		arch, err := elfGoarch(p)
		if err == nil {
			return arch, nil
		}
		last = err
		tmp, copyErr := copyReadable(p)
		if copyErr != nil {
			continue
		}
		arch, err = elfGoarch(tmp)
		_ = os.Remove(tmp)
		if err == nil {
			return arch, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("no /bin/bash")
	}
	return "", fmt.Errorf("cannot detect Flynn OS layer architecture from /bin/bash: %w (set PLUGIN_GOARCH=amd64 or arm64)", last)
}

func copyReadable(src string) (string, error) {
	dst, err := os.CreateTemp("", "flynn-plugin-elf-")
	if err != nil {
		return "", err
	}
	dst.Close()
	if err := sudoCommand("cp", "-f", src, dst.Name()).Run(); err != nil {
		_ = os.Remove(dst.Name())
		return "", err
	}
	if err := sudoCommand("chmod", "0644", dst.Name()).Run(); err != nil {
		_ = os.Remove(dst.Name())
		return "", err
	}
	return dst.Name(), nil
}
