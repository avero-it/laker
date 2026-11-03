package discover

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fixture is a home with a Thunderbird profile, an Evolution account, a
// .pgpass, a .my.cnf and document folders; nothing listens anywhere.
func fixture(t *testing.T) Env {
	home := t.TempDir()
	write(t, filepath.Join(home, ".thunderbird", "abcd.default", "prefs.js"), `
user_pref("mail.server.server1.hostname", "imap.gmail.com");
user_pref("mail.server.server1.type", "imap");
user_pref("mail.server.server1.port", 993);
user_pref("mail.server.server1.socketType", 3);
user_pref("mail.server.server1.userName", "jane@gmail.com");
user_pref("mail.server.server2.hostname", "mail.example.com");
user_pref("mail.server.server2.type", "imap");
user_pref("mail.server.server2.port", 143);
user_pref("mail.server.server2.socketType", 2);
user_pref("mail.server.server2.userName", "jane");
user_pref("mail.server.server3.hostname", "Local Folders");
user_pref("mail.server.server3.type", "none");
`)
	write(t, filepath.Join(home, ".config", "evolution", "sources", "1.source"), `
[Data Source]
DisplayName=Work
[Mail Account]
BackendName=imapx
[Authentication]
Host=imap.work.example
Port=993
User=jane.doe
[Security]
Method=ssl-on-alternate-port
`)
	write(t, filepath.Join(home, ".pgpass"), "# comment\ndb.example:5432:erp:reader:p@ss:w\\:rd\n*:*:*:x:y\n")
	write(t, filepath.Join(home, ".my.cnf"), "[client]\nuser=blog\npassword=\"s3cret\"\ndatabase=blog\n")
	os.MkdirAll(filepath.Join(home, "Docs"), 0o755)
	os.MkdirAll(filepath.Join(home, "Desk"), 0o755)
	write(t, filepath.Join(home, ".config", "user-dirs.dirs"), "XDG_DESKTOP_DIR=\"$HOME/Desk\"\nXDG_DOCUMENTS_DIR=\"$HOME/Docs\"\nXDG_MUSIC_DIR=\"$HOME/Music\"\n")
	os.MkdirAll(filepath.Join(home, ".local", "share", "TelegramDesktop", "tdata"), 0o700)
	os.MkdirAll(filepath.Join(home, ".var", "app", "com.rtosta.zapzap"), 0o700)
	mounts := filepath.Join(home, "mounts")
	write(t, mounts, "/dev/sda1 / ext4 rw 0 0\n//nas/archive /mnt/nas\\040archive cifs rw 0 0\n")

	env := OSEnv()
	env.Home, env.GOOS, env.DockerSocket = home, "linux", ""
	env.Getenv = func(string) string { return "" }
	env.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("closed") }
	real := env.ReadFile
	env.ReadFile = func(p string) ([]byte, error) {
		if p == "/proc/mounts" {
			return real(mounts)
		}
		return real(p)
	}
	return env
}

func byName(cs []Candidate) map[string]Candidate {
	m := map[string]Candidate{}
	for _, c := range cs {
		m[c.Adapter.Name] = c
	}
	return m
}

func param(c Candidate, key string) any {
	for _, p := range c.Adapter.Params {
		if p.Key == key {
			return p.Value
		}
	}
	return nil
}

