package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core"
	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/intern/coretest"
	"github.com/avero-it/laker/pkg/parquetx"
)

// The tests run against real servers when these are set, e.g.
//
//	LAKER_TEST_PG_DSN=postgres://postgres@127.0.0.1:5432/postgres
//	LAKER_TEST_MYSQL_DSN=root:pw@tcp(127.0.0.1:3306)/
func dsnFor(t *testing.T, typ string) string {
	env := map[string]string{TypePostgres: "LAKER_TEST_PG_DSN", TypeMySQL: "LAKER_TEST_MYSQL_DSN"}[typ]
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("%s not set", env)
	}
	return dsn
}

type schemaSQL struct {
	setup  []string
	schema string
}

func fixture(typ string) schemaSQL {
	if typ == TypePostgres {
		return schemaSQL{schema: "lakertest", setup: []string{
			`DROP SCHEMA IF EXISTS lakertest CASCADE`,
			`CREATE SCHEMA lakertest`,
			`CREATE TABLE lakertest.customers (id int PRIMARY KEY, name text, balance numeric(10,2), active boolean,
				created timestamptz, photo bytea, meta jsonb, score double precision)`,
			`CREATE TABLE lakertest.events (kind text, n int)`,
		}}
	}
	return schemaSQL{schema: "lakertest", setup: []string{
		`DROP DATABASE IF EXISTS lakertest`,
		`CREATE DATABASE lakertest`,
		"CREATE TABLE lakertest.customers (id int PRIMARY KEY, name text, balance decimal(10,2), active boolean, " +
			"created datetime(6), photo blob, meta json, score double)",
		`CREATE TABLE lakertest.events (kind text, n int)`,
	}}
}

