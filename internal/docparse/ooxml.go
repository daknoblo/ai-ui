package docparse

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
)

// maxOOXMLPartBytes caps the uncompressed size of a single part inside an OOXML
// archive. A .docx/.xlsx/.pptx is a ZIP, so a few hundred kilobytes on disk can
// expand into gigabytes in memory ("zip bomb"). Both the declared size and the
// actual read are bounded, because the declared size in the ZIP header cannot
// be trusted.
const maxOOXMLPartBytes = 64 << 20 // 64 MiB

// maxOOXMLParts bounds how many parts of an archive are read. Presentations and
// workbooks are processed part by part, so a crafted archive with a huge number
// of tiny entries is stopped here.
const maxOOXMLParts = 2000

var errOOXMLLimit = errors.New("OOXML resource limit exceeded")

// The budgets are shared by every part and decoder in one upload, including
// metadata and repeated relationship targets. Work also counts text copies,
// which bounds amplification through nested tables and shared strings.
type ooxmlLimits struct {
	partBytes, totalBytes, workBytes int64
	parts, tokens, depth, textBytes  int
}

func defaultOOXMLLimits() ooxmlLimits {
	return ooxmlLimits{
		partBytes: maxOOXMLPartBytes, totalBytes: 128 << 20, workBytes: 256 << 20,
		parts: maxOOXMLParts, tokens: 2_000_000, depth: 256, textBytes: maxTextBytes,
	}
}

type ooxmlBudget struct {
	limits        ooxmlLimits
	bytes, work   int64
	parts, tokens int
}

func (b *ooxmlBudget) spendWork(n int) error {
	if int64(n) > b.limits.workBytes-b.work {
		return fmt.Errorf("%w: text processing", errOOXMLLimit)
	}
	b.work += int64(n)
	return nil
}

type ooxmlArchive struct {
	*zip.Reader
	budget *ooxmlBudget
	files  map[string]*zip.File
}

// openOOXML opens an upload as a ZIP archive.
func openOOXML(data []byte, kind string) (*ooxmlArchive, error) {
	return openOOXMLWithLimits(data, kind, defaultOOXMLLimits())
}

func openOOXMLWithLimits(data []byte, kind string, limits ooxmlLimits) (*ooxmlArchive, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("read %s (zip): %w", kind, err)
	}
	if len(zr.File) > limits.parts {
		return nil, fmt.Errorf("%w: archive entry count", errOOXMLLimit)
	}
	a := &ooxmlArchive{Reader: zr, budget: &ooxmlBudget{limits: limits}, files: make(map[string]*zip.File, len(zr.File))}
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		if !validPartName(name) {
			return nil, fmt.Errorf("invalid OOXML part name %q", f.Name)
		}
		if _, exists := a.files[f.Name]; exists {
			return nil, fmt.Errorf("duplicate OOXML part %q", f.Name)
		}
		a.files[f.Name] = f
	}
	return a, nil
}

// readPart checks both the declared and actual inflated sizes.
func (a *ooxmlArchive) readPart(f *zip.File) ([]byte, error) {
	b := a.budget
	if b.parts >= b.limits.parts {
		return nil, fmt.Errorf("%w: parts read", errOOXMLLimit)
	}
	b.parts++
	limit := min(b.limits.partBytes, b.limits.totalBytes-b.bytes, b.limits.workBytes-b.work)
	if limit < 0 || f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("%w: inflated size of %s", errOOXMLLimit, f.Name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }() // Read-only ZIP entry; there is nothing to flush.

	// Read one byte beyond the limit so an understated header is detected.
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	b.bytes += int64(len(data))
	b.work += int64(len(data))
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: inflated size of %s", errOOXMLLimit, f.Name)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// findPart returns the entry with the given name.
func findPart(a *ooxmlArchive, name string) *zip.File {
	return a.files[name]
}

// partsWithPrefix returns the entries below a directory whose name ends in
// suffix. Natural filename order is only a fallback for minimal archives
// without the document's ordering metadata.
func partsWithPrefix(zr *ooxmlArchive, prefix, suffix string) []*zip.File {
	var out []*zip.File
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, prefix) && strings.HasSuffix(f.Name, suffix) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return naturalLess(out[i].Name, out[j].Name)
	})
	return out
}

