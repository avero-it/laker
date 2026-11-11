package domain

import "encoding/json"

// Commit is what a run hands over at each commit: the parquet files it
// encoded and the index changes. The receiver (laker serve, for itself or
// for an agent) writes the files, a manifest and the index; the sender then
// saves its checkpoint.
type Commit struct {
	Run     string `json:"run"`
	Adapter string `json:"adapter"`
	Type    string `json:"type"`
	Seq     int    `json:"seq"`
	// Part numbers the pieces a sender cut a large commit into, so that no
	// request is larger than it can afford; each piece is a commit of its
	// own, with the same Run and Seq.
	Part  int          `json:"part,omitempty"`
	Files []CommitFile `json:"files"`
	// Mail documents carry only the place this run saw them in; the receiver
	// merges it with the places it already knows.
	Mail       []MailDoc `json:"mail"`
	FileDocs   []FileDoc `json:"file_docs"`
	ChatDocs   []ChatDoc `json:"chat_docs"`
	Unindex    []string  `json:"unindex"`
	BlobsNew   int       `json:"blobs_new"`
	BlobsDedup int       `json:"blobs_dedup"`
	Bytes      int64     `json:"bytes"`
}

type CommitFile struct {
	Key  string `json:"key"`
	Data []byte `json:"data"`
}

// Empty tells a commit that changes nothing in the lake or the index.
func (c Commit) Empty() bool {
	return len(c.Files) == 0 && len(c.Mail) == 0 && len(c.FileDocs) == 0 && len(c.ChatDocs) == 0 && len(c.Unindex) == 0 && c.BlobsNew == 0
}

// Split cuts c into commits of about max bytes each on the wire, numbered by
// Part: the files first, then the index documents. A single file or document
// larger than max travels alone. Applying the pieces in order is the same as
// applying c; the counters of the run go with the first.
func (c Commit) Split(max int) []Commit {
	// in one piece a document unindexed by the same commit is not indexed:
	// keep it so when the two end up in different pieces
	unindexed := make(map[string]bool, len(c.Unindex))
	for _, id := range c.Unindex {
		unindexed[id] = true
	}

	var parts []Commit
	cur := Commit{Run: c.Run, Adapter: c.Adapter, Type: c.Type, Seq: c.Seq, BlobsNew: c.BlobsNew, BlobsDedup: c.BlobsDedup, Bytes: c.Bytes}
	size := 0
	// room makes space for n more bytes, closing the current piece if needed
	room := func(n int) {
		if size > 0 && size+n > max {
			parts = append(parts, cur)
			cur = Commit{Run: c.Run, Adapter: c.Adapter, Type: c.Type, Seq: c.Seq, Part: len(parts)}
			size = 0
		}
		size += n
	}
	jsonLen := func(v any) int {
		b, _ := json.Marshal(v)
		return len(b)
	}

	for _, f := range c.Files {
		room(len(f.Key) + len(f.Data)*4/3) // base64 in JSON
		cur.Files = append(cur.Files, f)
	}
	for _, d := range c.Mail {
		room(jsonLen(d))
		cur.Mail = append(cur.Mail, d)
	}
	for _, d := range c.FileDocs {
		if unindexed[FileDocID(d.Source, d.Path)] {
			continue
		}
		room(jsonLen(d))
		cur.FileDocs = append(cur.FileDocs, d)
	}
	for _, d := range c.ChatDocs {
		room(jsonLen(d))
		cur.ChatDocs = append(cur.ChatDocs, d)
	}
	for _, id := range c.Unindex {
		room(len(id) + 3)
		cur.Unindex = append(cur.Unindex, id)
	}
	return append(parts, cur)
}
