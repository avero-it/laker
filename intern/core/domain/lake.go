package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// LakeInfo describes the lake a build writes to, as `serve --check` reports it.
type LakeInfo struct {
	Kind     string `json:"kind"`     // "s3" | "fs"
	Location string `json:"location"` // bucket URL or directory
	// ObjectLock tells whether the storage itself forbids deleting and
	// overwriting objects (S3 Object Lock). Laker never deletes either way.
	ObjectLock bool   `json:"object_lock"`
	Retention  string `json:"retention,omitempty"` // default retention, e.g. "COMPLIANCE 3650d"
}

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// BlobKey is where the content with the given SHA-256 (lowercase hex) lives:
// blobs/sha256/ab/cd/abcd…. One content, one object, whoever brought it.
func BlobKey(sha256Hex string) (string, error) {
	if !sha256RE.MatchString(sha256Hex) {
		return "", fmt.Errorf("ERR100: %q is not a lowercase hex sha256", sha256Hex)
	}
	return "blobs/sha256/" + sha256Hex[0:2] + "/" + sha256Hex[2:4] + "/" + sha256Hex, nil
}

// ValidKey rejects keys that are not plain relative slash-separated paths, so
// no adapter can address something outside the lake root.
func ValidKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("ERR101: invalid lake key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "\\\x00") {
			return fmt.Errorf("ERR102: invalid lake key %q", key)
		}
	}
	return nil
}
