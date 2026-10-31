// Package parquetx buffers typed rows and encodes them as zstd Parquet.
package parquetx

import (
	"bytes"
	"fmt"
	"io"

	"github.com/parquet-go/parquet-go"
)

// Buffer is a set of rows of one table that become one Parquet file.
type Buffer interface {
	Len() int
	WriteParquet(w io.Writer) error
}

// Rows is a Buffer of T, whose parquet struct tags give the schema.
type Rows[T any] struct{ Items []T }

func (r *Rows[T]) Len() int { return len(r.Items) }

func (r *Rows[T]) WriteParquet(w io.Writer) error {
	return parquet.Write(w, r.Items, parquet.Compression(&parquet.Zstd))
}

// Tables hands out the buffer of a table, creating it on first use.
type Tables interface {
	Table(key string, newBuffer func() Buffer) Buffer
}

// Append adds row to the table at key.
func Append[T any](t Tables, key string, row T) {
	buf := t.Table(key, func() Buffer { return &Rows[T]{} }).(*Rows[T])
	buf.Items = append(buf.Items, row)
}

// Read decodes a whole Parquet file held in memory.
func Read[T any](b []byte) ([]T, error) {
	rows, err := parquet.Read[T](bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("ERR760: cannot decode parquet: %w", err)
	}
	return rows, nil
}
