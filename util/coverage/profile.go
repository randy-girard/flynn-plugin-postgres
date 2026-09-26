package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	modulePath  = "github.com/randy-girard/flynn-plugin-postgres/"
	reportTitle = "flynn-plugin-postgres unit tests"
)

type Block struct {
	StartLine int
	StartCol  int
	EndLine   int
	EndCol    int
	NumStmt   int
	Count     int
}

type FileCover struct {
	Path    string // repo-relative, e.g. pkg/plugin/manifest.go
	AbsPath string
	Blocks  []Block
	Covered int
	Total   int
}

func (f FileCover) Percent() float64 {
	if f.Total == 0 {
		return 0
	}
	return 100 * float64(f.Covered) / float64(f.Total)
}

type PackageCover struct {
	Path    string
	Files   []FileCover
	Covered int
	Total   int
}

func (p PackageCover) Percent() float64 {
	if p.Total == 0 {
		return 0
	}
	return 100 * float64(p.Covered) / float64(p.Total)
}

type AreaCover struct {
	Name     string
	Packages []PackageCover
	Covered  int
	Total    int
}

func (a AreaCover) Percent() float64 {
	if a.Total == 0 {
		return 0
	}
	return 100 * float64(a.Covered) / float64(a.Total)
}

type Report struct {
	Files   []FileCover
	Areas   []AreaCover
	Covered int
	Total   int
}

func (r Report) Percent() float64 {
	if r.Total == 0 {
		return 0
	}
	return 100 * float64(r.Covered) / float64(r.Total)
}

func parseProfile(path, srcRoot string) ([]FileCover, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	byFile := map[string]*FileCover{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if !strings.HasPrefix(line, "mode:") {
				return nil, fmt.Errorf("%s: first line must be mode:", path)
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		file, blk, err := parseBlockLine(line)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		rel := strings.TrimPrefix(file, modulePath)
		fc, ok := byFile[rel]
		if !ok {
			fc = &FileCover{Path: rel, AbsPath: joinSrc(srcRoot, rel)}
			byFile[rel] = fc
		}
		fc.Blocks = append(fc.Blocks, blk)
		fc.Total += blk.NumStmt
		if blk.Count > 0 {
			fc.Covered += blk.NumStmt
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	out := make([]FileCover, 0, len(byFile))
	for _, fc := range byFile {
		out = append(out, *fc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func joinSrc(root, rel string) string {
	if root == "" {
		return rel
	}
	return strings.TrimRight(root, "/") + "/" + rel
}

func parseBlockLine(line string) (string, Block, error) {
	// name.go:line.col,line.col numStmt count
	colon := strings.LastIndex(line, ":")
	if colon < 0 {
		return "", Block{}, fmt.Errorf("invalid cover line %q", line)
	}
	name := line[:colon]
	rest := line[colon+1:]
	var b Block
	n, err := fmt.Sscanf(rest, "%d.%d,%d.%d %d %d", &b.StartLine, &b.StartCol, &b.EndLine, &b.EndCol, &b.NumStmt, &b.Count)
	if err != nil || n != 6 {
		// Sscanf can fail on extra spaces; split instead.
		parts := strings.Fields(rest)
		if len(parts) != 3 {
			return "", Block{}, fmt.Errorf("invalid cover line %q", line)
		}
		span := strings.Split(parts[0], ",")
		if len(span) != 2 {
			return "", Block{}, fmt.Errorf("invalid cover line %q", line)
		}
		if err := parseDotPair(span[0], &b.StartLine, &b.StartCol); err != nil {
			return "", Block{}, err
		}
		if err := parseDotPair(span[1], &b.EndLine, &b.EndCol); err != nil {
			return "", Block{}, err
		}
		b.NumStmt, err = strconv.Atoi(parts[1])
		if err != nil {
			return "", Block{}, err
		}
		b.Count, err = strconv.Atoi(parts[2])
		if err != nil {
			return "", Block{}, err
		}
	}
	return name, b, nil
}

func parseDotPair(s string, a, b *int) error {
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return fmt.Errorf("invalid position %q", s)
	}
	var err error
	*a, err = strconv.Atoi(s[:i])
	if err != nil {
		return err
	}
	*b, err = strconv.Atoi(s[i+1:])
	return err
}

func buildReport(files []FileCover) Report {
	type pkgKey struct{ area, pkg string }
	pkgs := map[pkgKey]*PackageCover{}
	r := Report{Files: files}
	for _, f := range files {
		r.Covered += f.Covered
		r.Total += f.Total
		area, pkg := splitAreaPackage(f.Path)
		k := pkgKey{area, pkg}
		p, ok := pkgs[k]
		if !ok {
			p = &PackageCover{Path: pkg}
			pkgs[k] = p
		}
		p.Files = append(p.Files, f)
		p.Covered += f.Covered
		p.Total += f.Total
	}

	areas := map[string]*AreaCover{}
	for k, p := range pkgs {
		sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
		a, ok := areas[k.area]
		if !ok {
			a = &AreaCover{Name: k.area}
			areas[k.area] = a
		}
		a.Packages = append(a.Packages, *p)
		a.Covered += p.Covered
		a.Total += p.Total
	}
	for _, a := range areas {
		sort.Slice(a.Packages, func(i, j int) bool { return a.Packages[i].Path < a.Packages[j].Path })
		r.Areas = append(r.Areas, *a)
	}
	sort.Slice(r.Areas, func(i, j int) bool { return r.Areas[i].Name < r.Areas[j].Name })
	return r
}

func splitAreaPackage(rel string) (area, pkg string) {
	rel = strings.TrimPrefix(rel, "/")
	dir := rel
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		dir = rel[:i]
	} else {
		return "root", "."
	}
	area = dir
	if i := strings.IndexByte(dir, '/'); i >= 0 {
		area = dir[:i]
	}
	return area, dir
}
