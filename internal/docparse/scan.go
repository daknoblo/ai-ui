package docparse

import (
	"bytes"
	"errors"
	"log/slog"
	"sort"

	"github.com/ledongthuc/pdf"
)

// ErrNeedsOCR reports a PDF that carries no text layer but does carry page
// images - a scan. The caller can hand those images to a model that sees
// instead of rejecting the upload.
var ErrNeedsOCR = errors.New("pdf has no text layer")

// Image is a raster image lifted out of a document.
type Image struct {
	MIME string
	Data []byte
}

// minScanImageBytes filters the decorative artwork a generator leaves behind -
// separators, logos, spacers. A scanned page never compresses that far, so
// anything below this is not worth a transcription request.
const minScanImageBytes = 1 << 10

// candidate is an image stream before it has been matched to a page.
type candidate struct {
	order int // position in the file, which is reading order
	image Image
}

// ScanImages returns the page images embedded in a PDF, in file order - which
// is page order for the scanners and phone apps that produce these documents.
//
// The images are read straight from the raw bytes rather than through the PDF
// reader, because its stream decoder panics on DCTDecode instead of handing the
// JPEG back. That is not a loss: a DCTDecode stream *is* a JPEG file, so it can
// be passed to the model unchanged.
//
// Bypassing the cross-reference table has a cost: the scan also finds streams
// that are not current page images - thumbnails, embedded attachments and the
// superseded objects an incremental update leaves behind. The result is
// therefore reduced to the number of pages the document actually has, keeping
// the largest streams, because a scanned page is by far the biggest image on
// it. Without that bound one crafted file could turn into tens of thousands of
// billable transcription requests.
func ScanImages(data []byte) []Image {
	found := rawImageStreams(data)
	if len(found) == 0 {
		return nil
	}

	pages := pageCount(data)
	if pages <= 0 {
		// Without the document structure a page cannot be told apart from a
		// thumbnail, an embedded attachment or a revision that was superseded.
		// Guessing would mean sending arbitrary streams to a third party and
		// paying for each of them, so nothing is returned. The transcription
		// path is only reached for a PDF the reader accepted, so this does not
		// cost a readable scan anything.
		slog.Warn("cannot determine the page count, skipping the page images",
			"streams", len(found))
		return nil
	}

	if len(found) > pages {
		slog.Debug("more image streams than pages, keeping the largest",
			"streams", len(found), "pages", pages)
		// Largest first, ties by position so the choice is deterministic.
		sort.SliceStable(found, func(i, j int) bool {
			if len(found[i].image.Data) != len(found[j].image.Data) {
				return len(found[i].image.Data) > len(found[j].image.Data)
			}
			return found[i].order < found[j].order
		})
		found = found[:pages]
		// Restore reading order for the ones that survived.
		sort.Slice(found, func(i, j int) bool { return found[i].order < found[j].order })
	}

	out := make([]Image, 0, len(found))
	for _, c := range found {
		out = append(out, c.image)
	}
	return out
}

// rawImageStreams collects every stream whose payload is a complete image.
func rawImageStreams(data []byte) []candidate {
	var (
		out  []candidate
		rest = data
		n    int
	)
	for {
		start := bytes.Index(rest, []byte("stream"))
		if start < 0 {
			return out
		}
		body := rest[start+len("stream"):]
		// The keyword is followed by CR, LF or CRLF before the payload begins.
		for len(body) > 0 && (body[0] == '\r' || body[0] == '\n') {
			body = body[1:]
		}

		end := bytes.Index(body, []byte("endstream"))
		if end < 0 {
			return out
		}
		payload := bytes.TrimRight(body[:end], "\r\n")
		if mime := rasterMIME(payload); mime != "" && len(payload) >= minScanImageBytes {
			out = append(out, candidate{order: n, image: Image{MIME: mime, Data: payload}})
			n++
		}

		// Advance past the whole stream; "endstream" itself contains "stream",
		// so a shorter step would rescan the same bytes out of alignment.
		rest = body[end+len("endstream"):]
	}
}

// pageCount reports how many pages a PDF declares, or zero when that cannot be
// determined. The reader is the same third-party parser used for the text, so
// the panic guard applies here as well.
func pageCount(data []byte) (pages int) {
	defer func() {
		if r := recover(); r != nil {
			pages = 0
		}
	}()

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0
	}
	return min(r.NumPage(), maxPDFPages)
}

// rasterMIME identifies the image formats that can be embedded in a PDF and
// sent to a model unchanged. Both formats are checked at their start *and* end,
// so a truncated stream - which no model could read - is not passed on.
func rasterMIME(payload []byte) string {
	switch {
	case bytes.HasPrefix(payload, []byte{0xFF, 0xD8, 0xFF}) &&
		bytes.HasSuffix(payload, []byte{0xFF, 0xD9}): // JPEG: SOI … EOI
		return "image/jpeg"
	case bytes.HasPrefix(payload, []byte("\x89PNG\r\n\x1a\n")) &&
		bytes.HasSuffix(payload, []byte("IEND\xae\x42\x60\x82")): // PNG: magic … IEND
		return "image/png"
	}
	// A JPXDecode stream holds JPEG 2000, which no chat model reads.
	return ""
}
