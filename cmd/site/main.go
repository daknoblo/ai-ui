// Command site generates the static documentation website that is published on
// GitHub Pages. Its sources are README.md and the screenshots produced by
// tools/screenshots, so the site follows the repository automatically:
//
//	go run ./cmd/site -out site
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

//go:embed assets
var assets embed.FS

// markdown renders the README. Raw HTML stays escaped, as it does in the
// application itself. Hard wraps are off on purpose: the README is wrapped at
// 80 columns, which must not turn into line breaks on the website.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// shot is one entry of the screenshot manifest.
type shot struct {
	ID      string `json:"id"`
	Lang    string `json:"lang"`
	File    string `json:"file"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Title   string `json:"title"`
	Caption string `json:"caption"`
}

// gallery groups the screenshots of one interface language.
type gallery struct {
	Lang  string
	Name  string
	Shots []shot
}

// heading is one entry of the table of contents.
type heading struct {
	ID    string
	Title string
}

// pageData is the model of both generated pages.
type pageData struct {
	Title       string
	Name        string
	Tagline     string
	Repo        string
	Description string
	ShotsDir    string
	Features    template.HTML
	Body        template.HTML
	Headings    []heading
	Galleries   []gallery
	Year        int
}

func main() {
	out := flag.String("out", "site", "output directory of the generated website")
	readme := flag.String("readme", "README.md", "Markdown source of the documentation")
	shots := flag.String("screenshots", "docs/screenshots", "directory holding the screenshots and their manifest")
	repo := flag.String("repo", "https://github.com/daknoblo/ai-ui", "repository URL")
	flag.Parse()

	if err := run(*out, *readme, *shots, *repo); err != nil {
		fmt.Fprintln(os.Stderr, "site:", err)
		os.Exit(1)
	}
}

func run(out, readme, shots, repo string) error {
	src, err := os.ReadFile(readme) //nolint:gosec // path comes from a flag of a build tool
	if err != nil {
		return err
	}

	body, headings, err := render(src)
	if err != nil {
		return err
	}
	features, err := renderSection(src, "Features")
	if err != nil {
		return err
	}
	galleries, err := loadGalleries(shots)
	if err != nil {
		return err
	}

	// The screenshots keep the path they have in the repository, so the image
	// links of the rendered README resolve on the website as well.
	shotsDir := filepath.ToSlash(shots)
	if filepath.IsAbs(shots) {
		shotsDir = "screenshots"
	}

	data := pageData{
		Name:        title(src),
		Tagline:     tagline(src),
		Repo:        repo,
		Description: tagline(src),
		ShotsDir:    shotsDir,
		Features:    features,
		Body:        body,
		Headings:    headings,
		Galleries:   galleries,
		Year:        time.Now().Year(),
	}

	tmpl, err := template.ParseFS(assets, "assets/*.html")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(out, "assets"), 0o750); err != nil {
		return err
	}
	for _, page := range []struct{ file, name, title string }{
		{"index.html", "index", data.Name},
		{"docs.html", "docs", "Documentation - " + data.Name},
	} {
		data.Title = page.title
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, page.name, data); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, page.file), buf.Bytes(), 0o600); err != nil {
			return err
		}
	}

	css, err := assets.ReadFile("assets/site.css")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "assets", "site.css"), css, 0o600); err != nil {
		return err
	}
	// Without this file GitHub Pages would run the output through Jekyll.
	if err := os.WriteFile(filepath.Join(out, ".nojekyll"), nil, 0o600); err != nil {
		return err
	}
	if err := copyTree(shots, filepath.Join(out, filepath.FromSlash(shotsDir))); err != nil {
		return err
	}

	fmt.Printf("site: wrote %s (%d screenshots)\n", out, countShots(galleries))
	return nil
}

// render converts the Markdown source and collects the second level headings
// for the table of contents.
func render(src []byte) (template.HTML, []heading, error) {
	doc := markdown.Parser().Parse(text.NewReader(src))

	var headings []heading
	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !entering || !ok || h.Level != 2 {
			return ast.WalkContinue, nil
		}
		attr, ok := h.AttributeString("id")
		if !ok {
			return ast.WalkContinue, nil
		}
		id, ok := attr.([]byte)
		if !ok {
			return ast.WalkContinue, nil
		}
		headings = append(headings, heading{ID: string(id), Title: nodeText(src, h)})
		return ast.WalkContinue, nil
	})
	if err != nil {
		return "", nil, err
	}

	var buf bytes.Buffer
	if err := markdown.Renderer().Render(&buf, src, doc); err != nil {
		return "", nil, err
	}
	//nolint:gosec // goldmark runs without WithUnsafe, so raw HTML is escaped
	return template.HTML(buf.String()), headings, nil
}

// nodeText collects the plain text of a node and its children.
func nodeText(src []byte, n ast.Node) string {
	var sb strings.Builder
	_ = ast.Walk(n, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := node.(type) {
		case *ast.Text:
			sb.Write(t.Segment.Value(src))
		case *ast.String:
			sb.Write(t.Value)
		}
		return ast.WalkContinue, nil
	})
	return sb.String()
}

// renderSection converts a single "## " section of the source, without its own
// heading. An unknown section yields empty output.
func renderSection(src []byte, name string) (template.HTML, error) {
	var section []string
	inFence, collecting := false, false
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "## ") {
			collecting = strings.TrimSpace(strings.TrimPrefix(line, "## ")) == name
			continue
		}
		if collecting {
			section = append(section, line)
		}
	}
	if len(section) == 0 {
		return "", nil
	}
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(strings.Join(section, "\n")), &buf); err != nil {
		return "", err
	}
	//nolint:gosec // goldmark runs without WithUnsafe, so raw HTML is escaped
	return template.HTML(buf.String()), nil
}

// title returns the first level one heading of the source.
func title(src []byte) string {
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return "Documentation"
}

// tagline returns the first paragraph that is neither a heading nor a badge
// list, joined into a single line.
func tagline(src []byte) string {
	var current []string
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(current) > 0 {
				return strings.Join(current, " ")
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "[!") {
			current = nil
			continue
		}
		current = append(current, trimmed)
	}
	return strings.Join(current, " ")
}

// languageNames maps the interface languages to their gallery heading.
var languageNames = map[string]string{"en": "English interface", "de": "German interface"}

// loadGalleries reads the screenshot manifest and groups it by language.
func loadGalleries(dir string) ([]gallery, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:gosec // path comes from a flag of a build tool
	if os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "site: no screenshot manifest, gallery stays empty")
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Shots []shot `json:"shots"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}

	var out []gallery
	for _, s := range manifest.Shots {
		idx := -1
		for i := range out {
			if out[i].Lang == s.Lang {
				idx = i
				break
			}
		}
		if idx < 0 {
			name, ok := languageNames[s.Lang]
			if !ok {
				name = strings.ToUpper(s.Lang)
			}
			out = append(out, gallery{Lang: s.Lang, Name: name})
			idx = len(out) - 1
		}
		out[idx].Shots = append(out[idx].Shots, s)
	}
	return out, nil
}

// countShots totals the screenshots of all galleries.
func countShots(galleries []gallery) int {
	total := 0
	for _, g := range galleries {
		total += len(g.Shots)
	}
	return total
}

// copyTree copies a directory recursively into the output.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, err := os.ReadFile(path) //nolint:gosec // walking the screenshot directory
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600) //nolint:gosec // target is built from the walked relative path
	})
}
