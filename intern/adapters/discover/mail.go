package discover

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/avero-it/laker/pkg/config"
)

type mailAccount struct {
	host, user, security, origin string
	port                         int
}

func imapCandidate(prefix string, a mailAccount) Candidate {
	local := a.user
	if i := strings.Index(local, "@"); i > 0 {
		local = local[:i]
	}
	domain := a.host
	if net.ParseIP(a.host) == nil {
		labels := strings.Split(a.host, ".")
		domain = labels[max(0, len(labels)-2)] // imap.gmail.com: gmail
	}
	n := name(prefix, "mail", domain+"-"+local)
	pw := varName(n, "PASSWORD")
	folders, note := []string{"**"}, ""
	if strings.HasSuffix(a.host, "gmail.com") || strings.HasSuffix(a.host, "googlemail.com") {
		note = `Gmail: labels are folders, so every message appears several times (stored once); ` +
			`to read each message once, set folders to the "All Mail" folder ("config validate" lists the names)`
	}
	c := Candidate{
		Key: "imap:" + a.host + ":" + a.user,
		Adapter: config.GenAdapter{
			Name: n, Type: "imap", Schedule: "@every 15m",
			Comment: "Found in " + a.origin + ".",
			Params: []config.GenParam{
				{Key: "host", Value: a.host}, {Key: "port", Value: a.port}, {Key: "security", Value: a.security},
				{Key: "username", Value: a.user},
				{Key: "password", Value: "${" + pw + "}", Comment: "an app password where the provider asks for one"},
				{Key: "folders", Value: folders}, {Key: "exclude", Value: []string{"Trash", "Spam", "Junk"}},
			},
		},
		Secrets: []Secret{{Var: pw, Origin: a.origin + " (the mail client stores the password encrypted)"}},
	}
	if note != "" {
		c.Notes = append(c.Notes, note)
	}
	return c
}

var prefRE = regexp.MustCompile(`user_pref\("mail\.server\.(server\d+)\.(\w+)",\s*"?(.*?)"?\);`)

// thunderbird reads the IMAP accounts of every Thunderbird profile.
func thunderbird(_ context.Context, env Env, prefix string) []Candidate {
	var dirs []string
	switch env.GOOS {
	case "darwin":
		dirs = []string{filepath.Join(env.Home, "Library", "Thunderbird", "Profiles", "*")}
	case "windows":
		dirs = []string{filepath.Join(env.Getenv("APPDATA"), "Thunderbird", "Profiles", "*")}
	default:
		dirs = []string{filepath.Join(env.Home, ".thunderbird", "*"), filepath.Join(env.Home, "snap", "thunderbird", "common", ".thunderbird", "*")}
	}
	var out []Candidate
	for _, d := range dirs {
		profiles, _ := env.Glob(filepath.Join(d, "prefs.js"))
		for _, prefs := range profiles {
			b, err := env.ReadFile(prefs)
			if err != nil {
				continue
			}
			servers := map[string]map[string]string{}
			for _, m := range prefRE.FindAllStringSubmatch(string(b), -1) {
				if servers[m[1]] == nil {
					servers[m[1]] = map[string]string{}
				}
				servers[m[1]][m[2]] = m[3]
			}
			for _, id := range sortedMapKeys(servers) {
				s := servers[id]
				if s["type"] != "imap" || s["hostname"] == "" {
					continue
				}
				a := mailAccount{host: s["hostname"], user: s["userName"], origin: "Thunderbird (" + prefs + ")", security: "tls", port: 993}
				switch s["socketType"] { // 0 none, 2 STARTTLS, 3 SSL/TLS
				case "2":
					a.security, a.port = "starttls", 143
				case "0":
					a.security, a.port = "none", 143
				}
				if p, err := strconv.Atoi(s["port"]); err == nil {
					a.port = p
				}
				out = append(out, imapCandidate(prefix, a))
			}
		}
	}
	return out
}

// evolution reads the IMAP accounts of GNOME Evolution.
func evolution(_ context.Context, env Env, prefix string) []Candidate {
	files, _ := env.Glob(filepath.Join(env.Home, ".config", "evolution", "sources", "*.source"))
	var out []Candidate
	for _, f := range files {
		b, err := env.ReadFile(f)
		if err != nil {
			continue
		}
		ini := parseINI(b)
		if ini["Mail Account"]["BackendName"] != "imapx" {
			continue
		}
		auth := ini["Authentication"]
		a := mailAccount{host: auth["Host"], user: auth["User"], origin: "Evolution (" + f + ")", security: "tls", port: 993}
		if a.host == "" {
			continue
		}
		switch ini["Security"]["Method"] {
		case "starttls-on-standard-port":
			a.security, a.port = "starttls", 143
		case "none":
			a.security, a.port = "none", 143
		}
		if p, err := strconv.Atoi(auth["Port"]); err == nil && p > 0 {
			a.port = p
		}
		out = append(out, imapCandidate(prefix, a))
	}
	return out
}

// parseINI reads [section] key=value files.
func parseINI(b []byte) map[string]map[string]string {
	out := map[string]map[string]string{}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[' && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			if out[section] == nil {
				out[section] = map[string]string{}
			}
			out[section][strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}
