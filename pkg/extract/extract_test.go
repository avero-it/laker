package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	return buf.Bytes()
}

// minimalPDF builds a one-page PDF showing text with a standard font.
func minimalPDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func TestFormats(t *testing.T) {
	x := New()
	defer x.Close()
	ctx := context.Background()

	xl := excelize.NewFile()
	xl.SetCellValue("Sheet1", "A1", "Customer")
	xl.SetCellValue("Sheet1", "B1", "Amount")
	xl.SetCellValue("Sheet1", "A2", "Smith")
	var xlsx bytes.Buffer
	xl.Write(&xlsx)

	cases := map[string]struct {
		data []byte
		want []string
	}{
		"lease.docx": {zipOf(t, map[string]string{
			"word/document.xml": `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>Lease</w:t></w:r><w:r><w:t xml:space="preserve"> agreement</w:t></w:r></w:p><w:p><w:r><w:t>Monthly rent</w:t></w:r></w:p></w:body></w:document>`,
		}), []string{"Lease agreement\nMonthly rent"}},
		"deck.pptx": {zipOf(t, map[string]string{
			"ppt/slides/slide10.xml": `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:t>Tenth</a:t></a:p></p:sld>`,
			"ppt/slides/slide2.xml":  `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:t>Second</a:t></a:p></p:sld>`,
		}), []string{"Second\nTenth"}},
		"nota.odt": {zipOf(t, map[string]string{
			"content.xml": `<office:document-content xmlns:office="o" xmlns:text="t"><office:body><text:h>Title</text:h><text:p>Body</text:p></office:body></office:document-content>`,
		}), []string{"Title\nBody"}},
		"accounts.xlsx": {xlsx.Bytes(), []string{"Customer Amount", "Smith"}},
		"page.html":     {[]byte(`<html><head><style>x{}</style><title>T</title></head><body><p>Hello <b>world</b></p><script>bad()</script></body></html>`), []string{"Hello world"}},
		"latin1.txt":    {[]byte("Caf\xe9 ready"), []string{"Café ready"}},
		"invoice.pdf":   {minimalPDF("Invoice number 42"), []string{"Invoice number 42"}},
	}
	for name, c := range cases {
		got, err := x.Text(ctx, name, c.data)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: got %q, want it to contain %q", name, got, w)
			}
		}
		if strings.Contains(got, "bad()") || strings.Contains(got, "x{}") {
			t.Errorf("%s: script or style text kept: %q", name, got)
		}
	}

	if _, err := x.Text(ctx, "photo.jpg", []byte{1}); err != ErrUnsupported {
		t.Errorf("jpg: err = %v", err)
	}
	if _, err := x.Text(ctx, "broken.docx", []byte("not a zip")); err == nil {
		t.Error("broken docx: no error")
	}
	if !Supports("A.PDF") || Supports("a.jpg") {
		t.Error("Supports")
	}
}
