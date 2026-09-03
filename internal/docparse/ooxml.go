package docparse

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
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

// openOOXML opens an upload as a ZIP archive.
func openOOXML(data []byte, kind string) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("read %s (zip): %w", kind, err)
	}
	return zr, nil
}

// readPart reads a single archive entry with both limits applied.
func readPart(f *zip.File) ([]byte, error) {
	if f.UncompressedSize64 > maxOOXMLPartBytes {
		return nil, fmt.Errorf("%s is too large (%d bytes)", f.Name, f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	// Read one byte beyond the limit so an understated header is detected.
	data, err := io.ReadAll(io.LimitReader(rc, maxOOXMLPartBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxOOXMLPartBytes {
		return nil, fmt.Errorf("%s exceeds the size limit", f.Name)
	}
	return data, nil
}

// findPart returns the entry with the given name.
func findPart(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// partsWithPrefix returns the entries below a directory whose name ends in
// suffix, sorted by name so slides and sheets keep their document order.
func partsWithPrefix(zr *zip.Reader, prefix, suffix string) []*zip.File {
	var out []*zip.File
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, prefix) && strings.HasSuffix(f.Name, suffix) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return naturalLess(out[i].Name, out[j].Name)
	})
	if len(out) > maxOOXMLParts {
		out = out[:maxOOXMLParts]
	}
	return out
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
