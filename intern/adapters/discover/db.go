package discover

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/avero-it/laker/pkg/config"
)

type dbFound struct {
	typ, host, user, db, password, origin string
	port                                  int
	notes                                 []string
}

func dbCandidate(prefix string, f dbFound) Candidate {
	hint := f.db
	if hint == "" {
		hint = f.host
	}
	kind := "pg"
	if f.typ == "mysql" {
		kind = "mysql"
	}
	n := name(prefix, kind, hint)
	pw := varName(n, "PASSWORD")
	c := Candidate{
		Key:     fmt.Sprintf("%s:%s:%d:%s", f.typ, f.host, f.port, f.db),
		Secrets: []Secret{{Var: pw, Origin: f.origin, Value: f.password}},
		Notes:   f.notes,
	}
	var dsn string
	var tables []string
	if f.typ == "postgres" {
		db := f.db
		if db == "" {
			db = "postgres"
		}
		dsn = fmt.Sprintf("host=%s port=%d user=%s dbname=%s password='${%s}'", f.host, f.port, f.user, db, pw)
		c.Secrets[0].Quote = "libpq"
		tables = []string{"public.*"}
	} else {
		dsn = fmt.Sprintf("%s:${%s}@tcp(%s:%d)/%s", f.user, pw, f.host, f.port, f.db)
		tables = []string{}
	}
	c.Adapter = config.GenAdapter{
		Name: n, Type: f.typ, Schedule: "0 */30 * * * *", Comment: "Found in " + f.origin + ".",
		Params: []config.GenParam{
			{Key: "dsn", Value: dsn, Comment: "a read-only user is enough"},
			{Key: "tables", Value: tables}, {Key: "exclude", Value: []string{}},
		},
	}
	return c
}

// postgres reads ~/.pgpass and the PG* variables, and notices a server on
// the default port.
func postgres(ctx context.Context, env Env, prefix string) []Candidate {
	var out []Candidate
	pgpass := filepath.Join(env.Home, ".pgpass")
	if env.GOOS == "windows" {
		pgpass = filepath.Join(env.Getenv("APPDATA"), "postgresql", "pgpass.conf")
	}
	if b, err := env.ReadFile(pgpass); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for n := 1; sc.Scan(); n++ {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			f := splitPgpass(line)
			if len(f) != 5 || f[0] == "*" || f[3] == "*" {
				continue
			}
			port := 5432
			fmt.Sscan(f[1], &port)
			db := f[2]
			if db == "*" {
				db = ""
			}
			out = append(out, dbCandidate(prefix, dbFound{typ: "postgres", host: f[0], port: port, db: db, user: f[3],
				password: f[4], origin: fmt.Sprintf("%s line %d", pgpass, n)}))
		}
	}
	if h, u := env.Getenv("PGHOST"), env.Getenv("PGUSER"); h != "" && u != "" {
		port := 5432
		fmt.Sscan(env.Getenv("PGPORT"), &port)
		out = append(out, dbCandidate(prefix, dbFound{typ: "postgres", host: h, port: port, user: u, db: env.Getenv("PGDATABASE"),
			password: env.Getenv("PGPASSWORD"), origin: "the PG* environment variables"}))
	}
	if len(out) == 0 && reachable(ctx, env, 5432) {
		out = append(out, dbCandidate(prefix, dbFound{typ: "postgres", host: "localhost", port: 5432, user: "laker",
			origin: "a server listening on localhost:5432",
			notes:  []string{"set the user and database of the dsn; create a read-only user for laker"}}))
	}
	return out
}

// splitPgpass splits host:port:db:user:password, where \: and \\ escape.
func splitPgpass(line string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case line[i] == ':' && len(out) < 4:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(out, cur.String())
}

// mysql reads ~/.my.cnf and notices a server on the default port.
func mysql(ctx context.Context, env Env, prefix string) []Candidate {
	var out []Candidate
	cnf := filepath.Join(env.Home, ".my.cnf")
	if b, err := env.ReadFile(cnf); err == nil {
		client := parseINI(b)["client"]
		if client["user"] != "" {
			host := client["host"]
			if host == "" || host == "localhost" {
				host = "127.0.0.1"
			}
			port := 3306
			fmt.Sscan(client["port"], &port)
			out = append(out, dbCandidate(prefix, dbFound{typ: "mysql", host: host, port: port, user: client["user"],
				db: client["database"], password: client["password"], origin: cnf + " [client]"}))
		}
	}
	if len(out) == 0 && reachable(ctx, env, 3306) {
		out = append(out, dbCandidate(prefix, dbFound{typ: "mysql", host: "127.0.0.1", port: 3306, user: "laker",
			origin: "a server listening on localhost:3306",
			notes:  []string{"set the user and database of the dsn; create a read-only user for laker"}}))
	}
	return out
}