func TestDiscover(t *testing.T) {
	got := byName(Run(context.Background(), fixture(t), "home"))
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}

	gmail, ok := got["home-mail-gmail-jane"]
	if !ok || param(gmail, "host") != "imap.gmail.com" || param(gmail, "security") != "tls" || len(gmail.Notes) == 0 {
		t.Errorf("gmail: %+v (all: %v)", gmail, names)
	}
	if gmail.Secrets[0].Found() || gmail.Secrets[0].Var != "LAKER_HOME_MAIL_GMAIL_JANE_PASSWORD" {
		t.Errorf("gmail secret: %+v", gmail.Secrets)
	}
	if other := got["home-mail-example-jane"]; param(other, "security") != "starttls" || param(other, "port") != 143 {
		t.Errorf("starttls account: %+v", other)
	}
	if work := got["home-mail-work-jane-doe"]; param(work, "host") != "imap.work.example" {
		t.Errorf("evolution: %+v (all: %v)", work, names)
	}

	pg := got["home-pg-erp"]
	if !strings.Contains(param(pg, "dsn").(string), "host=db.example port=5432 user=reader dbname=erp password='${LAKER_HOME_PG_ERP_PASSWORD}'") {
		t.Errorf("pg dsn: %v", param(pg, "dsn"))
	}
	if pg.Secrets[0].Value != "p@ss:w:rd" || !strings.Contains(pg.Secrets[0].Origin, ".pgpass line 2") {
		t.Errorf("pg secret: %+v", pg.Secrets)
	}
	my := got["home-mysql-blog"]
	if param(my, "dsn") != "blog:${LAKER_HOME_MYSQL_BLOG_PASSWORD}@tcp(127.0.0.1:3306)/blog" || my.Secrets[0].Value != "s3cret" {
		t.Errorf("mysql: %+v %+v", param(my, "dsn"), my.Secrets)
	}

	home := got["home-files-home"]
	if paths := param(home, "paths").([]string); len(paths) != 2 || !strings.HasSuffix(paths[0], "Desk") {
		t.Errorf("folders: %v", paths)
	}
	if nas := got["home-files-nas-archive"]; param(nas, "paths").([]string)[0] != "/mnt/nas archive" {
		t.Errorf("share: %+v (all: %v)", nas, names)
	}
	if wa := got["home-whatsapp"]; wa.Adapter.Type != "whatsapp" || len(wa.Secrets) != 0 || len(wa.Notes) != 1 {
		t.Errorf("whatsapp: %+v", wa)
	}
	tgc := got["home-telegram"]
	if tgc.Adapter.Type != "telegram" || len(tgc.Secrets) != 2 || tgc.Secrets[0].Found() {
		t.Errorf("telegram: %+v (all: %v)", tgc, names)
	}
	for _, c := range got {
		if c.Adapter.Enabled {
			t.Errorf("%s enabled", c.Adapter.Name)
		}
	}
}

func TestSecretEncoding(t *testing.T) {
	s := Secret{Value: `it's a \ test`, Quote: "libpq"}
	if s.Encoded() != `it\'s a \\ test` {
		t.Errorf("libpq: %q", s.Encoded())
	}
	if (Secret{Value: "hunter2"}).Masked() != "h*****2" {
		t.Errorf("masked: %q", (Secret{Value: "hunter2"}).Masked())
	}
}

func TestDocker(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("no unix sockets here")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"Id": "aaaaaaaaaaaaaaaa", "Names": []string{"/shop-db"}, "Image": "postgres:16"},
			{"Id": "bbbbbbbbbbbbbbbb", "Names": []string{"/blog-db"}, "Image": "mariadb:11"},
			{"Id": "cccccccccccccccc", "Names": []string{"/web"}, "Image": "nginx"},
		})
	})
	mux.HandleFunc("/containers/aaaaaaaaaaaaaaaa/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Config":{"Env":["POSTGRES_USER=shop","POSTGRES_PASSWORD=pw1","POSTGRES_DB=shop"]},
			"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"15432"}]}}}`))
	})
	mux.HandleFunc("/containers/bbbbbbbbbbbbbbbb/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Config":{"Env":["MARIADB_ROOT_PASSWORD=rootpw","MARIADB_DATABASE=blog"]},
			"NetworkSettings":{"Ports":{},"Networks":{"bridge":{"IPAddress":"172.17.0.3"}}}}`))
	})
	go http.Serve(ln, mux)
	t.Cleanup(func() { ln.Close() })

	env := fixture(t)
	env.DockerSocket = sock
	env.Dial = (&net.Dialer{}).DialContext
	got := byName(docker(context.Background(), env, "srv"))
	pg := got["srv-pg-shop"]
	if !strings.Contains(param(pg, "dsn").(string), "host=127.0.0.1 port=15432 user=shop dbname=shop") || pg.Secrets[0].Value != "pw1" {
		t.Errorf("postgres container: %+v %+v", param(pg, "dsn"), pg.Secrets)
	}
	maria := got["srv-mysql-blog"]
	if param(maria, "dsn") != "root:${LAKER_SRV_MYSQL_BLOG_PASSWORD}@tcp(172.17.0.3:3306)/blog" || maria.Secrets[0].Value != "rootpw" || len(maria.Notes) != 2 {
		t.Errorf("mariadb container: %+v %+v %v", param(maria, "dsn"), maria.Secrets, maria.Notes)
	}
	if len(got) != 2 {
		t.Errorf("candidates: %v", got)
	}
}
