package domain

import (
	"strings"
	"time"
)

// Database rows are stored as a changelog: every run reads each table whole,
// compares each row with the hash it had at the previous run, and appends
// only what changed. The columns of the table are kept with their types;
// these are added to each row:
const (
	DBOp         = "_op"          // insert | update | delete
	DBPK         = "_pk"          // primary key values, as a JSON array of strings
	DBRowHash    = "_row_hash"    // sha256 of the row (names, types, values)
	DBSeenAt     = "_seen_at"     // when the run read it
	DBSchemaHash = "_schema_hash" // sha256 of the table columns
	DBRunID      = "_run_id"
)

const (
	DBInsert = "insert"
	DBUpdate = "update"
	DBDelete = "delete"
)

// DBMeta reads only the bookkeeping columns of a changelog file.
type DBMeta struct {
	Op      string    `parquet:"_op"`
	PK      string    `parquet:"_pk"`
	RowHash string    `parquet:"_row_hash"`
	SeenAt  time.Time `parquet:"_seen_at,timestamp(microsecond)"`
	RunID   string    `parquet:"_run_id"`
}

// DBPrefix is where the changes of a table ("schema.table") live in the lake.
func DBPrefix(source, table string, day time.Time) string {
	return "db/" + source + "/" + table + "/changes/dt=" + day.UTC().Format("2006-01-02")
}

// DBTableOf splits a changelog key into its source and table.
func DBTableOf(key string) (source, table string, ok bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 5 || parts[0] != "db" || parts[3] != "changes" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// RowStateKey holds the last hash of a row of a table.
func RowStateKey(table, pk string) string { return "row:" + table + "\x00" + pk }

// RowStatePrefix covers every row of a table.
func RowStatePrefix(table string) string { return "row:" + table + "\x00" }
