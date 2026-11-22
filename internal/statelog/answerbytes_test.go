package statelog_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A READ STOPS BEFORE THE ROW THAT WOULD TAKE IT PAST ITS BOUND, KEEPS ITS FIRST
// ROW WHATEVER ITS SIZE, AND SAYS IT WAS CUT: a page past the bound is a reply
// the transport refuses, a page holding nothing is a cursor that never moves,
// and a cut nobody hears is a short page read as the whole answer. Once a row is
// refused every later one is, so the kept rows are a prefix a cursor resumes
// after.
//
// Mutations, each red: take a row that lands exactly on the bound as past it;
// drop the first-row exemption; take a row after one was refused; never set
// Cut; count the held bytes without the row just taken.
func TestARowReadStopsBeforeTheRowPastItsBoundKeepingTheFirst(t *testing.T) {
	t.Parallel()
	held := statelog.NewRowBytes(10)
	for i, step := range []struct {
		n    int
		kept bool
	}{{4, true}, {6, true}, {1, false}, {0, false}} {
		if got := held.Take(step.n); got != step.kept {
			t.Fatalf("row %d of %d bytes: kept %v, want %v", i, step.n, got, step.kept)
		}
	}
	if !held.Cut() || held.Held() != 10 {
		t.Fatalf("after a refusal: cut %v holding %d, want cut holding 10", held.Cut(), held.Held())
	}

	first := statelog.NewRowBytes(10)
	if !first.Take(100) || first.Cut() || first.Held() != 100 {
		t.Fatalf("a first row past the bound: cut %v holding %d, want it kept", first.Cut(), first.Held())
	}
	if first.Take(1) || !first.Cut() {
		t.Fatal("a second row after a first past the bound was kept")
	}

	var unset statelog.RowBytes
	if !unset.Take(1) || unset.Take(1) || !unset.Cut() {
		t.Fatal("an unset bound kept more than its first row")
	}
}

// A ROW SCANNED COLUMN BY COLUMN IS MEASURED AS THE WIRE CARRIES IT: its field
// names and quoting count, so a page of many small fields is not taken for a
// page of their values alone.
//
// Mutations, each red: measure only the strings a row holds; count an encoding
// failure as nothing.
func TestARowScannedByColumnIsMeasuredAsTheWireEncodesIt(t *testing.T) {
	t.Parallel()
	type row struct{ Name, Detail string }
	one := row{Name: "a", Detail: "b"} // {"Name":"a","Detail":"b"}: 25 bytes
	held := statelog.NewRowBytes(49)
	if !held.TakeEncoded(one) || held.Held() != 25 {
		t.Fatalf("an encoded row holds %d bytes, want the 25 its encoding is", held.Held())
	}
	if held.TakeEncoded(one) {
		t.Fatal("a second row past the bound by its encoding was kept")
	}
	if bad := statelog.NewRowBytes(10); !bad.TakeEncoded(func() {}) || bad.TakeEncoded(func() {}) {
		t.Fatal("an unencodable row after the first was kept")
	}
}

// AN ANSWER'S ROWS ARE HALF THE TRANSPORT'S MESSAGE, by default and at most: the
// other half carries what rides beside them and the seal. A bound the context
// carries holds only where it is smaller.
//
// Mutations, each red: default to the whole message; honour a context bound
// larger than AnswerBytes; ignore the context's bound.
func TestAnAnswerHoldsHalfTheMessageUnlessItsReadDeclaredLess(t *testing.T) {
	t.Parallel()
	if statelog.AnswerBytes != queue.MaxPayloadBytes/2 {
		t.Fatalf("AnswerBytes is %d, want half of the %d a message carries", statelog.AnswerBytes,
			queue.MaxPayloadBytes)
	}
	half := strings.Repeat("x", statelog.AnswerBytes/2)
	for _, tc := range []struct {
		name  string
		bound int
		rows  int
	}{
		{"no bound carried", 0, 2},
		{"a larger bound carried", 2 * statelog.AnswerBytes, 2},
		{"a smaller bound carried", statelog.AnswerBytes / 4, 1},
	} {
		ctx := t.Context()
		if tc.bound > 0 {
			ctx = statelog.WithAnswerBytes(ctx, tc.bound)
		}
		held, rows := statelog.RowsOf(ctx), 0
		for held.Take(len(half)) {
			rows++
		}
		if rows != tc.rows {
			t.Errorf("%s: kept %d rows of half the answer's bytes, want %d", tc.name, rows, tc.rows)
		}
	}
}
