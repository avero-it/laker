// Package sqldb is the source that keeps the history of the tables of a
// Postgres or MySQL/MariaDB database: every run reads each table whole inside
// one consistent read-only snapshot, compares each row with the hash it had at
// the previous run, and appends to the lake only the rows inserted, updated
// or deleted since. The tables need no updated_at column; deletions are seen.
package sqldb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
	"github.com/avero-it/laker/pkg/config"
	"github.com/avero-it/laker/pkg/glob"
	"github.com/avero-it/laker/pkg/parquetx"
)

const (
	TypePostgres = "postgres"
	TypeMySQL    = "mysql"
)

type Params struct {
	// DSN as the driver takes it: "postgres://user:pass@host:5432/db" or
	// "user:pass@tcp(host:3306)/db".
	DSN string `yaml:"dsn"`
	// Tables and Exclude are patterns on "schema.table" (for MySQL the schema
	// is the database): "public.*", "shop.orders". Default: every table.
	Tables  []string `yaml:"tables"`
	Exclude []string `yaml:"exclude"`
}

type dialect struct {
	driver  string
	begin   []string
	commit  string
	quote   func(string) string
	ph      func(n int) string
	tables  string
	columns string // name, data_type, full type
}

var dialects = map[string]dialect{
	TypePostgres: {
		driver: "pgx",
		begin:  []string{"BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"},
		commit: "COMMIT",
		quote:  func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` },
		ph:     func(n int) string { return "$" + strconv.Itoa(n) },
		tables: `SELECT table_schema, table_name FROM information_schema.tables
			WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')
			ORDER BY 1, 2`,
		columns: `SELECT column_name, data_type, data_type FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position`,
	},
	TypeMySQL: {
		driver: "mysql",
		begin: []string{
			"SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ",
			"START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY",
		},
		commit: "COMMIT",
		quote:  func(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" },
		ph:     func(int) string { return "?" },
		tables: `SELECT table_schema, table_name FROM information_schema.tables
			WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
			ORDER BY 1, 2`,
		columns: `SELECT column_name, data_type, column_type FROM information_schema.columns
			WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position`,
	},
}

type Source struct {
	name        string
	typ         string
	d           dialect
	dsn         string
	include     glob.Set
	exclude     glob.Set
	commitEvery int
	logger      zerolog.Logger
	now         func() time.Time
}

var _ out.Source = (*Source)(nil)

func New(name, typ string, params map[string]any, commitEvery int, logger zerolog.Logger) (*Source, error) {
	var p Params
	if err := config.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	d, ok := dialects[typ]
	if !ok {
		return nil, fmt.Errorf("ERR220: unknown database type %q", typ)
	}
	if p.DSN == "" {
		return nil, fmt.Errorf("ERR221: dsn is required")
	}
	dsn := p.DSN
	tablesGiven := len(p.Tables) > 0
	if !tablesGiven {
		p.Tables = []string{"*.*"}
	}
	if typ == TypeMySQL {
		// times as time.Time in UTC, whatever the DSN says
		c, err := mysql.ParseDSN(dsn)
		if err != nil {
			return nil, fmt.Errorf("ERR222: invalid dsn: %w", err)
		}
		c.ParseTime, c.Loc = true, time.UTC
		dsn = c.FormatDSN()
		// a MySQL login sees every database: by default, only the one named
		// in the DSN
		if !tablesGiven && c.DBName != "" {
			p.Tables = []string{c.DBName + ".*"}
		}
	}
	return &Source{
		name: name, typ: typ, d: d, dsn: dsn, include: glob.CompileAll(p.Tables), exclude: glob.CompileAll(p.Exclude),
		commitEvery: commitEvery, logger: logger, now: time.Now,
	}, nil
}

func (s *Source) Run(ctx context.Context, rs out.RunSink) error {
	db, err := sql.Open(s.d.driver, s.dsn)
	if err != nil {
		return fmt.Errorf("ERR223: %w", err)
	}
	defer db.Close()

	// one connection holds the snapshot for every table
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("ERR224: cannot connect: %w", err)
	}
	defer conn.Close()
	for _, q := range s.d.begin {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("ERR225: cannot open the snapshot: %w", err)
		}
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), s.d.commit)

	tables, err := s.tables(ctx, conn)
	if err != nil {
		return err
	}
	s.logger.Debug().Strs("tables", tables).Msg("DBG220: tables to read")

	for _, t := range tables {
		if err := s.table(ctx, conn, rs, t); err != nil {
			return fmt.Errorf("ERR226: table %s: %w", t, err)
		}
	}
	return nil
}

func (s *Source) tables(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, s.d.tables)
	if err != nil {
		return nil, fmt.Errorf("ERR227: cannot list the tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, fmt.Errorf("ERR228: %w", err)
		}
		t := schema + "." + name
		if s.include.Match(t) && !s.exclude.Match(t) {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

type column struct {
	name string
	kind parquetx.Kind
	typ  string // database type, part of the schema hash
}

func (s *Source) columns(ctx context.Context, conn *sql.Conn, schema, table string) ([]column, []string, error) {
	rows, err := conn.QueryContext(ctx, s.d.columns, schema, table)
	if err != nil {
		return nil, nil, fmt.Errorf("ERR229: cannot read the columns: %w", err)
	}
	defer rows.Close()
	var cols []column
	for rows.Next() {
		var name, dataType, fullType string
		if err := rows.Scan(&name, &dataType, &fullType); err != nil {
			return nil, nil, fmt.Errorf("ERR22A: %w", err)
		}
		if strings.HasPrefix(name, "_") && isMeta(name) {
			return nil, nil, fmt.Errorf("ERR22B: column %s clashes with a column Laker adds", name)
		}
		cols = append(cols, column{name: name, kind: kindOf(dataType, fullType), typ: fullType})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	pkQuery := fmt.Sprintf(`SELECT kcu.column_name FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema AND tc.table_name = kcu.table_name
		WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema = %s AND tc.table_name = %s
		ORDER BY kcu.ordinal_position`, s.d.ph(1), s.d.ph(2))
	pkRows, err := conn.QueryContext(ctx, pkQuery, schema, table)
	if err != nil {
		return nil, nil, fmt.Errorf("ERR22C: cannot read the primary key: %w", err)
	}
	defer pkRows.Close()
	var pk []string
	for pkRows.Next() {
		var c string
		if err := pkRows.Scan(&c); err != nil {
			return nil, nil, fmt.Errorf("ERR22D: %w", err)
		}
		pk = append(pk, c)
	}
	return cols, pk, pkRows.Err()
}

func isMeta(name string) bool {
	switch name {
	case domain.DBOp, domain.DBPK, domain.DBRowHash, domain.DBSeenAt, domain.DBSchemaHash, domain.DBRunID:
		return true
	}
	return false
}

// kindOf maps a database type to a Parquet column kind. Exact numbers
// (numeric, decimal) and anything unusual stay text, so no value is altered.
func kindOf(dataType, fullType string) parquetx.Kind {
	t := strings.ToLower(dataType)
	switch t {
	case "smallint", "integer", "int", "mediumint", "tinyint", "year":
		return parquetx.Int64
	case "bigint":
		if strings.Contains(strings.ToLower(fullType), "unsigned") {
			return parquetx.String // may not fit in int64
		}
		return parquetx.Int64
	case "real", "double precision", "double", "float":
		return parquetx.Double
	case "boolean":
		return parquetx.Bool
	case "date", "datetime", "timestamp", "timestamp without time zone", "timestamp with time zone":
		return parquetx.Timestamp
	case "bytea", "blob", "tinyblob", "mediumblob", "longblob", "binary", "varbinary":
		return parquetx.Bytes
	case "json", "jsonb":
		return parquetx.JSON
	default:
		return parquetx.String
	}
}

func (s *Source) table(ctx context.Context, conn *sql.Conn, rs out.RunSink, table string) error {
	schema, name, _ := strings.Cut(table, ".")
	cols, pk, err := s.columns(ctx, conn, schema, name)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}

	schemaHash := hashStrings(func() []string {
		var parts []string
		for _, c := range cols {
			parts = append(parts, c.name, c.typ)
		}
		return parts
	}())
	pqCols := []parquetx.Column{
		{Name: domain.DBOp, Kind: parquetx.String}, {Name: domain.DBPK, Kind: parquetx.String},
		{Name: domain.DBRowHash, Kind: parquetx.String}, {Name: domain.DBSeenAt, Kind: parquetx.Timestamp},
		{Name: domain.DBSchemaHash, Kind: parquetx.String}, {Name: domain.DBRunID, Kind: parquetx.String},
	}
	for _, c := range cols {
		pqCols = append(pqCols, parquetx.Column{Name: c.name, Kind: c.kind})
	}
	newBuffer := func() parquetx.Buffer { return parquetx.NewDynamic(table, pqCols) }

	var quoted []string
	for _, c := range cols {
		quoted = append(quoted, s.d.quote(c.name))
	}
	query := fmt.Sprintf("SELECT %s FROM %s.%s", strings.Join(quoted, ", "), s.d.quote(schema), s.d.quote(name))
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("ERR22E: cannot read: %w", err)
	}
	defer rows.Close()

	pkIdx := map[string]bool{}
	for _, p := range pk {
		pkIdx[p] = true
	}
	seen := map[string]bool{}
	dups := map[string]int{} // tables without a primary key: identical rows
	changed, total := 0, 0
	raw := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}

	emit := func(op, pkStr, hash string, values map[string]any) error {
		values[domain.DBOp], values[domain.DBPK], values[domain.DBRowHash] = op, pkStr, hash
		values[domain.DBSeenAt], values[domain.DBSchemaHash], values[domain.DBRunID] = s.now().UTC(), schemaHash, rs.RunID()
		buf := rs.Table(domain.DBPrefix(s.name, table, s.now()), newBuffer).(*parquetx.Dynamic)
		if err := buf.Add(values); err != nil {
			return err
		}
		changed++
		if changed%s.commitEvery == 0 {
			return rs.Commit(ctx)
		}
		return nil
	}

	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("ERR22F: %w", err)
		}
		total++
		values := make(map[string]any, len(cols)+6)
		canon := make([]string, 0, 2*len(cols))
		var pkParts []string
		for i, c := range cols {
			v, err := normalize(c.kind, raw[i])
			if err != nil {
				return fmt.Errorf("ERR230: column %s: %w", c.name, err)
			}
			values[c.name] = v
			cs := canonical(v)
			canon = append(canon, c.name+":"+c.typ, cs)
			if pkIdx[c.name] {
				pkParts = append(pkParts, cs)
			}
		}
		hash := hashStrings(canon)
		var pkStr string
		if len(pk) > 0 {
			pkStr = string(domain.MustJSON(pkParts))
		} else {
			pkStr = hash + "#" + strconv.Itoa(dups[hash])
			dups[hash]++
		}
		seen[pkStr] = true

		key := domain.RowStateKey(table, pkStr)
		prev, err := rs.GetState(key)
		if err != nil {
			return err
		}
		if string(prev) == hash {
			continue
		}
		op := domain.DBUpdate
		if prev == nil {
			op = domain.DBInsert
		}
		rs.SetState(key, []byte(hash))
		if err := emit(op, pkStr, hash, values); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ERR231: %w", err)
	}

	var gone []string
	err = rs.ForEachState(domain.RowStatePrefix(table), func(k string, _ []byte) error {
		if p := strings.TrimPrefix(k, domain.RowStatePrefix(table)); !seen[p] {
			gone = append(gone, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(gone)
	for _, p := range gone {
		rs.DeleteState(domain.RowStateKey(table, p))
		if err := emit(domain.DBDelete, p, "", pkValues(cols, pkIdx, p)); err != nil {
			return err
		}
	}

	s.logger.Debug().Str("table", table).Int("rows", total).Int("changed", changed).Msg("DBG221: table read")
	return nil
}

// pkValues gives a deleted row its primary key columns back from its _pk
// (the canonical values, in column order), so the row is not all NULL.
func pkValues(cols []column, pkIdx map[string]bool, pk string) map[string]any {
	values := map[string]any{}
	var parts []string
	if len(pkIdx) == 0 || json.Unmarshal([]byte(pk), &parts) != nil {
		return values
	}
	i := 0
	for _, c := range cols {
		if !pkIdx[c.name] || i >= len(parts) {
			continue
		}
		if v, err := fromCanonical(c.kind, parts[i]); err == nil {
			values[c.name] = v
		}
		i++
	}
	return values
}

func fromCanonical(k parquetx.Kind, s string) (any, error) {
	if s == "\x00null" {
		return nil, nil
	}
	switch k {
	case parquetx.Int64:
		return strconv.ParseInt(s, 10, 64)
	case parquetx.Double:
		return strconv.ParseFloat(s, 64)
	case parquetx.Bool:
		return strconv.ParseBool(s)
	case parquetx.Timestamp:
		return time.Parse(time.RFC3339Nano, s)
	case parquetx.Bytes:
		return hex.DecodeString(s)
	default:
		return s, nil
	}
}

// normalize turns a driver value into the Go type its Parquet kind stores.
func normalize(k parquetx.Kind, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if b, ok := v.([]byte); ok && k != parquetx.Bytes {
		v = string(b)
	}
	switch k {
	case parquetx.Int64:
		switch x := v.(type) {
		case int64:
			return x, nil
		case uint64:
			if x > math.MaxInt64 {
				return nil, fmt.Errorf("ERR232: %d does not fit in int64", x)
			}
			return int64(x), nil
		case bool:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		case string:
			return strconv.ParseInt(x, 10, 64)
		}
	case parquetx.Double:
		switch x := v.(type) {
		case float64:
			return x, nil
		case float32:
			return float64(x), nil
		case int64:
			return float64(x), nil
		case string:
			return strconv.ParseFloat(x, 64)
		}
	case parquetx.Bool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case int64:
			return x != 0, nil
		case string:
			return strconv.ParseBool(x)
		}
	case parquetx.Timestamp:
		if t, ok := v.(time.Time); ok {
			return t.UTC(), nil
		}
	case parquetx.Bytes:
		switch x := v.(type) {
		case []byte:
			return append([]byte{}, x...), nil // the driver reuses its buffer
		case string:
			return []byte(x), nil
		}
	default:
		switch x := v.(type) {
		case string:
			return x, nil
		case time.Time:
			return x.UTC().Format(time.RFC3339Nano), nil
		default:
			return fmt.Sprint(x), nil
		}
	}
	return nil, fmt.Errorf("ERR233: unexpected %T for this column type", v)
}

// canonical is the text form of a normalized value hashed into the row hash.
func canonical(v any) string {
	switch x := v.(type) {
	case nil:
		return "\x00null"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return hex.EncodeToString(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func hashStrings(parts []string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
