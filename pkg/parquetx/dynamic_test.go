package parquetx

import (
	"bytes"
	"testing"
	"time"
)

func TestDynamicRoundTrip(t *testing.T) {
	d := NewDynamic("t", []Column{{"id", Int64}, {"name", String}, {"price", Double}, {"ok", Bool}, {"at", Timestamp}, {"raw", Bytes}, {"doc", JSON}})
	at := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	if err := d.Add(map[string]any{"id": int64(1), "name": "a", "price": 1.5, "ok": true, "at": at, "raw": []byte{0, 1}, "doc": `{"k":1}`}); err != nil {
		t.Fatal(err)
	}
	if err := d.Add(map[string]any{"id": int64(2)}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := d.WriteParquet(&buf); err != nil {
		t.Fatal(err)
	}

	type row struct {
		ID    *int64     `parquet:"id"`
		Name  *string    `parquet:"name"`
		Price *float64   `parquet:"price"`
		OK    *bool      `parquet:"ok"`
		At    *time.Time `parquet:"at,timestamp(microsecond)"`
		Raw   []byte     `parquet:"raw"`
		Doc   *string    `parquet:"doc"`
	}
	rows, err := Read[row](buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || *rows[0].ID != 1 || *rows[0].Name != "a" || *rows[0].Price != 1.5 || !*rows[0].OK ||
		!rows[0].At.Equal(at) || !bytes.Equal(rows[0].Raw, []byte{0, 1}) || *rows[0].Doc != `{"k":1}` {
		t.Fatalf("row 0 = %+v", rows[0])
	}
	if *rows[1].ID != 2 || rows[1].Name != nil || rows[1].At != nil {
		t.Fatalf("row 1 = %+v", rows[1])
	}
}