type ooxmlDecoder struct {
	*xml.Decoder
	budget *ooxmlBudget
	depth  int
}

func newOOXMLDecoder(raw []byte, budget *ooxmlBudget) *ooxmlDecoder {
	return &ooxmlDecoder{Decoder: xml.NewDecoder(bytes.NewReader(raw)), budget: budget}
}

func (d *ooxmlDecoder) Token() (xml.Token, error) {
	tok, err := d.Decoder.Token()
	if err != nil {
		return nil, err
	}
	if d.budget.tokens >= d.budget.limits.tokens {
		return nil, fmt.Errorf("%w: XML token count", errOOXMLLimit)
	}
	d.budget.tokens++
	switch tok.(type) {
	case xml.StartElement:
		d.depth++
		if d.depth > d.budget.limits.depth {
			return nil, fmt.Errorf("%w: XML nesting", errOOXMLLimit)
		}
	case xml.EndElement:
		d.depth--
	}
	return tok, nil
}

type ooxmlText struct {
	strings.Builder
	budget *ooxmlBudget
	err    error
}

func (b *ooxmlText) write(s string) {
	if b.err != nil {
		return
	}
	if len(s) > b.budget.limits.textBytes-b.Len() {
		b.err = fmt.Errorf("%w: extracted text size", errOOXMLLimit)
		return
	}
	if b.err = b.budget.spendWork(len(s)); b.err == nil {
		_, _ = b.WriteString(s) // strings.Builder writes cannot fail.
	}
}

type ooxmlRelationship struct {
	target, kind string
	external     bool
}

const (
	officeRelationships = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	strictRelationships = "http://purl.oclc.org/ooxml/officeDocument/relationships"
)

func relationshipKind(kind string) string {
	for _, namespace := range []string{officeRelationships, strictRelationships} {
		if suffix, ok := strings.CutPrefix(kind, namespace+"/"); ok {
			return suffix
		}
	}
	return ""
}

func validPartName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.HasPrefix(name, "../") &&
		!strings.HasPrefix(name, "/") && !strings.ContainsAny(name, "\\:\x00") && path.Clean(name) == name
}

// relationshipTarget resolves package-relative URI paths, never filesystem
// paths or network URLs. Parent segments are legal only within the package.
func relationshipTarget(source, target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || u.Path == "" ||
		strings.ContainsAny(u.Path, "\\:\x00") || strings.HasPrefix(u.Path, "//") {
		return "", fmt.Errorf("invalid OOXML relationship target %q", target)
	}
	base := path.Dir(source)
	if strings.HasPrefix(u.Path, "/") {
		base = ""
	}
	name := path.Join(base, strings.TrimPrefix(u.Path, "/"))
	if !validPartName(name) {
		return "", fmt.Errorf("OOXML relationship escapes package: %q", target)
	}
	return name, nil
}

func (a *ooxmlArchive) relationships(source string) (map[string]ooxmlRelationship, bool, error) {
	name := path.Join(path.Dir(source), "_rels", path.Base(source)+".rels")
	f := findPart(a, name)
	if f == nil {
		return nil, false, nil
	}
	raw, err := a.readPart(f)
	if err != nil {
		return nil, true, err
	}
	out := make(map[string]ooxmlRelationship)
	dec := newOOXMLDecoder(raw, a.budget)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, true, nil
		}
		if err != nil {
			return nil, true, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Relationship" {
			continue
		}
		var id, target, kind, mode string
		for _, attr := range start.Attr {
			switch attr.Name.Local {
			case "Id":
				id = attr.Value
			case "Target":
				target = attr.Value
			case "Type":
				kind = relationshipKind(attr.Value)
			case "TargetMode":
				mode = attr.Value
			}
		}
		if id == "" || target == "" || (mode != "" && mode != "Internal" && mode != "External") {
			return nil, true, fmt.Errorf("invalid OOXML relationship in %s", name)
		}
		if _, exists := out[id]; exists {
			return nil, true, fmt.Errorf("duplicate OOXML relationship %q", id)
		}
		rel := ooxmlRelationship{kind: kind, external: mode == "External"}
		if !rel.external {
			if rel.target, err = relationshipTarget(source, target); err != nil {
				return nil, true, err
			}
		}
		out[id] = rel
	}
}

