package search

import (
	"slices"
	"testing"
)

// EVERY OPERATION AND EVERY SUBJECT KIND ABOVE THE BASE FORMAT HAS ITS ROW, AND
// NOTHING ELSE DOES.
//
// A record is written at the lowest version its table says it needs
// ([versionedFields]). That is a mechanism only while the table covers every
// member: a kind added to [Sources] or an operation added to [Ops] with no row
// would be written at version one — a version the builds before it READ — and
// refused by every one of them as a writer fault, retried on every redelivery,
// where the rolling upgrade's contract is that they defer it. So every member
// outside the base format names a row by its value, and a member of the base
// format names none: a row for an embed would hold every vector this build
// computes back from every older peer.
func TestEveryKindAboveTheBaseFormatHasItsRow(t *testing.T) {
	t.Parallel()
	base := struct {
		ops     []Op
		sources []Source
	}{
		ops:     []Op{OpEmbed, OpForget},
		sources: []Source{SourcePage, SourceTask},
	}
	rows := map[string]bool{}
	for _, field := range versionedFields {
		rows[field.Equals] = true
	}
	for _, op := range Ops {
		_, named := rows[string(op)]
		if slices.Contains(base.ops, op) == named {
			t.Errorf("operation %q: base format = %v and a row = %v — every "+
				"operation after the base format states its version, and no "+
				"base one does", op, slices.Contains(base.ops, op), named)
		}
	}
	for _, source := range append(append([]Source{}, Sources...), IndexSource) {
		_, named := rows[string(source)]
		if slices.Contains(base.sources, source) == named {
			t.Errorf("subject kind %q: base format = %v and a row = %v",
				source, slices.Contains(base.sources, source), named)
		}
	}

	// AND A RECORD IS STAMPED BY IT: the base format's at one, the index's at
	// the version this build reads.
	for _, tc := range []struct {
		op     Op
		source Source
		want   int
	}{
		{OpEmbed, SourcePage, 1},
		{OpForget, SourceTask, 1},
		{OpCentroids, IndexSource, RecordVersion},
		{OpReassign, IndexSource, RecordVersion},
		{OpMeasure, IndexSource, RecordVersion},
	} {
		got, err := VectorRecord{RecordEnvelope: RecordEnvelope{
			Op: tc.op, Subject: Subject{Source: tc.source},
		}}.minimumVersion()
		if err != nil || got != tc.want {
			t.Errorf("a %s record on a %s is stamped %d (%v), want %d",
				tc.op, tc.source, got, err, tc.want)
		}
	}
	if got := IndexRecordVersion(); got != RecordVersion {
		t.Errorf("the index's records need version %d to be read, and this "+
			"build reads %d", got, RecordVersion)
	}
}
