package docparse

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// tinyJPEG is a complete 1x1 baseline JPEG: SOI … EOI.
var tinyJPEG = func() []byte {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	jpeg = append(jpeg, bytes.Repeat([]byte{0x42}, minScanImageBytes)...)
	return append(jpeg, 0xFF, 0xD9)
}()

// buildScanPDF assembles a PDF whose pages carry nothing but an image, which is
// what a scanner or a phone camera app produces.
func buildScanPDF(images [][]byte) []byte {
	return buildScanPDFWithExtras(images, nil)
}

// buildScanPDFWithExtras additionally registers image streams that belong to no
// page - a thumbnail, an embedded attachment or a superseded revision.
func buildScanPDFWithExtras(images, extras [][]byte) []byte {
	var objs [][]byte
	objs = append(objs, []byte("<< /Type /Catalog /Pages 2 0 R >>"))

	kids := make([]string, 0, len(images))
	for i := range images {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+i*3))
	}
	objs = append(objs, fmt.Appendf(nil, "<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.Join(kids, " "), len(images)))

	for i, img := range images {
		content, xobj := 4+i*3, 5+i*3
		objs = append(objs, fmt.Appendf(nil,
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>", xobj, content))

		draw := []byte("q 612 0 0 792 0 0 cm /Im0 Do Q")
		objs = append(objs, fmt.Appendf(nil, "<< /Length %d >>\nstream\n%s\nendstream", len(draw), draw))
		objs = append(objs, fmt.Appendf(nil,
			"<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray "+
				"/BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", len(img), img))
	}

	for _, extra := range extras {
		objs = append(objs, fmt.Appendf(nil,
			"<< /Type /XObject /Subtype /Image /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream",
			len(extra), extra))
	}

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, obj := range objs {
		offsets[i+1] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objs)+1, xref)
	return pdf.Bytes()
}

// TestScanPDFReportsNeedsOCR checks the signal the ingestion depends on: a PDF
// without a text layer but with page images has to be distinguishable from one
// that is simply broken.
func TestScanPDFReportsNeedsOCR(t *testing.T) {
	_, err := Extract("scan.pdf", "application/pdf", buildScanPDF([][]byte{tinyJPEG}))
	if !errors.Is(err, ErrNeedsOCR) {
		t.Fatalf("Extract error = %v, want ErrNeedsOCR", err)
	}
}

// TestTextPDFDoesNotAskForOCR is the counterpart: a PDF that has text must not
// trigger a transcription, which would cost a request per page for nothing.
func TestTextPDFDoesNotAskForOCR(t *testing.T) {
	got, err := Extract("report.pdf", "application/pdf", buildPDF([]string{"Readable"}))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(got, "Readable") {
		t.Errorf("extract = %q", got)
	}
}

// TestPDFWithoutImagesKeepsTheOldAdvice makes sure a PDF that has neither text
// nor images still explains what to do instead of asking for a transcription.
func TestPDFWithoutImagesKeepsTheOldAdvice(t *testing.T) {
	_, err := Extract("empty.pdf", "application/pdf", buildPDF(nil))
	if errors.Is(err, ErrNeedsOCR) {
		t.Fatal("a PDF without page images must not ask for OCR")
	}
	if err == nil || !strings.Contains(err.Error(), "OCR") {
		t.Errorf("error = %v, want the OCR advice", err)
	}
}

// TestScanImagesKeepsPageOrder covers the mapping the transcription relies on:
// the images come back in file order, which is page order.
func TestScanImagesKeepsPageOrder(t *testing.T) {
	pages := make([][]byte, 3)
	for i := range pages {
		page := append([]byte{}, tinyJPEG...)
		// Make each page distinguishable without breaking the JPEG markers.
		page[8] = byte('A' + i)
		pages[i] = page
	}

	got := ScanImages(buildScanPDF(pages))
	if len(got) != 3 {
		t.Fatalf("got %d page images, want 3", len(got))
	}
	for i, img := range got {
		if img.MIME != "image/jpeg" {
			t.Errorf("page %d has MIME %q, want image/jpeg", i+1, img.MIME)
		}
		if img.Data[8] != byte('A'+i) {
			t.Errorf("page %d is out of order: marker %q", i+1, img.Data[8])
		}
	}
}

// TestScanImagesSkipsDecoration makes sure a logo or separator does not become
// a page of its own - every extra image is a request that costs money.
func TestScanImagesSkipsDecoration(t *testing.T) {
	logo := append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x01, 0x02}, 0xFF, 0xD9)
	got := ScanImages(buildScanPDF([][]byte{logo, tinyJPEG}))
	if len(got) != 1 {
		t.Fatalf("got %d images, want only the page-sized one", len(got))
	}
}

