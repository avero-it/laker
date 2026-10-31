package web

import (
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
)

const pageSize = 25

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// the search asks for ANSI highlighting; the page escapes everything, then
// turns the ANSI marks into <mark>, so no indexed text is ever HTML
const (
	ansiOn  = "\x1b[43m"
	ansiOff = "\x1b[0m"
)

func markFragment(f string) template.HTML {
	f = html.EscapeString(strings.Join(strings.Fields(f), " "))
	f = strings.ReplaceAll(f, html.EscapeString(ansiOn), "<mark>")
	f = strings.ReplaceAll(f, html.EscapeString(ansiOff), "</mark>")
	return template.HTML(f)
}

func (s *Server) when(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case string:
		var err error
		if t, err = time.Parse(time.RFC3339, x); err != nil {
			return x
		}
	default:
		return ""
	}
	if t.IsZero() {
		return ""
	}
	return t.In(s.Location).Format("2006-01-02 15:04")
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, str(e))
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(x)
	}
}

func num(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

type hitView struct {
	Kind, SHA, Subject, From, Where, When string
	Name, Size, Source, Dir               string
	Fragments                             []template.HTML
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	kind := r.URL.Query().Get("kind")
	if kind != "mail" && kind != "file" {
		kind = ""
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)
	data := map[string]any{"Title": "Search", "Q": q, "Kind": kind}

	res, err := s.Index.Search(r.Context(), domain.Query{Text: q, Kind: kind, Limit: pageSize, Offset: offset, Highlight: "ansi"})
	if err != nil {
		data["Error"] = "Invalid search: " + err.Error()
		s.render(w, "search", data)
		return
	}
	var hits []hitView
	for _, h := range res.Hits {
		f := h.Fields
		v := hitView{Kind: str(f["kind"]), SHA: str(f["sha256"])}
		if v.Kind == "mail" {
			v.Subject, v.From, v.Where, v.When = str(f["subject"]), str(f["from"]), str(f["folder"]), s.when(f["date"])
		} else {
			p := str(f["path"])
			v.Name, v.Dir, v.Source = path.Base(p), path.Dir(p), str(f["source"])
			v.Size, v.When = humanSize(num(f["size"])), s.when(f["mtime"])
		}
		for _, field := range []string{"subject", "name", "body", "content"} {
			for _, frag := range h.Fragments[field] {
				v.Fragments = append(v.Fragments, markFragment(frag))
			}
		}
		hits = append(hits, v)
	}
	link := func(off int) string {
		return "/?" + url.Values{"q": {q}, "kind": {kind}, "offset": {strconv.Itoa(off)}}.Encode()
	}
	data["Result"], data["Hits"], data["Took"] = res, hits, fmt.Sprintf("%d ms", res.Took.Milliseconds())
	if offset > 0 {
		data["Prev"] = link(max(offset-pageSize, 0))
	}
	if uint64(offset+pageSize) < res.Total {
		data["Next"] = link(offset + pageSize)
	}
	s.render(w, "search", data)
}

// rawMail reads the original message from the lake.
func (s *Server) rawMail(r *http.Request, sha string) ([]byte, error) {
	if !shaRE.MatchString(sha) {
		return nil, errors.New("ERR408: not a sha256")
	}
	key, _ := domain.BlobKey(sha)
	rc, err := s.Lake.Get(r.Context(), key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

type attachmentView struct {
	Index      int
	Name, MIME string
	Size       string
}

func (s *Server) mail(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !shaRE.MatchString(sha) {
		s.fail(w, http.StatusNotFound, "Message not found", errors.New("ERR409: not a sha256"))
		return
	}
	doc, err := s.Index.Document(r.Context(), domain.MailDocID(sha))
	if err != nil || doc == nil {
		s.fail(w, http.StatusNotFound, "Message not found", fmt.Errorf("ERR40A: %s is not in the index", sha))
		return
	}
	data := map[string]any{
		"Title": str(doc["subject"]), "SHA": sha, "Subject": str(doc["subject"]), "From": str(doc["from"]),
		"To": str(doc["to"]), "Cc": str(doc["cc"]), "When": s.when(doc["date"]), "Where": str(doc["folder"]),
		"Body": str(doc["body"]),
	}

	raw, err := s.rawMail(r, sha)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "Cannot read the message", err)
		return
	}
	pm, err := parseEML(raw)
	if err == nil {
		var atts []attachmentView
		for _, p := range pm.Parts {
			if !p.Inline {
				atts = append(atts, attachmentView{Index: p.Index, Name: p.Name, MIME: p.MIME, Size: humanSize(p.Size)})
			}
		}
		data["Attachments"], data["HasHTML"] = atts, pm.HTML != ""

		q := r.URL.Query()
		if pm.HTML != "" && q.Get("html") == "1" {
			images := q.Get("images") == "1"
			body, blocked := pm.safeHTML(images)
			img := "data:"
			if images {
				img = "data: https: http:"
			}
			// the srcdoc frame inherits this policy: no script anywhere,
			// remote images only on request
			w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' "+img+"; style-src 'self' 'unsafe-inline'; frame-ancestors 'self'")
			data["ShowHTML"], data["Images"], data["Blocked"] = true, images, blocked
			data["HTML"] = `<!doctype html><meta charset="utf-8"><base target="_blank">` +
				`<style>body{font-family:system-ui,sans-serif;font-size:15px;color:#111;margin:12px;word-wrap:break-word}img{max-width:100%}</style>` + body
		}
	}
	s.render(w, "mail", data)
}

func (s *Server) eml(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	raw, err := s.rawMail(r, sha)
	if err != nil {
		s.fail(w, http.StatusNotFound, "Message not found", err)
		return
	}
	download(w, sha[:12]+".eml", "message/rfc822", false)
	w.Write(raw)
}

func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	raw, err := s.rawMail(r, r.PathValue("sha"))
	if err != nil {
		s.fail(w, http.StatusNotFound, "Message not found", err)
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	pm, err := parseEML(raw)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "Cannot read the message", err)
		return
	}
	p, ok := pm.part(n)
	if !ok {
		s.fail(w, http.StatusNotFound, "Attachment not found", fmt.Errorf("ERR40B: no part %d", n))
		return
	}
	download(w, p.Name, p.MIME, false)
	w.Write(p.data)
}

