package discover

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/avero-it/laker/pkg/config"
)

func sortStrings(s []string) { sort.Strings(s) }

var fsExclude = []string{"**/.git/**", "**/node_modules/**", "**/.DS_Store", "**/*.tmp", "**/.Trash*/**"}

func fsCandidate(prefix, hint, key, comment string, paths []string, notes ...string) Candidate {
	return Candidate{
		Key: key,
		Adapter: config.GenAdapter{
			Name: name(prefix, "files", hint), Type: "fs", Schedule: "0 0 3 * * *", Comment: comment,
			Params: []config.GenParam{
				{Key: "paths", Value: paths}, {Key: "exclude", Value: fsExclude},
				{Key: "extract", Value: true}, {Key: "extractmaxsize", Value: 52428800},
			},
		},
		Notes: notes,
	}
}

var xdgRE = regexp.MustCompile(`(?m)^XDG_(DOCUMENTS|DESKTOP)_DIR="(.*)"`)

// folders proposes the document and desktop folders of the user.
func folders(_ context.Context, env Env, prefix string) []Candidate {
	candidates := []string{filepath.Join(env.Home, "Documents"), filepath.Join(env.Home, "Desktop")}
	if b, err := env.ReadFile(filepath.Join(env.Home, ".config", "user-dirs.dirs")); err == nil {
		// localized names, e.g. XDG_DOCUMENTS_DIR="$HOME/Dokumente"
		candidates = nil
		for _, m := range xdgRE.FindAllStringSubmatch(string(b), -1) {
			candidates = append(candidates, strings.Replace(m[2], "$HOME", env.Home, 1))
		}
	}
	var paths []string
	for _, p := range candidates {
		if st, err := env.Stat(p); err == nil && st.IsDir() && p != env.Home {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	return []Candidate{fsCandidate(prefix, "home", "fs:home", "Document folders of "+env.Home+".", paths)}
}

var shareTypes = map[string]bool{"cifs": true, "smb3": true, "smbfs": true, "nfs": true, "nfs4": true, "fuse.sshfs": true, "afpfs": true}

// shares proposes the network shares mounted on this machine.
func shares(_ context.Context, env Env, prefix string) []Candidate {
	var out []Candidate
	switch env.GOOS {
	case "linux":
		b, err := env.ReadFile("/proc/mounts")
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 3 || !shareTypes[f[2]] {
				continue
			}
			mount := strings.ReplaceAll(f[1], `\040`, " ")
			out = append(out, fsCandidate(prefix, filepath.Base(mount), "fs:"+mount,
				"Network share "+f[0]+" ("+f[2]+") mounted on "+mount+".", []string{mount},
				"a share can be large: check the size with config validate before enabling it"))
		}
	case "darwin":
		vols, _ := env.Glob("/Volumes/*")
		for _, v := range vols {
			if filepath.Base(v) == "Macintosh HD" {
				continue
			}
			out = append(out, fsCandidate(prefix, filepath.Base(v), "fs:"+v, "Volume mounted on "+v+".", []string{v},
				"a volume can be external or network storage: check it before enabling it"))
		}
	}
	return out
}
