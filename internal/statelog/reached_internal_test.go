package statelog

import "testing"

// THE REACHED POSITION PASSES NOTHING THAT WRITES A ROW, AND NOTHING IT HAS NOT
// BEEN HANDED IN ORDER.
//
// A reader is released the moment the position passes a record, before any
// transaction — which is right for a barrier this node authenticated, since its
// transaction is the checkpoint and nothing else, and wrong for every record
// whose transaction writes a row: a read released over it is served from rows
// without it. So each case is a record one term of [writesNoRow] or the walk's
// own order is there to stop, put next in the log after the position, with a
// barrier the position could pass behind it — and the position must stop in
// front of the record, never pass it to that barrier.
func TestTheReachedPositionPassesNothingThatWritesARow(t *testing.T) {
	t.Parallel()
	at := func(seq uint64) Position { return Position{Stream: "s", Generation: 1, Seq: seq} }
	barrier := func(seq uint64) Record {
		return Record{
			Envelope: Envelope{
				V: BarrierVersion, Kind: BarrierKind, Subject: Subject{Kind: BarrierKind},
				Gen: 1, Scope: ScopeSet{Paths: []string{FrameworkScope}},
			},
			Position: at(seq),
			verdict:  Verified,
		}
	}
	// as is barrier(seq) with one thing about it changed.
	as := func(seq uint64, change func(*Record)) Record {
		rec := barrier(seq)
		change(&rec)
		return rec
	}
	arbitrated := []string{"object"}
	from := at(3)

	cases := []struct {
		name string
		run  []Record
		want uint64
	}{
		{
			name: "every barrier next in the log is passed",
			run:  []Record{barrier(4), barrier(5), barrier(6)},
			want: 6,
		},
		{
			name: "a redelivery at or below the position is stepped over, not stopped at",
			run:  []Record{barrier(2), barrier(4), barrier(3), barrier(5)},
			want: 5,
		},
		{
			name: "a record that writes rows",
			run: []Record{as(4, func(r *Record) {
				r.Kind, r.Subject = "edit", Subject{Kind: "object", ID: "o4"}
				r.Scope = ScopeSet{Paths: []string{"object/o4"}}
			}), barrier(5)},
			want: 3,
		},
		{
			name: "the trimmed record, which removes anchors in its own transaction",
			run: []Record{as(4, func(r *Record) {
				r.Kind, r.Subject = TrimmedKind, Subject{Kind: TrimmedKind, ID: "1.2"}
			}), barrier(5)},
			want: 3,
		},
		{
			name: "a barrier under a key this node does not hold, which it retains",
			run:  []Record{as(4, func(r *Record) { r.verdict = KeyUnknown }), barrier(5)},
			want: 3,
		},
		{
			name: "a barrier at a version this build may not read, which it retains",
			run:  []Record{as(4, func(r *Record) { r.V = BarrierVersion + 1 }), barrier(5)},
			want: 3,
		},
		{
			name: "a barrier claiming an object's scope",
			run: []Record{as(4, func(r *Record) {
				r.Scope = ScopeSet{Paths: []string{"object/o4"}}
			}), barrier(5)},
			want: 3,
		},
		{
			name: "a barrier on a subject the log arbitrates, which advances an anchor",
			run: []Record{as(4, func(r *Record) {
				r.Subject = Subject{Kind: "object", ID: "o4"}
			}), barrier(5)},
			want: 3,
		},
		{
			name: "a record above a hole",
			run:  []Record{barrier(5), barrier(6)},
			want: 3,
		},
		{
			name: "a record in a generation the position is not in",
			run: []Record{as(4, func(r *Record) {
				r.Position = Position{Stream: "s", Generation: 2, Seq: 4}
			}), barrier(5)},
			want: 3,
		},
		{
			name: "a barrier behind a record that writes rows waits for that record",
			run: []Record{barrier(4), as(5, func(r *Record) {
				r.Kind, r.Subject = "edit", Subject{Kind: "object", ID: "o5"}
				r.Scope = ScopeSet{Paths: []string{"object/o5"}}
			}), barrier(6)},
			want: 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := reachedOver(from, tc.run, arbitrated)
			if got != at(tc.want) {
				t.Errorf("from %s over %d record(s) the position reached %s, want %s — "+
					"a read released past a record that writes a row is served from "+
					"rows without it", from, len(tc.run), got, at(tc.want))
			}
		})
	}
}
