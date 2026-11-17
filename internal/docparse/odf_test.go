package docparse

import (
	"strings"
	"testing"
)

func TestOpenDocumentExtraction(t *testing.T) {
	prefix := `<office:document-content xmlns:office="` + odfOffice + `" xmlns:text="` + odfText + `" xmlns:table="` + odfTable + `"><office:body>`
	for _, tc := range []struct {
		extension, content, expected string
	}{
		{"odt", `<office:text><text:p>First <text:span>paragraph</text:span></text:p><office:annotation><text:p>Hidden</text:p></office:annotation><text:p>Second</text:p></office:text>`, "First paragraph\nSecond"},
		{"odp", `<office:presentation><text:h>Title</text:h><text:p>Slide text</text:p></office:presentation>`, "Title\nSlide text"},
		{"ods", `<office:spreadsheet><table:table table:name="Totals"><table:table-row table:number-rows-repeated="2"><table:table-cell><text:p>Net</text:p></table:table-cell><table:table-cell office:value="1234.56"/></table:table-row><table:table-row table:number-rows-repeated="1048576"/></table:table></office:spreadsheet>`, "Totals\nNet\t1234.56\nNet\t1234.56"},
	} {
		t.Run(tc.extension, func(t *testing.T) {
			data := buildZIP(t, map[string]string{"content.xml": prefix + tc.content + `</office:body></office:document-content>`,
				"mimetype": "application/vnd.oasis.opendocument.text"})
			for _, name := range []string{"file." + tc.extension, "unknown"} {
				got, err := Extract(name, "", data)
				if err != nil || got != tc.expected {
					t.Fatalf("got=%q err=%v", got, err)
				}
			}
		})
	}
}

func TestOpenDocumentRejectsExpansionAndMalformedXML(t *testing.T) {
	for _, body := range []string{
		`<table:table-row table:number-rows-repeated="999999"><table:table-cell><text:p>Amplify</text:p></table:table-cell></table:table-row>`,
		`<text:p>invalid`,
		`<text:p><text:s text:c="-1"/></text:p>`,
	} {
		data := buildZIP(t, map[string]string{"content.xml": `<office:document-content xmlns:office="` + odfOffice + `" xmlns:text="` + odfText + `" xmlns:table="` + odfTable + `"><office:body>` + body + `</office:body></office:document-content>`})
		if _, err := Extract("file.odt", "", data); err == nil {
			t.Fatal("invalid or amplifying OpenDocument accepted")
		}
	}
	if !strings.Contains(UploadAccept(), ".ods") {
		t.Fatal("OpenDocument missing from picker")
	}
}
