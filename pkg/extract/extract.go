// Package extract pulls the plain text out of documents, in Go only: PDF
// through PDFium compiled to WebAssembly (run by wazero, no cgo), the Office
// and OpenDocument formats from their zipped XML, HTML, and plain text.
// Scanned PDFs have no text layer and give no text: there is no OCR.
package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/xuri/excelize/v2"
	"golang.org/x/net/html"
	"golang.org/x/text/encoding/charmap"
)

// MaxText caps the text kept per document.
const MaxText = 1 << 20

// ErrUnsupported is returned for a format without an extractor.
var ErrUnsupported = errors.New("ERR770: unsupported format")

var plainExt = map[string]bool{
	"txt": true, "md": true, "csv": true, "tsv": true, "json": true, "xml": true,
	"log": true, "yaml": true, "yml": true, "ini": true, "eml": false,
}

// Extractor is safe for concurrent use. The PDF engine starts on the first
// PDF (it takes a moment) and is released by Close.
type Extractor struct {
	once sync.Once
	pool pdfium.Pool
	pdf  pdfium.Pdfium
	err  error
	mu   sync.Mutex // one PDF at a time on the single instance
}

func New() *Extractor { return &Extractor{} }

// Ext is the lowercase extension of name, without the dot.
func Ext(name string) string { return strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".") }

// Supports tells whether name has a format with an extractor.
func Supports(name string) bool {
	switch e := Ext(name); e {
	case "pdf", "docx", "pptx", "xlsx", "odt", "ods", "odp", "html", "htm":
		return true
	default:
		return plainExt[e]
	}
}

// Text returns the text of the document b named name.
func (x *Extractor) Text(ctx context.Context, name string, b []byte) (string, error) {
	var text string
	var err error
	switch e := Ext(name); {
	case e == "pdf":
		text, err = x.pdfText(ctx, b)
	case e == "docx":
		text, err = zipXML(b, func(n string) bool { return n == "word/document.xml" }, "p")
	case e == "pptx":
		text, err = zipXML(b, func(n string) bool {
			return strings.HasPrefix(n, "ppt/slides/slide") && strings.HasSuffix(n, ".xml")
		}, "p")
	case e == "odt" || e == "ods" || e == "odp":
		text, err = zipXML(b, func(n string) bool { return n == "content.xml" }, "p", "h")
	case e == "xlsx":
		text, err = xlsxText(b)
	case e == "html" || e == "htm":
		text = HTML(b)
	case plainExt[e]:
		text = plain(b)
	default:
		return "", ErrUnsupported
	}
	if err != nil {
		return "", err
	}
	return clean(text), nil
}

func (x *Extractor) Close() {
	if x.pdf != nil {
		x.pdf.Close()
	}
	if x.pool != nil {
		x.pool.Close()
	}
}

func (x *Extractor) pdfText(ctx context.Context, b []byte) (string, error) {
	x.once.Do(func() {
		x.pool, x.err = webassembly.Init(webassembly.Config{MinIdle: 1, MaxIdle: 1, MaxTotal: 1})
		if x.err == nil {
			x.pdf, x.err = x.pool.GetInstance(time.Minute)
		}
	})
	if x.err != nil {
		return "", fmt.Errorf("ERR771: cannot start the PDF engine: %w", x.err)
	}
	x.mu.Lock()
	defer x.mu.Unlock()

	doc, err := x.pdf.OpenDocument(&requests.OpenDocument{File: &b})
	if err != nil {
		return "", fmt.Errorf("ERR772: cannot open the PDF: %w", err)
	}
	defer x.pdf.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})

	count, err := x.pdf.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return "", fmt.Errorf("ERR773: %w", err)
	}
	var sb strings.Builder
	for i := 0; i < count.PageCount && sb.Len() < MaxText; i++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		page, err := x.pdf.GetPageText(&requests.GetPageText{Page: requests.Page{
			ByIndex: &requests.PageByIndex{Document: doc.Document, Index: i},
		}})
		if err != nil {
			return "", fmt.Errorf("ERR774: page %d: %w", i+1, err)
		}
		sb.WriteString(page.Text)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// zipXML concatenates the text of the XML parts of a zip selected by want,
