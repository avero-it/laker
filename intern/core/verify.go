package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"path"
	"sort"
	"strings"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

// VerifyOptions says how far Verify goes beyond comparing names.
type VerifyOptions struct {
	// Hash is how many blobs, picked at random, are downloaded and hashed
	// again; negative: all of them.
	Hash int
	// Progress, if set, is told what Verify is doing.
	Progress func(step string)
}

// verifyExamples is how many of each kind of problem a report names; the
// counts are always complete.
const verifyExamples = 50

// Problems counts one kind of problem and names the first of them.
type Problems struct {
	Count    int      `json:"count"`
	Examples []string `json:"examples,omitempty"`
}

func (p *Problems) add(what string) {
	p.Count++
	if len(p.Examples) < verifyExamples {
		p.Examples = append(p.Examples, what)
	}
}

// VerifyReport is what Verify found. The lake is sound when OK is true.
type VerifyReport struct {
	Manifests    int `json:"manifests"`
	ParquetFiles int `json:"parquet_files"`
	Blobs        int `json:"blobs"`
	// BlobRefs is how many distinct contents the rows name.
	BlobRefs int `json:"blob_refs"`
	Hashed   int `json:"hashed"`

	// Problems: any of these makes the lake unsound.
	BadManifests  Problems `json:"bad_manifests"`  // cannot be read
	MissingFiles  Problems `json:"missing_files"`  // in a manifest, not in the lake
	BadParquet    Problems `json:"bad_parquet"`    // cannot be decoded
	MissingBlobs  Problems `json:"missing_blobs"`  // named by a row, not in the lake
	CorruptBlobs  Problems `json:"corrupt_blobs"`  // content does not hash to its name
	MisplacedKeys Problems `json:"misplaced_keys"` // under blobs/ but not a blob key

	// Notes: expected in a healthy lake, shown for information.
	// UnlistedFiles are Parquet files of no manifest: a commit that stopped
	// before its manifest (its rows are read all the same), or one in flight.
	UnlistedFiles Problems `json:"unlisted_files"`
	// UnreferencedBlobs are contents no row names: a run that stopped after
	// the upload and before the commit.
	UnreferencedBlobs int `json:"unreferenced_blobs"`
}

// OK tells a lake where everything written is there and intact, as far as
// the options looked.
func (r VerifyReport) OK() bool {
	return r.BadManifests.Count+r.MissingFiles.Count+r.BadParquet.Count+
		r.MissingBlobs.Count+r.CorruptBlobs.Count+r.MisplacedKeys.Count == 0
}

// dataPrefixes are where the runs write Parquet files.
var dataPrefixes = []string{"mail/messages/", "files/inventory/", "files/text/", "chat/messages/", "db/"}

type shaKey [sha256.Size]byte

func parseSHA(s string) (shaKey, bool) {
	var k shaKey
	if len(s) != 2*sha256.Size {
		return k, false
	}
	if _, err := hex.Decode(k[:], []byte(s)); err != nil {
		return k, false
	}
	return k, true
}