// inlineTypes are shown in the browser; anything else, HTML above all, is
// only downloaded so no stored file can run in the page's origin.
var inlineTypes = map[string]bool{
	"application/pdf": true, "image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"text/plain": true, "audio/mpeg": true, "video/mp4": true,
}

func download(w http.ResponseWriter, name, contentType string, inline bool) {
	base, _, _ := mime.ParseMediaType(contentType)
	disposition := "attachment"
	if inline && inlineTypes[base] {
		disposition = "inline"
	} else if !inlineTypes[base] {
		contentType = "application/octet-stream"
	}
	if base == "text/plain" {
		contentType = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	if base == "application/pdf" {
		w.Header().Del("Content-Security-Policy") // the browser PDF viewer needs its own resources
	}
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request) {
	sha, name := r.PathValue("sha"), path.Base(r.PathValue("name"))
	key, err := domain.BlobKey(sha)
	if err != nil {
		s.fail(w, http.StatusNotFound, "File not found", err)
		return
	}
	rc, err := s.Lake.Get(r.Context(), key)
	if err != nil {
		s.fail(w, http.StatusNotFound, "File not found", err)
		return
	}
	defer rc.Close()
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	download(w, name, ct, r.URL.Query().Get("inline") == "1")
	io.Copy(w, rc)
}

func (s *Server) fileSources() []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range s.Adapters {
		if a.Type == "fs" {
			out, seen[a.Name] = append(out, a.Name), true
		}
	}
	_, owners := s.agentSources()
	var remote []string
	for src, o := range owners {
		if o.Type == "fs" && !seen[src] {
			remote = append(remote, src)
		}
	}
	sort.Strings(remote)
	return append(out, remote...)
}

func (s *Server) knownSource(name string) bool {
	for _, src := range s.fileSources() {
		if src == name {
			return true
		}
	}
	return false
}

func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	s.render(w, "sources", map[string]any{"Title": "Files", "Sources": s.fileSources()})
}

type crumb struct{ Name, Dir string }

type dirView struct {
	Name, Dir, Size string
	Files           int
}

type fileView struct {
	Name, Path, SHA, Size, When string
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	if !s.knownSource(source) {
		s.fail(w, http.StatusNotFound, "Source not found", fmt.Errorf("ERR40C: no file source %q", source))
		return
	}
	q := r.URL.Query()
	data := map[string]any{"Title": source, "Source": source, "At": q.Get("at")}

	var at time.Time
	if a := q.Get("at"); a != "" {
		t, err := core.ParseAt(a, s.Location)
		if err != nil {
			data["Error"] = err.Error()
			s.render(w, "files", data)
			return
		}
		at = t
		data["AtQuery"] = template.URL("&at=" + url.QueryEscape(a))
	}
	tree, err := core.Tree(r.Context(), s.Lake, s.State, source, at)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "Cannot read the tree", err)
		return
	}
	l, ok := core.List(tree, q.Get("dir"))
	if !ok && len(tree) > 0 {
		l, _ = core.List(tree, "") // the directory did not exist then: show the root
	}
	if len(tree) == 0 {
		data["Error"] = "No files at this date."
	}

	var crumbs []crumb
	acc := ""
	for _, seg := range strings.Split(strings.Trim(l.Dir, "/"), "/") {
		if seg == "" {
			continue
		}
		acc += "/" + seg
		crumbs = append(crumbs, crumb{Name: seg, Dir: acc})
	}
	var dirs []dirView
	for _, d := range l.Dirs {
		dirs = append(dirs, dirView{Name: d.Name, Dir: l.Dir + "/" + d.Name, Size: humanSize(d.Size), Files: d.Files})
	}
	var files []fileView
	for _, f := range l.Files {
		files = append(files, fileView{Name: path.Base(f.Path), Path: f.Path, SHA: f.SHA256, Size: humanSize(f.Size), When: s.when(f.Mtime)})
	}
	data["Dir"], data["Crumbs"], data["Dirs"], data["Files"] = l.Dir, crumbs, dirs, files
	s.render(w, "files", data)
}

