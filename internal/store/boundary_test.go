package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

func TestTimeEncodingRoundTrips(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   time.Time
	}{
		{"utc", time.Date(2026, 4, 1, 12, 0, 0, 123456000, time.UTC)},
		{"offset", time.Date(2026, 4, 1, 14, 0, 0, 0, time.FixedZone("CEST", 2*3600))},
		{"epoch", time.Unix(0, 0).UTC()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := store.DecodeTime(store.EncodeTime(tc.in))
			if !got.Equal(tc.in) {
				t.Fatalf("round trip: %v -> %v", tc.in, got)
			}
			if got.Location() != time.UTC {
				t.Fatalf("decoded in %v, want UTC", got.Location())
			}
		})
	}
}

// TestTimeEncodingOrdersAcrossZones is the reason the columns are integers.
// The same instant written with an explicit offset and written naive compares
// differently as text — which silently interleaves rows a reader is paging
// through — and identically as microseconds.
func TestTimeEncodingOrdersAcrossZones(t *testing.T) {
	t.Parallel()
	utc := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	sameInstantElsewhere := utc.In(time.FixedZone("UTC+2", 2*3600))
	if store.EncodeTime(utc) != store.EncodeTime(sameInstantElsewhere) {
		t.Fatal("one instant encoded two ways")
	}
	earlier := utc.Add(-time.Minute)
	if store.EncodeTime(earlier) >= store.EncodeTime(sameInstantElsewhere) {
		t.Fatal("encoding does not preserve order across zones")
	}
}

func TestNullBoundary(t *testing.T) {
	t.Parallel()
	if store.NullText("") != nil {
		t.Error(`NullText("") must send NULL: SQL treats NULLs as distinct, ` +
			`which is what makes an unconstrained row unconstrained`)
	}
	if store.NullText("k") != any("k") {
		t.Error("NullText passed a real value through wrong")
	}
	if got := store.Text(sql.NullString{}); got != "" {
		t.Errorf("Text(NULL) = %q, want the Go zero value", got)
	}
	if got := store.Text(sql.NullString{String: "k", Valid: true}); got != "k" {
		t.Errorf("Text = %q", got)
	}
	if store.NullTime(time.Time{}) != nil {
		t.Error("the zero time means never; it must not encode as year 1")
	}
	if !store.TimeAt(sql.NullInt64{}).IsZero() {
		t.Error("TimeAt(NULL) must be the zero time")
	}
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	if got := store.TimeAt(sql.NullInt64{Int64: store.EncodeTime(at), Valid: true}); !got.Equal(at) {
		t.Errorf("TimeAt = %v, want %v", got, at)
	}
}

func TestVectorEncoding(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "v.db"),
		store.Options{EmbeddingDim: 4})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	in := []float32{1, -0.5, 0, 2.25}
	blob, err := db.EncodeVector(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(blob) != 4*len(in) {
		t.Fatalf("%d bytes for %d float32s", len(blob), len(in))
	}
	out, err := store.DecodeVector(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("round trip %v -> %v", in, out)
		}
	}

	// The width is checked here rather than baked into the column type,
	// because a column type is fixed at creation and the schema is
	// forward-only: a guessed width would be irreversible.
	if _, err := db.EncodeVector([]float32{1, 2, 3}); !errors.Is(err, store.ErrVectorDimension) {
		t.Fatalf("wrong-width embedding gave %v, want ErrVectorDimension", err)
	}
	if _, err := store.DecodeVector([]byte{1, 2, 3}); !errors.Is(err, store.ErrVectorDimension) {
		t.Fatalf("ragged blob gave %v, want ErrVectorDimension", err)
	}
	// A NULL embedding is what a row written during an embeddings outage
	// looks like. It must read as absent, not as an error that loses the
	// row.
	if v, err := store.DecodeVector(nil); err != nil || v != nil {
		t.Fatalf("DecodeVector(nil) = %v, %v", v, err)
	}
}

// TestVectorDimensionUnconfigured: with no embedding model configured there is
// no width to check against, and a caller that has vectors anyway is not
// wrong — it just has no declared width to violate.
func TestVectorDimensionUnconfigured(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "v0.db"),
		store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if db.EmbeddingDim() != 0 {
		t.Fatalf("EmbeddingDim = %d, want 0", db.EmbeddingDim())
	}
	if _, err := db.EncodeVector([]float32{1, 2, 3}); err != nil {
		t.Fatalf("encode without a configured width: %v", err)
	}
}