// Verify checks, reading only, that the lake holds what it says it holds:
// every file of every manifest, every Parquet file readable, every content
// named by a row, and (with opts.Hash) that contents still hash to their
// names. It reads what refers before what is referred to, so it can run
// while runs commit. It keeps in memory one entry for each content and each
// Parquet file. The error is for a lake that cannot be read at all; what is
// wrong with it is in the report.
func Verify(ctx context.Context, lake out.Lake, opts VerifyOptions) (VerifyReport, error) {
	var rep VerifyReport
	progress := func(format string, args ...any) {
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf(format, args...))
		}
	}

	// manifests first, then the files they name
	progress("reading the manifests")
	listed := map[string]bool{}
	var manifests []string
	err := lake.List(ctx, "_runs/", func(k string) error {
		if strings.HasSuffix(k, ".json") {
			manifests = append(manifests, k)
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	for _, k := range manifests {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Manifests++
		var man domain.Manifest
		if err := readJSON(ctx, lake, k, &man); err != nil {
			rep.BadManifests.add(k)
			continue
		}
		for _, f := range man.Files {
			listed[f] = true
		}
	}

	// rows before blobs: a blob is in the lake before the row that names it
	refs := map[shaKey]string{} // content → the first file naming it
	ref := func(sha, file string) {
		if sha == "" {
			return // recorded, not stored
		}
		k, ok := parseSHA(sha)
		if !ok {
			rep.BadParquet.add(file + ": row with the invalid sha256 " + sha)
			return
		}
		if _, seen := refs[k]; !seen {
			refs[k] = file
		}
	}
	present := map[string]bool{}
	for _, prefix := range dataPrefixes {
		progress("reading %s", prefix)
		keys, err := parquetKeys(ctx, lake, prefix, nil)
		if err != nil {
			return rep, err
		}
		for _, k := range keys {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			rep.ParquetFiles++
			present[k] = true
			if !listed[k] {
				rep.UnlistedFiles.add(k)
			}
			var err error
			switch prefix {
			case "mail/messages/":
				err = eachRow(ctx, lake, []string{k}, nil, func(r mailRef) error { ref(r.SHA256, k); return nil })
			case "files/inventory/":
				err = eachRow(ctx, lake, []string{k}, nil, func(r domain.FileRow) error { ref(r.SHA256, k); return nil })
			case "chat/messages/":
				err = eachRow(ctx, lake, []string{k}, nil, func(r domain.ChatRow) error {
					for _, m := range r.Media {
						ref(m.SHA256, k)
					}
					return nil
				})
			case "files/text/":
				err = eachRow(ctx, lake, []string{k}, nil, func(textRef) error { return nil })
			default:
				err = eachRow(ctx, lake, []string{k}, nil, func(domain.DBMeta) error { return nil })
			}
			if err != nil {
				if ctx.Err() != nil {
					return rep, ctx.Err()
				}
				rep.BadParquet.add(k)
			}
		}
	}
	for f := range listed {
		if present[f] {
			continue
		}
		// written since the listing, perhaps: ask for it by name
		if ok, err := lake.Exists(ctx, f); err != nil {
			return rep, err
		} else if !ok {
			rep.MissingFiles.add(f)
		}
	}
	sort.Strings(rep.MissingFiles.Examples)
	present, listed = nil, nil

	progress("listing the blobs")
	rep.BlobRefs = len(refs)
	var blobs []shaKey
	err = lake.List(ctx, "blobs/", func(k string) error {
		sha := path.Base(k)
		id, ok := parseSHA(sha)
		if want, err := domain.BlobKey(sha); !ok || err != nil || want != k {
			rep.MisplacedKeys.add(k)
			return nil
		}
		rep.Blobs++
		if _, named := refs[id]; named {
			delete(refs, id)
		} else {
			rep.UnreferencedBlobs++
		}
		if opts.Hash != 0 {
			blobs = append(blobs, id)
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	// what is left in refs was not in the listing
	for id, file := range refs {
		sha := hex.EncodeToString(id[:])
		key, _ := domain.BlobKey(sha)
		if ok, err := lake.Exists(ctx, key); err != nil {
			return rep, err
		} else if !ok {
			rep.MissingBlobs.add(sha + " named by " + file)
		}
	}
	sort.Strings(rep.MissingBlobs.Examples)

	if opts.Hash > 0 && opts.Hash < len(blobs) {
		rand.Shuffle(len(blobs), func(i, j int) { blobs[i], blobs[j] = blobs[j], blobs[i] })
		blobs = blobs[:opts.Hash]
	}
	for i, id := range blobs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if i%100 == 0 {
			progress("hashing the blobs: %d of %d", i, len(blobs))
		}
		sha := hex.EncodeToString(id[:])
		key, _ := domain.BlobKey(sha)
		got, err := hashObject(ctx, lake, key)
		if err != nil {
			return rep, err
		}
		rep.Hashed++
		if got != sha {
			rep.CorruptBlobs.add(key + " hashes to " + got)
		}
	}
	return rep, nil
}

func readJSON(ctx context.Context, lake out.Lake, key string, v any) error {
	rc, err := lake.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	return json.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(v)
}

func hashObject(ctx context.Context, lake out.Lake, key string) (string, error) {
	rc, err := lake.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", fmt.Errorf("ERR160: cannot read %s: %w", key, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
