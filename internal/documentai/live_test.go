package documentai

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// Opt-in only: one billed two-page analysis of synthetic data. CLI credentials
// are the default; application credentials require explicit selection.
func TestLiveLayout(t *testing.T) {
	endpoint := os.Getenv("DOCUMENTAI_LIVE_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set DOCUMENTAI_LIVE_TEST_ENDPOINT to explicitly authorize a billed synthetic Layout test")
	}
	var credential azcore.TokenCredential
	var err error
	auth := os.Getenv("DOCUMENTAI_LIVE_TEST_AUTH")
	if auth != "" && auth != "cli" && auth != "application" {
		t.Fatal("DOCUMENTAI_LIVE_TEST_AUTH must be cli or application")
	}
	if auth == "application" {
		tenant, client, secret := os.Getenv("AZURE_TENANT_ID"), os.Getenv("AZURE_CLIENT_ID"), os.Getenv("AZURE_CLIENT_SECRET")
		if tenant == "" || client == "" || secret == "" {
			t.Fatal("application live test requires explicit Azure identity environment variables")
		}
		credential, err = azidentity.NewClientSecretCredential(tenant, client, secret, nil)
	} else {
		credential, err = azidentity.NewAzureCLICredential(&azidentity.AzureCLICredentialOptions{
			Subscription: os.Getenv("DOCUMENTAI_LIVE_TEST_SUBSCRIPTION"),
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(endpoint, "", credential)
	if err != nil {
		t.Fatal(err)
	}
	pages, err := client.Analyze(t.Context(), syntheticPDF(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || !strings.Contains(pages[0].Text, "1234") || !strings.Contains(pages[1].Text, "5678") {
		t.Fatalf("mixed text/Flate scan extraction failed: %+v", pages)
	}
	t.Log("Layout extracted both synthetic text and scanned pages and deleted the result")
}

func syntheticPDF(t *testing.T) []byte {
	t.Helper()
	const width, height = 600, 160
	rgb := bytes.Repeat([]byte{255}, width*height*3)
	digits := []string{
		"11111100001000011110000010000111110",
		"01111100001000011110100011000101110",
		"11111000010001000100010000100001000",
		"01110100011000101110100011000101110",
	}
	for i, digit := range digits {
		for cell, bit := range digit {
			if bit != '1' {
				continue
			}
			for y := range 12 {
				for x := range 12 {
					offset := ((30+(cell/5)*12+y)*width + 40 + i*100 + (cell%5)*12 + x) * 3
					clear(rgb[offset : offset+3])
				}
			}
		}
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(rgb); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	stream := func(dictionary string, data []byte) []byte {
		return fmt.Appendf(nil, "<< %s /Length %d >>\nstream\n%s\nendstream", dictionary, len(data), data)
	}
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 6 0 R >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /XObject << /Im0 8 0 R >> >> /Contents 7 0 R >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
		stream("", []byte("BT /F1 24 Tf 50 700 Td (Synthetic invoice total 1234 EUR) Tj ET")),
		stream("", []byte("q 600 0 0 160 6 600 cm /Im0 Do Q")),
		stream("/Type /XObject /Subtype /Image /Width 600 /Height 160 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode", compressed.Bytes()),
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return pdf.Bytes()
}
