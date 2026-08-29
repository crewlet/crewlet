package store

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// now is the clock every store row and every time-bounded query reads. One
// function so the read floor, the retention sweep and a row's own timestamp
// cannot disagree about what "now" is.
func now() time.Time { return time.Now().UTC() }

// EncodeTime converts an instant to the storage encoding: microseconds since
// the Unix epoch, UTC.
//
// Integers rather than ISO-8601 text because ordering is load-bearing here.
// Keyset paging compares (event_time, event_id) as a tuple, and the same
// instant written "…T12:00:00+00:00" sorts AFTER itself written naive — so a
// text column silently interleaves rows a reader is paging through. Integer
// microseconds is also exactly the resolution TIMESTAMPTZ carried, so the
// encoding trades no precision for that guarantee.
func EncodeTime(t time.Time) int64 { return t.UTC().UnixMicro() }

// DecodeTime converts the storage encoding back to an instant, always UTC.
func DecodeTime(micros int64) time.Time { return time.UnixMicro(micros).UTC() }

// NullText maps a Go string onto a nullable TEXT column, sending NULL for the
// zero value.
//
// This is the boundary half of the design in decisions/002: where the
// Postgres schema wrote `UNIQUE(a, b) WHERE b <> ”` to mean "an empty b is
// legitimately unconstrained", the schema here writes a PLAIN unique index
// over a nullable column and stores NULL for that case. SQL treats NULLs as
// distinct, so the semantics are identical — and a plain index is the only
// thing a bare `ON CONFLICT (a, b)` can target, which is what makes the whole
// advisory-lock-and-WHERE-NOT-EXISTS idiom unnecessary.
//
// Callers keep the Go zero value on both sides: "" goes in, "" comes back.
func NullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Text reads a nullable TEXT column back as a Go string, mapping NULL to "".
// The inverse of NullText; see there for why the mapping exists.
func Text(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// NullTime maps an instant onto a nullable timestamp column, sending NULL for
// the zero time. A zero time.Time is "no deadline / never happened", which is
// what NULL means in every nullable timestamp column in the schema — writing
// its epoch encoding instead would record the year 1 as a real instant.
func NullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return EncodeTime(t)
}

// TimeAt reads a nullable timestamp column back, mapping NULL to the zero
// time.
func TimeAt(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return DecodeTime(v.Int64)
}

// ErrVectorDimension reports an embedding whose width is not the one the
// active configuration declared.
var ErrVectorDimension = errors.New("store: embedding dimension mismatch")

// EncodeVector packs an embedding into the byte layout the vector columns
// hold: little-endian float32, no header — byte-identical to what Turso's
// vector32() produces, so a Go-written row and a SQL-written one are the same
// row and vector_distance_cos() reads either.
//
// The width is checked against the configured dimension rather than baked into
// the column type. A `vector(N)` column is fixed at creation and the schema is
// forward-only, so a width guessed at migrate time is irreversible: every
// later insert raises against the mismatched column, forever. That risk is
// what made the Postgres migrator a two-phase run; here a mismatch is one
// error on one insert.
func (d *DB) EncodeVector(v []float32) ([]byte, error) {
	if d.dim > 0 && len(v) != d.dim {
		return nil, fmt.Errorf("%w: got %d, configured %d",
			ErrVectorDimension, len(v), d.dim)
	}
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out, nil
}

// DecodeVector unpacks a stored embedding. A NULL column reads as nil, which
// is what a row written during an embeddings outage looks like — recall skips
// it, every other query still returns it.
func DecodeVector(b []byte) ([]float32, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("%w: %d bytes is not a whole number of float32s",
			ErrVectorDimension, len(b))
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out, nil
}
