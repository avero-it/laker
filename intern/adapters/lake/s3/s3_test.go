package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

// fakeS3 is the subset of path-style S3 the Lake uses, with If-None-Match.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	parts   map[string][]byte // uploadId/part → bytes
	lock    bool
	puts    int
	aborts  int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, "/bucket")
	key := strings.TrimPrefix(path, "/")
	q := r.URL.Query()
	conflict := func() bool {
		_, exists := f.objects[key]
		if exists && r.Header.Get("If-None-Match") == "*" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return true
		}
		return false
	}

	switch {
	case key == "" && r.Method == http.MethodHead:
	case key == "" && q.Has("object-lock"):
		if !f.lock {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>ObjectLockConfigurationNotFoundError</Code></Error>`)
			return
		}
		fmt.Fprint(w, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>`+
			`<Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>3650</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)
	case key == "" && q.Get("list-type") == "2":
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`)
		for _, k := range keys {
			fmt.Fprintf(w, `<Contents><Key>%s</Key></Contents>`, k)
		}
		fmt.Fprint(w, `</ListBucketResult>`)
	case r.Method == http.MethodHead:
		if _, ok := f.objects[key]; !ok {
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodGet:
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(b)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		b, _ := io.ReadAll(r.Body)
		f.parts[q.Get("uploadId")+"/"+fmt.Sprintf("%05s", q.Get("partNumber"))] = b
		w.Header().Set("ETag", `"etag"`)
	case r.Method == http.MethodPut:
		if conflict() {
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.objects[key] = b
		f.puts++
	case r.Method == http.MethodPost && q.Has("uploads"):
		fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		io.Copy(io.Discard, r.Body)
		if conflict() {
			return
		}
		var names []string
		for n := range f.parts {
			names = append(names, n)
		}
		sort.Strings(names)
		var all []byte
		for _, n := range names {
			all = append(all, f.parts[n]...)
		}
		f.objects[key] = all
		f.puts++
		fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>"x"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.aborts++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
	}
}

func newTestLake(t *testing.T) (*Lake, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: map[string][]byte{}, parts: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	l := New(Options{
		Endpoint: srv.URL, Region: "fsn1", Bucket: "bucket",
		AccessKey: "ak", SecretKey: "sk", PathStyle: true, ConditionalPut: true,
	}, zerolog.Nop())
	return l, fake
}

func TestPutIfAbsent(t *testing.T) {
	l, fake := newTestLake(t)
	ctx := context.Background()

	created, err := l.PutIfAbsent(ctx, "a/b", strings.NewReader("first"), 5)
	if err != nil || !created {
		t.Fatalf("first put: created=%v err=%v", created, err)
	}
	created, err = l.PutIfAbsent(ctx, "a/b", strings.NewReader("other"), 5)
	if err != nil || created {
		t.Fatalf("second put: created=%v err=%v", created, err)
	}
	if fake.puts != 1 || string(fake.objects["a/b"]) != "first" {
		t.Errorf("puts=%d content=%q", fake.puts, fake.objects["a/b"])
	}

	rc, err := l.Get(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "first" {
		t.Errorf("Get = %q", b)
	}
}

// Another writer creates the key between HEAD and PUT: the bucket answers 412
// and the Lake reports created=false instead of failing.
func TestPreconditionFailedIsNotAnError(t *testing.T) {
	l, fake := newTestLake(t)
	fake.objects["race"] = []byte("theirs")

	created, err := l.put(context.Background(), "race", strings.NewReader("mine"), 4)
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if string(fake.objects["race"]) != "theirs" {
		t.Error("object overwritten")
	}
}

func TestMultipart(t *testing.T) {
	partSize = 4
	t.Cleanup(func() { partSize = 64 << 20 })
	l, fake := newTestLake(t)

	body := "0123456789"
	created, err := l.PutIfAbsent(context.Background(), "big", strings.NewReader(body), int64(len(body)))
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if got := string(fake.objects["big"]); got != body {
		t.Errorf("assembled %q, want %q", got, body)
	}
	if fake.aborts != 0 {
		t.Errorf("aborts = %d", fake.aborts)
	}
}

func TestListAndExists(t *testing.T) {
	l, fake := newTestLake(t)
	fake.objects["mail/a/1"] = nil
	fake.objects["mail/ab/2"] = nil
	fake.objects["files/1"] = nil

	var got []string
	if err := l.List(context.Background(), "mail/", func(k string) error { got = append(got, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "mail/a/1,mail/ab/2" {
		t.Errorf("List = %v", got)
	}
	if ok, err := l.Exists(context.Background(), "files/1"); !ok || err != nil {
		t.Errorf("Exists = %v, %v", ok, err)
	}
	if ok, err := l.Exists(context.Background(), "files/2"); ok || err != nil {
		t.Errorf("Exists(missing) = %v, %v", ok, err)
	}
}

func TestCheckReadsObjectLock(t *testing.T) {
	l, fake := newTestLake(t)

	info, err := l.Check(context.Background())
	if err != nil || info.ObjectLock {
		t.Fatalf("without lock: %+v %v", info, err)
	}

	fake.lock = true
	info, err = l.Check(context.Background())
	if err != nil || !info.ObjectLock || info.Retention != "COMPLIANCE 3650d" {
		t.Fatalf("with lock: %+v %v", info, err)
	}
}
