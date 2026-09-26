package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
)

func writeHTML(r Report, outDir string) error {
	if err := os.MkdirAll(filepath.Join(outDir, "files"), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "style.css"), []byte(reportCSS), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "index.html"), []byte(renderIndex(r)), 0644); err != nil {
		return err
	}
	for _, f := range r.Files {
		dest := filepath.Join(outDir, "files", f.Path+".html")
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		body, err := renderFile(f)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, []byte(body), 0644); err != nil {
			return err
		}
	}
	return nil
}

func renderIndex(r Report) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	fmt.Fprintf(&b, "<title>%s coverage</title>\n<link rel=\"stylesheet\" href=\"style.css\">\n</head>\n<body>\n<main>\n", html.EscapeString(reportTitle))
	fmt.Fprintf(&b, "<p class=\"kicker\">%s</p>\n<div class=\"hero\">\n  <div>\n    <h1>Coverage report</h1>\n    <p class=\"muted\">", html.EscapeString(reportTitle))
	fmt.Fprintf(&b, "%d of %d statements covered</p>\n  </div>\n  <div class=\"hero-pct %s\">%.1f%%</div>\n</div>\n", r.Covered, r.Total, pctClass(r.Percent()), r.Percent())
	fmt.Fprintf(&b, "<div class=\"bar\"><span style=\"width:%.1f%%\"></span></div>\n", r.Percent())
	b.WriteString("<nav class=\"toc\"><strong>Package areas</strong>")
	for _, a := range r.Areas {
		fmt.Fprintf(&b, " <a href=\"#%s\">%s <span class=\"%s\">%.1f%%</span></a>", html.EscapeString(a.Name), html.EscapeString(a.Name), pctClass(a.Percent()), a.Percent())
	}
	b.WriteString("</nav>\n<section>\n<h2>Overall by package area</h2>\n<table>\n")
	b.WriteString("<thead><tr><th>Area</th><th class=\"num\">Coverage</th><th>Statements</th><th></th></tr></thead>\n<tbody>\n")
	for _, a := range r.Areas {
		fmt.Fprintf(&b, "<tr><td><a href=\"#%s\">%s</a></td><td class=\"num %s\">%.1f%%</td><td class=\"num muted\">%d / %d</td><td>%s</td></tr>\n",
			html.EscapeString(a.Name), html.EscapeString(a.Name), pctClass(a.Percent()), a.Percent(), a.Covered, a.Total, miniBar(a.Percent()))
	}
	b.WriteString("</tbody></table>\n</section>\n")
	for _, a := range r.Areas {
		fmt.Fprintf(&b, "<section id=\"%s\">\n<h2>%s <span class=\"h-pct %s\">%.1f%%</span></h2>\n", html.EscapeString(a.Name), html.EscapeString(a.Name), pctClass(a.Percent()), a.Percent())
		fmt.Fprintf(&b, "<p class=\"muted\">%d of %d statements · %d package", a.Covered, a.Total, len(a.Packages))
		if len(a.Packages) != 1 {
			b.WriteString("s")
		}
		b.WriteString("</p>\n<table>\n<thead><tr><th>Package</th><th class=\"num\">Coverage</th><th>Statements</th><th></th></tr></thead>\n<tbody>\n")
		for _, p := range a.Packages {
			fmt.Fprintf(&b, "<tr><td>%s</td><td class=\"num %s\">%.1f%%</td><td class=\"num muted\">%d / %d</td><td>%s</td></tr>\n",
				html.EscapeString(p.Path), pctClass(p.Percent()), p.Percent(), p.Covered, p.Total, miniBar(p.Percent()))
		}
		b.WriteString("</tbody></table>\n<table class=\"files\">\n<thead><tr><th>File</th><th class=\"num\">Coverage</th><th>Statements</th><th></th></tr></thead>\n<tbody>\n")
		for _, p := range a.Packages {
			for _, f := range p.Files {
				href := "files/" + f.Path + ".html"
				fmt.Fprintf(&b, "<tr><td><a href=\"%s\">%s</a></td><td class=\"num %s\">%.1f%%</td><td class=\"num muted\">%d / %d</td><td>%s</td></tr>\n",
					html.EscapeString(href), html.EscapeString(f.Path), pctClass(f.Percent()), f.Percent(), f.Covered, f.Total, miniBar(f.Percent()))
			}
		}
		b.WriteString("</tbody></table>\n</section>\n")
	}
	b.WriteString("</main>\n</body>\n</html>\n")
	return b.String()
}

func renderFile(f FileCover) (string, error) {
	src, err := os.ReadFile(f.AbsPath)
	if err != nil {
		src = []byte("// source not found: " + f.AbsPath + "\n")
	}
	indexHref := indexRel(f.Path)
	cssHref := strings.Repeat("../", pathDepth(f.Path)+1) + "style.css"

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n<title>")
	b.WriteString(html.EscapeString(f.Path))
	b.WriteString(" — coverage</title>\n<link rel=\"stylesheet\" href=\"")
	b.WriteString(html.EscapeString(cssHref))
	b.WriteString("\">\n</head>\n<body>\n<main class=\"file\">\n<p class=\"kicker\"><a href=\"")
	b.WriteString(html.EscapeString(indexHref))
	b.WriteString("\">← All files</a></p>\n<div class=\"hero\">\n  <div>\n    <h1>")
	b.WriteString(html.EscapeString(f.Path))
	b.WriteString("</h1>\n    <p class=\"muted\">")
	fmt.Fprintf(&b, "%d of %d statements covered</p>\n  </div>\n  <div class=\"hero-pct %s\">%.1f%%</div>\n</div>\n", f.Covered, f.Total, pctClass(f.Percent()), f.Percent())
	fmt.Fprintf(&b, "<div class=\"bar\"><span style=\"width:%.1f%%\"></span></div>\n", f.Percent())
	b.WriteString("<p class=\"legend\"><span class=\"cov\">covered</span> <span class=\"uncov\">uncovered</span> <span class=\"partial\">partial</span></p>\n<div class=\"src\"><table>\n")

	lines := strings.Split(string(src), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		n := i + 1
		fmt.Fprintf(&b, "<tr class=\"%s\"><td class=\"ln\">%d</td><td class=\"code\">%s</td></tr>\n", lineClass(n, f.Blocks), n, html.EscapeString(line))
	}
	b.WriteString("</table></div>\n</main>\n</body>\n</html>\n")
	return b.String(), nil
}

func lineClass(line int, blocks []Block) string {
	seenUnc, seenCov := false, false
	for _, b := range blocks {
		if line < b.StartLine || line > b.EndLine {
			continue
		}
		if b.Count > 0 {
			seenCov = true
		} else {
			seenUnc = true
		}
	}
	switch {
	case seenCov && seenUnc:
		return "partial"
	case seenCov:
		return "cov"
	case seenUnc:
		return "uncov"
	default:
		return "none"
	}
}

func pctClass(p float64) string {
	switch {
	case p >= 80:
		return "good"
	case p >= 50:
		return "mid"
	default:
		return "bad"
	}
}

func miniBar(p float64) string {
	return fmt.Sprintf("<span class=\"minibar\"><span class=\"%s\" style=\"width:%.1f%%\"></span></span>", pctClass(p), p)
}

func pathDepth(rel string) int {
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "." {
		return 0
	}
	return strings.Count(dir, "/") + 1
}

func indexRel(rel string) string {
	return strings.Repeat("../", pathDepth(rel)+1) + "index.html"
}