func exec(t *testing.T, db *sql.DB, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

type customer struct {
	Op      string     `parquet:"_op"`
	ID      *int64     `parquet:"id"`
	Name    *string    `parquet:"name"`
	Balance *string    `parquet:"balance"`
	Active  *bool      `parquet:"active"`
	Created *time.Time `parquet:"created,timestamp(microsecond)"`
	Photo   []byte     `parquet:"photo"`
	Meta    *string    `parquet:"meta"`
	Score   *float64   `parquet:"score"`
}

func readAll[T any](t *testing.T, lake out.Lake, prefix string) []T {
	t.Helper()
	ctx := context.Background()
	var keys []string
	lake.List(ctx, prefix, func(k string) error { keys = append(keys, k); return nil })
	sort.Strings(keys)
	var all []T
	for _, k := range keys {
		rc, _ := lake.Get(ctx, k)
		b, _ := io.ReadAll(rc)
		rc.Close()
		rows, err := parquetx.Read[T](b)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		all = append(all, rows...)
	}
	return all
}

func ops(t *testing.T, lake out.Lake, table string) map[string]int {
	n := map[string]int{}
	for _, m := range readAll[domain.DBMeta](t, lake, "db/db-test/"+table+"/") {
		n[m.Op]++
	}
	return n
}

func TestHistory(t *testing.T) {
	for _, typ := range []string{TypePostgres, TypeMySQL} {
		t.Run(typ, func(t *testing.T) {
			dsn := dsnFor(t, typ)
			driver := dialects[typ].driver
			db, err := sql.Open(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			f := fixture(typ)
			exec(t, db, f.setup...)
			exec(t, db,
				`INSERT INTO lakertest.customers VALUES (1, 'Smith', 12.50, true, '2025-03-01 10:00:00', 'AB', '{"vip": true}', 0.5)`,
				`INSERT INTO lakertest.customers VALUES (2, 'Jones', 0, false, NULL, NULL, NULL, NULL)`,
				`INSERT INTO lakertest.customers VALUES (3, 'Brown', 7, true, '2025-03-02 11:30:00.123456', NULL, '[]', 1e10)`,
				`INSERT INTO lakertest.events VALUES ('click', 1), ('click', 1), ('view', 2)`,
			)

			env := coretest.New(t)
			ctx := context.Background()
			src, err := New("db-test", typ, map[string]any{"dsn": dsn, "tables": []any{"lakertest.*"}}, 2, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			a := core.Adapter{Name: "db-test", Type: typ, Source: src}

			run := env.Runner.Run(ctx, a, false)
			if run.Status != domain.RunOK || run.Stats.Rows != 6 {
				t.Fatalf("first run: %+v %s", run.Stats, run.Error)
			}
			if run := env.Runner.Run(ctx, a, false); run.Stats.Rows != 0 {
				t.Fatalf("unchanged database: %d rows", run.Stats.Rows)
			}

			exec(t, db,
				`UPDATE lakertest.customers SET balance = 99.99 WHERE id = 1`,
				`DELETE FROM lakertest.customers WHERE id = 2`,
				`INSERT INTO lakertest.customers (id, name) VALUES (4, 'Taylor')`,
			)
			if typ == TypePostgres {
				exec(t, db, `DELETE FROM lakertest.events WHERE ctid IN (SELECT ctid FROM lakertest.events WHERE kind = 'click' LIMIT 1)`)
			} else {
				exec(t, db, `DELETE FROM lakertest.events WHERE kind = 'click' LIMIT 1`)
			}
			run = env.Runner.Run(ctx, a, false)
			if run.Status != domain.RunOK || run.Stats.Rows != 4 {
				t.Fatalf("third run: %+v %s", run.Stats, run.Error)
			}

			got := ops(t, env.Lake, "lakertest.customers")
			if got["insert"] != 4 || got["update"] != 1 || got["delete"] != 1 {
				t.Errorf("customers ops = %v", got)
			}
			if got := ops(t, env.Lake, "lakertest.events"); got["insert"] != 3 || got["delete"] != 1 {
				t.Errorf("events ops = %v", got)
			}

			rows := readAll[customer](t, env.Lake, "db/db-test/lakertest.customers/")
			var smith, brown *customer
			for i := range rows {
				if rows[i].ID != nil && *rows[i].ID == 1 && rows[i].Op == "insert" {
					smith = &rows[i]
				}
				if rows[i].ID != nil && *rows[i].ID == 3 {
					brown = &rows[i]
				}
			}
			if smith == nil || *smith.Name != "Smith" || *smith.Balance != "12.50" || !*smith.Active ||
				string(smith.Photo) != "AB" || !strings.Contains(*smith.Meta, "vip") || *smith.Score != 0.5 {
				t.Errorf("smith = %+v", smith)
			}
			for _, r := range rows {
				if r.Op == "delete" && (r.ID == nil || *r.ID != 2) {
					t.Errorf("deleted row without its key: %+v", r)
				}
			}
			if brown == nil || brown.Created == nil || brown.Created.Nanosecond() != 123456000 {
				t.Errorf("brown created = %v", brown)
			}

			// the state rebuilt from the lake matches the live one
			env.With(t, func(st out.StateStore, _ out.Indexer) {
				live := map[string]string{}
				st.ForEach(domain.SourceBucket("db-test"), "row:", func(k string, v []byte) error { live[k] = string(v); return nil })
				fresh := coretest.New(t)
				fresh.With(t, func(st2 out.StateStore, _ out.Indexer) {
					if _, err := core.RebuildState(ctx, env.Lake, st2); err != nil {
						t.Fatal(err)
					}
					rebuilt := map[string]string{}
					st2.ForEach(domain.SourceBucket("db-test"), "row:", func(k string, v []byte) error { rebuilt[k] = string(v); return nil })
					if fmt.Sprint(live) != fmt.Sprint(rebuilt) || len(live) != 5 {
						t.Errorf("rebuilt %d rows, live %d", len(rebuilt), len(live))
					}
				})
			})
		})
	}
}

func TestKindOf(t *testing.T) {
	cases := map[[2]string]parquetx.Kind{
		{"integer", "integer"}: parquetx.Int64, {"bigint", "bigint(20) unsigned"}: parquetx.String,
		{"numeric", "numeric"}: parquetx.String, {"timestamp with time zone", ""}: parquetx.Timestamp,
		{"jsonb", ""}: parquetx.JSON, {"bytea", ""}: parquetx.Bytes, {"uuid", ""}: parquetx.String,
	}
	for in, want := range cases {
		if got := kindOf(in[0], in[1]); got != want {
			t.Errorf("kindOf(%v) = %v, want %v", in, got, want)
		}
	}
}
