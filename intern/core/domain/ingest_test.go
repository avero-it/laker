package domain

import (
	"strings"
	"testing"
)

func TestSplitCutsALargeCommit(t *testing.T) {
	big := strings.Repeat("x", 300)
	c := Commit{
		Run: "01J", Adapter: "nas", Type: "fs", Seq: 2, BlobsNew: 4, BlobsDedup: 1, Bytes: 99,
		Files: []CommitFile{{Key: "a.parquet", Data: []byte(big)}, {Key: "b.parquet", Data: []byte("small")}},
		Mail:  []MailDoc{{SHA256: "m1", Body: big}, {SHA256: "m2", Body: "hi"}},
		FileDocs: []FileDoc{
			{Source: "nas", Path: "/kept", Content: "hi"},
			{Source: "nas", Path: "/gone", Content: "bye"},
		},
		ChatDocs: []ChatDoc{{Source: "nas", Text: "ciao"}},
		Unindex:  []string{FileDocID("nas", "/gone")},
	}

	// everything fits: one piece, as it was but for the unindexed document
	one := c.Split(1 << 20)
	if len(one) != 1 || one[0].Part != 0 || len(one[0].Files) != 2 || len(one[0].FileDocs) != 1 || one[0].BlobsNew != 4 {
		t.Fatalf("one piece: %+v", one)
	}

	parts := c.Split(200)
	if len(parts) < 3 {
		t.Fatalf("%d pieces, want the two large items apart from the rest", len(parts))
	}
	var files, mail, docs, chats, unindex []string
	for i, p := range parts {
		if p.Part != i || p.Run != "01J" || p.Adapter != "nas" || p.Type != "fs" || p.Seq != 2 {
			t.Errorf("piece %d is %+v", i, p)
		}
		if i > 0 && (p.BlobsNew != 0 || p.BlobsDedup != 0 || p.Bytes != 0) {
			t.Errorf("piece %d repeats the counters of the run", i)
		}
		if p.Empty() && i > 0 {
			t.Errorf("piece %d is empty", i)
		}
		for _, f := range p.Files {
			files = append(files, f.Key)
		}
		for _, d := range p.Mail {
			mail = append(mail, d.SHA256)
		}
		for _, d := range p.FileDocs {
			docs = append(docs, d.Path)
		}
		for range p.ChatDocs {
			chats = append(chats, "c")
		}
		unindex = append(unindex, p.Unindex...)
	}
	if parts[0].BlobsNew != 4 || parts[0].BlobsDedup != 1 || parts[0].Bytes != 99 {
		t.Errorf("the first piece lost the counters: %+v", parts[0])
	}
	got := strings.Join(files, ",") + "|" + strings.Join(mail, ",") + "|" + strings.Join(docs, ",") + "|" + strings.Join(chats, ",") + "|" + strings.Join(unindex, ",")
	if want := "a.parquet,b.parquet|m1,m2|/kept|c|file:nas:/gone"; got != want {
		t.Errorf("pieces carry %s\nwant         %s", got, want)
	}
	// the large file and the large message each travel alone
	if len(parts[0].Files) != 1 || len(parts[0].Mail) != 0 {
		t.Errorf("first piece: %d files, %d mails", len(parts[0].Files), len(parts[0].Mail))
	}

	// a commit that only counts blobs is still one commit
	if p := (Commit{Run: "01J", BlobsNew: 1}).Split(10); len(p) != 1 || p[0].BlobsNew != 1 {
		t.Errorf("blobs only: %+v", p)
	}

	if k := ManifestKey("nas", "01J", 2, 0); k != "_runs/nas/01J-0002.json" {
		t.Errorf("manifest of the first piece: %s", k)
	}
	if k := ManifestKey("nas", "01J", 2, 3); k != "_runs/nas/01J-0002-p003.json" {
		t.Errorf("manifest of a later piece: %s", k)
	}
}