// TestScanImagesIgnoresTruncatedJPEG rejects a stream that starts like a JPEG
// but never ends, which no model could read.
func TestScanImagesIgnoresTruncatedJPEG(t *testing.T) {
	truncated := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x42}, minScanImageBytes)...)
	if got := ScanImages(buildScanPDF([][]byte{truncated})); len(got) != 0 {
		t.Errorf("got %d images, want none for a truncated JPEG", len(got))
	}
}

// TestScanImagesBoundsByPageCount is the regression test for a cost fan-out:
// the raw byte scan also finds thumbnails, embedded attachments and the objects
// an incremental update superseded. Each of those became a billable
// transcription request, so a single file could turn into thousands of them.
// The number of pages the document declares is the honest upper bound.
func TestScanImagesBoundsByPageCount(t *testing.T) {
	decoys := make([][]byte, 50)
	for i := range decoys {
		decoy := append([]byte{}, tinyJPEG...)
		decoy[9] = byte(i)
		decoys[i] = decoy
	}

	got := ScanImages(buildScanPDFWithExtras([][]byte{tinyJPEG}, decoys))
	if len(got) != 1 {
		t.Errorf("got %d images for a one-page document, want exactly 1", len(got))
	}
}

// TestScanImagesKeepsTheLargestCandidates checks which image survives the
// bound: a scanned page is by far the biggest stream on it, so a thumbnail must
// never displace the page itself.
func TestScanImagesKeepsTheLargestCandidates(t *testing.T) {
	thumb := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x11}, minScanImageBytes+16)...)
	thumb = append(thumb, 0xFF, 0xD9)
	page := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x22}, minScanImageBytes*40)...)
	page = append(page, 0xFF, 0xD9)

	// The thumbnail sits on the page, the real scan is registered after it.
	got := ScanImages(buildScanPDFWithExtras([][]byte{thumb}, [][]byte{page}))
	if len(got) != 1 {
		t.Fatalf("got %d images, want 1", len(got))
	}
	if len(got[0].Data) != len(page) {
		t.Errorf("the thumbnail was kept instead of the page: %d bytes", len(got[0].Data))
	}
}

// TestScanImagesRefusesUnreadableStructure covers the case where the page count
// cannot be determined: without it a page cannot be told apart from a
// thumbnail, so nothing may be sent to the model.
func TestScanImagesRefusesUnreadableStructure(t *testing.T) {
	broken := append(buildScanPDF([][]byte{tinyJPEG}), []byte("\ngarbage after the trailer")...)
	broken = bytes.ReplaceAll(broken, []byte("startxref"), []byte("startxrEf"))

	if got := ScanImages(broken); len(got) != 0 {
		t.Errorf("got %d images from a PDF with an unreadable structure, want none", len(got))
	}
}

// TestScanImagesIgnoresTruncatedPNG mirrors the JPEG check: a stream that only
// starts with the PNG magic is not a picture a model could read.
func TestScanImagesIgnoresTruncatedPNG(t *testing.T) {
	truncated := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x42}, minScanImageBytes)...)
	if got := ScanImages(buildScanPDF([][]byte{truncated})); len(got) != 0 {
		t.Errorf("got %d images, want none for a PNG without IEND", len(got))
	}
}

// TestScanImagesOnNonPDF makes sure the byte scan does not invent images.
func TestScanImagesOnNonPDF(t *testing.T) {
	if got := ScanImages([]byte("this is just text, with the word stream in it")); len(got) != 0 {
		t.Errorf("got %d images from plain text", len(got))
	}
}
