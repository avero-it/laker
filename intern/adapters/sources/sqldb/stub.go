package sqldb

import "github.com/avero-it/laker/pkg/config"

// Stub is a documented, disabled database adapter for `config generate`.
func Stub(typ string) config.GenAdapter {
	a := config.GenAdapter{
		Type: typ, Schedule: "0 */30 * * * *",
		Comment: "A database: latest rows and the history of every row; a run is one version per row.",
	}
	if typ == TypePostgres {
		a.Name = "db-postgres"
		a.Params = []config.GenParam{
			{Key: "dsn", Value: "${LAKER_PG_DSN}", Comment: "postgres://user:password@host:5432/database (a read-only user is enough)"},
			{Key: "tables", Value: []string{"public.*"}, Comment: `"schema.table" patterns`},
			{Key: "exclude", Value: []string{}},
		}
	} else {
		a.Name = "db-mysql"
		a.Params = []config.GenParam{
			{Key: "dsn", Value: "${LAKER_MYSQL_DSN}", Comment: "user:password@tcp(host:3306)/database (MySQL or MariaDB, a read-only user is enough)"},
			{Key: "tables", Value: []string{}, Comment: `"database.table" patterns; empty: every table of the DSN database`},
			{Key: "exclude", Value: []string{}},
		}
	}
	return a
}