// in natural order (slide2 before slide10), with a line break after each
// element whose local name is in breaks.
func zipXML(b []byte, want func(string) bool, breaks ...string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", fmt.Errorf("ERR775: not a valid zip document: %w", err)
	}
	var files []*zip.File
	for _, f := range zr.File {
		if want(f.Name) {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return naturalLess(files[i].Name, files[j].Name) })

	var sb strings.Builder
	for _, f := range files {
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("ERR776: %s: %w", f.Name, err)
		}
		err = xmlText(io.LimitReader(rc, 64<<20), &sb, breaks)
		rc.Close()
		if err != nil {
			return "", fmt.Errorf("ERR777: %s: %w", f.Name, err)
		}
		if sb.Len() > MaxText {
			break
		}
	}
	return sb.String(), nil
}

func xmlText(r io.Reader, sb *strings.Builder, breaks []string) error {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			for _, b := range breaks {
				if t.Name.Local == b {
					sb.WriteString("\n")
				}
			}
		case xml.StartElement:
			if t.Name.Local == "tab" || t.Name.Local == "br" {
				sb.WriteString(" ")
			}
		}
	}
}

func xlsxText(b []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("ERR778: not a valid xlsx: %w", err)
	}
	defer f.Close()
	var sb strings.Builder
	for _, sheet := range f.GetSheetList() {
		rows, err := f.GetRows(sheet)
		if err != nil {
			return "", fmt.Errorf("ERR779: sheet %s: %w", sheet, err)
		}
		sb.WriteString(sheet + "\n")
		for _, row := range rows {
			sb.WriteString(strings.Join(row, "\t"))
			sb.WriteString("\n")
			if sb.Len() > MaxText {
				return sb.String(), nil
			}
		}
	}
	return sb.String(), nil
}

// HTML keeps the visible text of an HTML document.
func HTML(b []byte) string {
	z := html.NewTokenizer(bytes.NewReader(b))
	var sb strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return sb.String()
		case html.StartTagToken:
			if name, _ := z.TagName(); string(name) == "script" || string(name) == "style" {
				skip++
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style":
				if skip > 0 {
					skip--
				}
			case "p", "div", "br", "tr", "li", "h1", "h2", "h3", "h4", "title":
				sb.WriteString("\n")
			}
		case html.TextToken:
			if skip == 0 {
				sb.Write(z.Text())
				sb.WriteString(" ")
			}
		}
	}
}

// plain reads text as UTF-8, or as Latin-1 when it is not valid UTF-8.
func plain(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if d, err := charmap.ISO8859_1.NewDecoder().Bytes(b); err == nil {
		return string(d)
	}
	return strings.ToValidUTF8(string(b), "")
}

// clean collapses the spaces of each line, drops empty lines and caps the size.
func clean(s string) string {
	var sb strings.Builder
	for _, line := range strings.Split(strings.ToValidUTF8(s, ""), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			sb.WriteString(strings.Join(f, " "))
			sb.WriteString("\n")
		}
		if sb.Len() >= MaxText {
			break
		}
	}
	out := strings.TrimSpace(sb.String())
	if len(out) > MaxText {
		out = strings.ToValidUTF8(out[:MaxText], "")
	}
	return out
}

// naturalLess orders "slide2.xml" before "slide10.xml".
func naturalLess(a, b string) bool {
	na, pa := trailingNumber(a)
	nb, pb := trailingNumber(b)
	if pa == pb && na >= 0 && nb >= 0 {
		return na < nb
	}
	return a < b
}

func trailingNumber(s string) (int, string) {
	s = strings.TrimSuffix(s, path.Ext(s))
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(s[i:])
	if err != nil {
		return -1, s
	}
	return n, s[:i]
}
