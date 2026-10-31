package web

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"

	_ "github.com/emersion/go-message/charset" // decode non-UTF-8 parts
	"github.com/emersion/go-message/mail"
	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

// maxInlineImage caps a cid: image embedded in the HTML view as a data: URI.
const maxInlineImage = 2 << 20

type part struct {
	Index     int
	Name      string
	MIME      string
	Size      int64
	ContentID string
	Inline    bool
	data      []byte
}

type parsedMail struct {
	HTML  string // raw HTML body, if any
	Parts []part // attachments and inline parts, in order
}

// parseEML reads the HTML body and the attachments of a raw message.
func parseEML(raw []byte) (*parsedMail, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if mr == nil {
		return nil, fmt.Errorf("ERR400: cannot parse the message: %w", err)
	}
	defer mr.Close()

	pm := &parsedMail{}
	for i := 0; ; i++ {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return pm, nil // keep what was read
		}
		b, _ := io.ReadAll(io.LimitReader(p.Body, 64<<20))
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			cid := strings.Trim(h.Get("Content-Id"), "<>")
			switch {
			case ct == "text/html" && pm.HTML == "":
				pm.HTML = string(b)
			case strings.HasPrefix(ct, "image/") || cid != "":
				pm.Parts = append(pm.Parts, part{Index: i, Name: inlineName(h, ct, i), MIME: ct, Size: int64(len(b)), ContentID: cid, Inline: true, data: b})
			}
		case *mail.AttachmentHeader:
			ct, _, _ := h.ContentType()
			name, _ := h.Filename()
			if name == "" {
				name = fmt.Sprintf("allegato-%d", i)
			}
			cid := strings.Trim(h.Get("Content-Id"), "<>")
			disp, _, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
			// an image with a Content-ID and no explicit "attachment" belongs
			// to the HTML body, not to the attachment list
			inline := cid != "" && disp != "attachment"
			pm.Parts = append(pm.Parts, part{Index: i, Name: name, MIME: ct, Size: int64(len(b)), ContentID: cid, Inline: inline, data: b})
		}
	}
	return pm, nil
}

func inlineName(h *mail.InlineHeader, ct string, i int) string {
	if _, params, err := h.ContentType(); err == nil && params["name"] != "" {
		return params["name"]
	}
	ext := ".bin"
	if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 {
		ext = exts[0]
	}
	return fmt.Sprintf("inline-%d%s", i, ext)
}

func (pm *parsedMail) part(index int) (part, bool) {
	for _, p := range pm.Parts {
		if p.Index == index {
			return p, true
		}
	}
	return part{}, false
}

var policy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowDataURIImages()
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}()

// safeHTML makes the HTML body displayable: cid: images become data: URIs,
// remote images are dropped unless remote is true (they track the reader),
// then everything not in a conservative allow-list (scripts, styles, forms,
// event handlers) is removed. It reports how many remote images it dropped.
func (pm *parsedMail) safeHTML(remote bool) (string, int) {
	doc, err := html.Parse(strings.NewReader(pm.HTML))
	if err != nil {
		return policy.Sanitize(pm.HTML), 0
	}
	byCID := map[string]part{}
	for _, p := range pm.Parts {
		if p.ContentID != "" {
			byCID[p.ContentID] = p
		}
	}

	blocked := 0
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			for i, a := range n.Attr {
				if a.Key != "src" {
					continue
				}
				src := strings.TrimSpace(a.Val)
				switch {
				case strings.HasPrefix(strings.ToLower(src), "cid:"):
					if p, ok := byCID[src[4:]]; ok && len(p.data) <= maxInlineImage && strings.HasPrefix(p.MIME, "image/") {
						n.Attr[i].Val = "data:" + p.MIME + ";base64," + base64.StdEncoding.EncodeToString(p.data)
					} else {
						n.Attr[i].Val = ""
					}
				case strings.HasPrefix(strings.ToLower(src), "data:"):
				default:
					if !remote {
						n.Attr[i].Val = ""
						blocked++
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return policy.Sanitize(pm.HTML), blocked
	}
	return policy.Sanitize(buf.String()), blocked
}
