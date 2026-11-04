package discover

import (
	"context"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The detectors here read the account configuration of more mail clients and
// propose an imap source for each, exactly like thunderbird and evolution:
// host, port, security and user name only. A password is never read, not even
// when the client keeps it in clear text in these same files (mutt's
// imap_pass, offlineimap's remotepass, mbsync's Pass): imapCandidate leaves it
// as a ${VARIABLE} the user fills in. The value mappings of a few of these
// formats are not authoritatively documented, so each detector defaults to
// TLS on 993 and only narrows it on a marker it recognizes; the proposal is
// reviewed interactively and probed before it is enabled, so a wrong default
// is corrected, while a missing account would not be.

// mailFrom builds a candidate from a found account, defaulting to TLS/993 and
// skipping anything without both a host and a user.
func mailFrom(prefix, host, user, origin, security string, port int) (Candidate, bool) {
	host, user = strings.TrimSpace(host), strings.TrimSpace(user)
	if host == "" || user == "" {
		return Candidate{}, false
	}
	if security == "" {
		security = "tls"
	}
	if port == 0 {
		port = 993
	}
	return imapCandidate(prefix, mailAccount{host: host, user: user, origin: origin, security: security, port: port}), true
}

var muttSetRE = regexp.MustCompile(`(?mi)^\s*set\s+([a-z_]+)\s*=\s*"?([^"\n#]*?)"?\s*(?:#.*)?$`)
var imapURLRE = regexp.MustCompile(`^(imaps?)://(?:([^@/]+)@)?([^:/]+)(?::(\d+))?`)

// mutt reads the IMAP account of mutt and neomutt from the first config file
// found. It takes the host from an imap(s):// URL in folder or spoolfile and
// the user from imap_user; it never reads imap_pass.
func mutt(_ context.Context, env Env, prefix string) []Candidate {
	files := []string{
		filepath.Join(env.Home, ".muttrc"),
		filepath.Join(env.Home, ".mutt", "muttrc"),
		filepath.Join(env.Home, ".config", "mutt", "muttrc"),
		filepath.Join(env.Home, ".neomuttrc"),
		filepath.Join(env.Home, ".config", "neomutt", "neomuttrc"),
	}
	for _, f := range files {
		b, err := env.ReadFile(f)
		if err != nil {
			continue
		}
		sets := map[string]string{}
		for _, m := range muttSetRE.FindAllStringSubmatch(string(b), -1) {
			sets[strings.ToLower(m[1])] = m[2]
		}
		host, user, security, port := "", sets["imap_user"], "tls", 993
		for _, key := range []string{"folder", "spoolfile", "postponed"} {
			u := imapURLRE.FindStringSubmatch(strings.TrimSpace(sets[key]))
			if u == nil {
				continue
			}
			host = u[3]
			if u[1] == "imap" {
				security, port = "starttls", 143
			}
			if u[2] != "" && user == "" {
				user = u[2]
			}
			if p, err := strconv.Atoi(u[4]); err == nil && p > 0 {
				port = p
			}
			break
		}
		if c, ok := mailFrom(prefix, host, user, "mutt ("+f+")", security, port); ok {
			return []Candidate{c}
		}
	}
	return nil
}

// clawsMail reads the IMAP accounts of Claws Mail (and Sylpheed, same format)
// from accountrc: one [Account: N] section each, protocol 3 is IMAP.
func clawsMail(_ context.Context, env Env, prefix string) []Candidate {
	files := []string{
		filepath.Join(env.Home, ".claws-mail", "accountrc"),
		filepath.Join(env.Home, ".sylpheed-2.0", "accountrc"),
	}
	var out []Candidate
	for _, f := range files {
		b, err := env.ReadFile(f)
		if err != nil {
			continue
		}
		ini := parseINI(b)
		for _, sec := range sortedMapKeys(ini) {
			a := ini[sec]
			if !strings.HasPrefix(sec, "Account:") || a["protocol"] != "3" {
				continue
			}
			security, port := "none", 143 // ssl_imap: 0 none, 1 SSL, 2 STARTTLS
			switch a["ssl_imap"] {
			case "1":
				security, port = "tls", 993
			case "2":
				security, port = "starttls", 143
			}
			if p, err := strconv.Atoi(a["imap_port"]); err == nil && p > 0 {
				port = p
			}
			if c, ok := mailFrom(prefix, a["receive_server"], a["user_id"], "Claws Mail ("+f+")", security, port); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

// geary reads the IMAP accounts of Geary: one geary.ini per account folder,
// in an [AccountInformation] section.
func geary(_ context.Context, env Env, prefix string) []Candidate {
	files, _ := env.Glob(filepath.Join(env.Home, ".local", "share", "geary", "*", "geary.ini"))
	var out []Candidate
	for _, f := range files {
		b, err := env.ReadFile(f)
		if err != nil {
			continue
		}
		a := parseINI(b)["AccountInformation"]
		user := a["imap_username"]
		if user == "" {
			user = filepath.Base(filepath.Dir(f)) // the folder is the address
		}
		security, port := "tls", 993
		switch {
		case a["imap_starttls"] == "true":
			security, port = "starttls", 143
		case a["imap_ssl"] == "false":
			security, port = "none", 143
		}
		if p, err := strconv.Atoi(a["imap_port"]); err == nil && p > 0 {
			port = p
		}
		if c, ok := mailFrom(prefix, a["imap_host"], user, "Geary ("+f+")", security, port); ok {
			out = append(out, c)
		}
	}
	return out
}

// kmail reads the IMAP accounts of KMail: one akonadi_imap_resource_*rc per
// account, in a [network] group. The password lives in KWallet, never here.
func kmail(_ context.Context, env Env, prefix string) []Candidate {
	files, _ := env.Glob(filepath.Join(env.Home, ".config", "akonadi_imap_resource_*rc"))
	var out []Candidate
	for _, f := range files {
		b, err := env.ReadFile(f)
		if err != nil {
			continue
		}
		a := parseINI(b)["network"]
		security, port := "tls", 993
		switch strings.ToLower(a["Safety"]) {
		case "starttls":
			security, port = "starttls", 143
		case "none":
			security, port = "none", 143
		}
		if p, err := strconv.Atoi(a["ImapPort"]); err == nil && p > 0 {
			port = p
		}
		if c, ok := mailFrom(prefix, a["ImapServer"], a["UserName"], "KMail ("+f+")", security, port); ok {
			out = append(out, c)
		}
	}
	return out
}

// offlineimap reads the IMAP repositories of OfflineIMAP: [Repository …]
// sections with type = IMAP.
func offlineimap(_ context.Context, env Env, prefix string) []Candidate {
	files := []string{
		filepath.Join(env.Home, ".offlineimaprc"),
		filepath.Join(env.Home, ".config", "offlineimap", "config"),
	}
	var out []Candidate
	for _, f := range files {
		b, err := env.ReadFile(f)
		if err != nil {
			continue
		}
		ini := parseINI(b)
		for _, sec := range sortedMapKeys(ini) {
			a := ini[sec]
			if !strings.HasPrefix(sec, "Repository") || !strings.EqualFold(a["type"], "IMAP") {
				continue
			}
			security, port := "tls", 993
			switch {
			case strings.EqualFold(a["starttls"], "yes"):
				security, port = "starttls", 143
			case strings.EqualFold(a["ssl"], "no"):
				security, port = "none", 143
			}
			if p, err := strconv.Atoi(a["remoteport"]); err == nil && p > 0 {
				port = p
			}
			if c, ok := mailFrom(prefix, a["remotehost"], a["remoteuser"], "OfflineIMAP ("+f+")", security, port); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

// mbsync reads the IMAP accounts of mbsync/isync from an mbsyncrc: each
// IMAPAccount block holds Host, User, Port and SSLType. Pass and PassCmd are
// never read.
func mbsync(_ context.Context, env Env, prefix string) []Candidate {
	files := []string{
		filepath.Join(env.Home, ".mbsyncrc"),
		filepath.Join(env.Home, ".config", "isync", "mbsyncrc"),
	}
	var out []Candidate
	emit := func(f, host, user, sslType, port string) {
		security, p := "tls", 993
		switch strings.ToLower(sslType) {
		case "starttls":
			security, p = "starttls", 143
		case "none":
			security, p = "none", 143
		}
		if n, err := strconv.Atoi(port); err == nil && n > 0 {
			p = n
		}
		if c, ok := mailFrom(prefix, host, user, "mbsync ("+f+")", security, p); ok {
			out = append(out, c)
		}
	}
	for _, f := range files {
		b, err := env.ReadFile(f)
		if err != nil {
			continue
		}
		var host, user, sslType, port string
		inAccount := false
		flush := func() {
			if inAccount {
				emit(f, host, user, sslType, port)
			}
			host, user, sslType, port, inAccount = "", "", "", "", false
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line[0] == '#' {
				continue
			}
			key, rest, _ := strings.Cut(line, " ")
			val := strings.Trim(strings.TrimSpace(rest), `"`)
			switch key {
			case "IMAPAccount":
				flush()
				inAccount = true
			case "IMAPStore", "MaildirStore", "Channel", "Group":
				flush()
			case "Host":
				host = val
			case "User":
				user = val
			case "SSLType":
				sslType = val
			case "Port":
				port = val
			}
		}
		flush()
	}
	return out
}
