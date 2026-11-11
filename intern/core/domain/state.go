package domain

import (
	"encoding/json"
	"strings"
)

// State keys and values shared by the sources and by `laker state rebuild`,
// which writes them back from the lake.

// FolderStateKey is the IMAP checkpoint of a folder.
func FolderStateKey(folder string) string { return "folder:" + folder }

type FolderState struct {
	UIDValidity int64 `json:"uidvalidity"`
	LastUID     int64 `json:"last_uid"`
}

// InventoryKey is the last known version of a scanned file.
func InventoryKey(path string) string { return "inv:" + path }

// InventoryPath is the path of an InventoryKey.
func InventoryPath(key string) string { return strings.TrimPrefix(key, "inv:") }

type InventoryEntry struct {
	Size      int64  `json:"size"`
	MtimeNano int64  `json:"mtime"`
	SHA256    string `json:"sha256"`
}

// MailLocations is where a message (by SHA256) has been seen: "account/folder".
type MailLocations struct {
	Accounts []string `json:"accounts"`
	Folders  []string `json:"folders"`
}

// Add merges one more place; it reports whether anything changed.
func (l *MailLocations) Add(account, folder string) bool {
	changed := false
	if !contains(l.Accounts, account) {
		l.Accounts = append(l.Accounts, account)
		changed = true
	}
	if f := account + "/" + folder; !contains(l.Folders, f) {
		l.Folders = append(l.Folders, f)
		changed = true
	}
	return changed
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Buckets of the state store.
const (
	BucketMailLocations = "_mailloc"
	BucketRuns          = "_runs"
	BucketMeta          = "_meta"
)

// Keys of BucketMeta.
const (
	// MetaLakeChange names the last commit that reached the lake.
	MetaLakeChange = "lake_change"
	// MetaSnapshotOf is the MetaLakeChange the last snapshot was taken at.
	MetaSnapshotOf = "snapshot_of"
)

// SourceBucket is the private state bucket of an adapter.
func SourceBucket(adapter string) string { return "src:" + adapter }

func MustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only plain structs of this package are marshalled
	}
	return b
}
