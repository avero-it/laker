package domain

// Commit is what a run hands over at each commit: the parquet files it
// encoded and the index changes. The receiver (laker serve, for itself or
// for an agent) writes the files, a manifest and the index; the sender then
// saves its checkpoint.
type Commit struct {
	Run     string       `json:"run"`
	Adapter string       `json:"adapter"`
	Type    string       `json:"type"`
	Seq     int          `json:"seq"`
	Files   []CommitFile `json:"files"`
	// Mail documents carry only the place this run saw them in; the receiver
	// merges it with the places it already knows.
	Mail       []MailDoc `json:"mail"`
	FileDocs   []FileDoc `json:"file_docs"`
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
	return len(c.Files) == 0 && len(c.Mail) == 0 && len(c.FileDocs) == 0 && len(c.Unindex) == 0 && c.BlobsNew == 0
}