type versionView struct {
	When, Op, Size, SHA string
}

var opNames = map[string]string{
	domain.FileAdd: "added", domain.FileModify: "modified", domain.FileDelete: "deleted", domain.FileMove: "moved",
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	source, p := r.PathValue("source"), r.URL.Query().Get("path")
	if !s.knownSource(source) {
		s.fail(w, http.StatusNotFound, "Source not found", fmt.Errorf("ERR40D: no file source %q", source))
		return
	}
	rows, err := core.FileHistory(r.Context(), s.Lake, source, p)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "Cannot read the history", err)
		return
	}
	var versions []versionView
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		v := versionView{When: s.when(row.SeenAt), Op: opNames[row.Op], SHA: row.SHA256, Size: humanSize(row.Size)}
		switch {
		case row.Op == domain.FileMove && row.FromPath == p:
			v.Op, v.SHA = "moved to "+row.Path, ""
		case row.Op == domain.FileMove:
			v.Op = "moved from " + row.FromPath
		case row.Op == domain.FileDelete:
			v.SHA, v.Size = "", ""
		}
		versions = append(versions, v)
	}
	s.render(w, "history", map[string]any{"Title": path.Base(p), "Name": path.Base(p), "Source": source, "Path": p, "Versions": versions})
}

type adapterView struct {
	Name, Type, Schedule, Status, Last, Next, Error, Agent string
	Rows                                                   int
	Enabled, Running, CanRun                               bool
}

func (s *Server) adapterViews() []adapterView {
	var out []adapterView
	for _, a := range s.Adapters {
		v := adapterView{Name: a.Name, Type: a.Type, Schedule: a.Schedule, Enabled: a.Enabled, CanRun: a.Enabled,
			Running: s.Runs.Running(a.Name)}
		if n := s.Runs.Next(a.Name); !n.IsZero() {
			v.Next = s.when(n)
		}
		if run, err := core.LastRun(s.State, a.Name); err == nil && run != nil {
			v.Status, v.Last, v.Rows, v.Error = string(run.Status), s.when(run.EndedAt), run.Stats.Rows, run.Error
		}
		out = append(out, v)
	}

	// the sources of the agents, after the local ones
	_, owners := s.agentSources()
	names := map[string]string{}
	if list, err := core.ListAgents(s.State); err == nil {
		for _, a := range list {
			names[a.ID] = a.Name
		}
	}
	var remote []string
	for src, o := range owners {
		if o.Agent != domain.LocalOwner {
			remote = append(remote, src)
		}
	}
	sort.Strings(remote)
	for _, src := range remote {
		o := owners[src]
		v := adapterView{Name: src, Type: o.Type, Enabled: true, Agent: names[o.Agent]}
		if v.Agent == "" {
			v.Agent = o.Agent
		}
		if run, err := core.LastRun(s.State, src); err == nil && run != nil {
			v.Status, v.Last, v.Rows, v.Error = string(run.Status), s.when(run.EndedAt), run.Stats.Rows, run.Error
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Title": "Status", "Adapters": s.adapterViews(), "Version": s.Version,
		"Lake": s.LakeInfo.Kind + " " + s.LakeInfo.Location}
	if s.LakeInfo.ObjectLock {
		data["Lake"] = data["Lake"].(string) + " · Object Lock " + s.LakeInfo.Retention
	}
	switch q := r.URL.Query(); {
	case q.Get("started") != "":
		data["Notice"] = "Started: " + q.Get("started")
	case q.Get("busy") != "":
		data["Notice"] = "Already running: " + q.Get("busy")
	}
	s.render(w, "status", data)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch err := s.Runs.Trigger(name); {
	case err == nil:
		s.Logger.Info().Str("job", name).Msg("INF401: run started from the web interface")
		http.Redirect(w, r, "/status?started="+url.QueryEscape(name), http.StatusSeeOther)
	case errors.Is(err, core.ErrRunning):
		http.Redirect(w, r, "/status?busy="+url.QueryEscape(name), http.StatusSeeOther)
	default:
		s.fail(w, http.StatusNotFound, "Adapter not found", err)
	}
}
