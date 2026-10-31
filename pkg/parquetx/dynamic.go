package parquetx

import (
	"fmt"
	"io"
	"time"

	"github.com/parquet-go/parquet-go"
)

// Kind is the type of a column whose schema is only known at run time.
type Kind int

const (
	String Kind = iota
	Int64
	Double
	Bool
	Timestamp // stored in microseconds, UTC
	Bytes
	JSON
)

type Column struct {
	Name string
	Kind Kind
}

// Dynamic is a Buffer of rows whose columns (all nullable) come from a
// database table.
type Dynamic struct {
	schema *parquet.Schema
	index  map[string]int
	kinds  map[string]Kind
	rows   []parquet.Row
}

func NewDynamic(name string, cols []Column) *Dynamic {
	g := parquet.Group{}
	kinds := map[string]Kind{}
	for _, c := range cols {
		var n parquet.Node
		switch c.Kind {
		case Int64:
			n = parquet.Int(64)
		case Double:
			n = parquet.Leaf(parquet.DoubleType)
		case Bool:
			n = parquet.Leaf(parquet.BooleanType)
		case Timestamp:
			n = parquet.Timestamp(parquet.Microsecond)
		case Bytes:
			n = parquet.Leaf(parquet.ByteArrayType)
		case JSON:
			n = parquet.JSON()
		default:
			n = parquet.String()
		}
		g[c.Name] = parquet.Compressed(parquet.Optional(n), &parquet.Zstd)
		kinds[c.Name] = c.Kind
	}
	schema := parquet.NewSchema(name, g)
	index := map[string]int{}
	for i, path := range schema.Columns() {
		index[path[0]] = i
	}
	return &Dynamic{schema: schema, index: index, kinds: kinds}
}

// Add appends a row; values are int64, float64, bool, time.Time, []byte or
// string according to the column kind, nil for NULL. Missing columns are NULL.
func (d *Dynamic) Add(values map[string]any) error {
	row := make(parquet.Row, len(d.index))
	for name, i := range d.index {
		v := values[name]
		if v == nil {
			row[i] = parquet.NullValue().Level(0, 0, i)
			continue
		}
		var pv parquet.Value
		switch x := v.(type) {
		case int64:
			pv = parquet.Int64Value(x)
		case float64:
			pv = parquet.DoubleValue(x)
		case bool:
			pv = parquet.BooleanValue(x)
		case time.Time:
			pv = parquet.Int64Value(x.UTC().UnixMicro())
		case []byte:
			pv = parquet.ByteArrayValue(x)
		case string:
			pv = parquet.ByteArrayValue([]byte(x))
		default:
			return fmt.Errorf("ERR761: column %s: unsupported value %T", name, v)
		}
		row[i] = pv.Level(0, 1, i)
	}
	d.rows = append(d.rows, row)
	return nil
}

func (d *Dynamic) Len() int { return len(d.rows) }

func (d *Dynamic) WriteParquet(w io.Writer) error {
	pw := parquet.NewWriter(w, d.schema)
	if _, err := pw.WriteRows(d.rows); err != nil {
		return fmt.Errorf("ERR762: %w", err)
	}
	return pw.Close()
}