func (a *ooxmlArchive) relatedPart(rel ooxmlRelationship, kind string) (*zip.File, error) {
	if rel.external || rel.kind != kind {
		return nil, fmt.Errorf("invalid internal OOXML %s relationship", kind)
	}
	f := findPart(a, rel.target)
	if f == nil {
		return nil, fmt.Errorf("OOXML relationship part %q not found", rel.target)
	}
	return f, nil
}

type ooxmlNamedPart struct {
	file *zip.File
	name string
}

func (a *ooxmlArchive) orderedParts(source, listName, entryName, kind, prefix string) ([]ooxmlNamedPart, bool, error) {
	rels, hasRels, err := a.relationships(source)
	if err != nil {
		return nil, false, err
	}
	var entries []struct{ name, id string }
	if f := findPart(a, source); f != nil {
		raw, err := a.readPart(f)
		if err != nil {
			return nil, false, err
		}
		dec := newOOXMLDecoder(raw, a.budget)
		listDepth := 0
		for {
			tok, err := dec.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, false, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == listName {
					listDepth = dec.depth
				}
				if t.Name.Local != entryName || listDepth == 0 || dec.depth != listDepth+1 {
					continue
				}
				if len(entries) >= a.budget.limits.parts {
					return nil, false, fmt.Errorf("%w: document part count", errOOXMLLimit)
				}
				var name, id string
				for _, attr := range t.Attr {
					if attr.Name.Local == "name" {
						name = attr.Value
					}
					if attr.Name.Local == "id" && (attr.Name.Space == officeRelationships ||
						attr.Name.Space == strictRelationships || attr.Name.Space == "r") {
						id = attr.Value
					}
				}
				entries = append(entries, struct{ name, id string }{name, id})
			case xml.EndElement:
				if dec.depth < listDepth {
					listDepth = 0
				}
			}
		}
		if len(entries) == 0 {
			return nil, false, errNoText
		}
	} else if hasRels {
		return nil, false, fmt.Errorf("OOXML ordering part %q not found", source)
	}

	hasIDs := false
	for _, entry := range entries {
		hasIDs = hasIDs || entry.id != ""
	}
	// Older minimal fixtures omit all relationship metadata. Only that case
	// may use filenames; broken real metadata must not silently reorder tabs.
	if !hasRels && !hasIDs {
		files := partsWithPrefix(a, prefix, ".xml")
		if len(entries) != 0 && (kind != "worksheet" || len(entries) != len(files)) {
			return nil, false, fmt.Errorf("incomplete OOXML ordering metadata")
		}
		out := make([]ooxmlNamedPart, 0, len(files))
		for i, f := range files {
			name := ""
			if i < len(entries) {
				name = entries[i].name
			}
			out = append(out, ooxmlNamedPart{f, name})
		}
		return out, true, nil
	}
	var out []ooxmlNamedPart
	seen := make(map[string]bool)
	for _, entry := range entries {
		rel, ok := rels[entry.id]
		if !ok || entry.id == "" {
			return nil, false, fmt.Errorf("OOXML %s relationship %q not found", kind, entry.id)
		}
		f, err := a.relatedPart(rel, kind)
		if err != nil {
			return nil, false, err
		}
		if seen[f.Name] {
			return nil, false, fmt.Errorf("duplicate OOXML ordered part %q", f.Name)
		}
		seen[f.Name] = true
		out = append(out, ooxmlNamedPart{f, entry.name})
	}
	return out, false, nil
}

// naturalLess orders names so that "slide2.xml" sorts before "slide10.xml".
func naturalLess(a, b string) bool {
	for len(a) > 0 && len(b) > 0 {
		if isDigit(a[0]) && isDigit(b[0]) {
			na, ra := leadingNumber(a)
			nb, rb := leadingNumber(b)
			if na != nb {
				return na < nb
			}
			a, b = ra, rb
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// leadingNumber splits off the number at the start of s. Overlong digit runs are
// clamped, which is fine because they only decide a sort order.
func leadingNumber(s string) (int, string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	n := 0
	for _, c := range []byte(s[:i]) {
		if n > 1<<28 {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n, s[i:]
}
