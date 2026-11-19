package search

import (
	"slices"
	"testing"
)

// EVERY OPERATION AND EVERY SUBJECT KIND ABOVE THE BASE FORMAT HAS ITS ROW, AND
// NOTHING ELSE DOES.
//
// A record is written at the lowest version its table ([versionedFields]) says
// it needs, so a kind or operation with no row would be written at version one,
// which the builds before it READ, and refused by each as a writer fault and
// retried on every redelivery, where the rolling upgrade's contract is that they
// defer it. A base-format member names no row: one for an embed would hold every
// vector back from every peer on the previous version. The table is empty today;
// the first member a later build adds fails here until it has its row.
func TestEveryKindAboveTheBaseFormatHasItsRow(t *testing.T) {
	t.Parallel()
	base := struct {
		ops     []Op
		sources []Source
	}{
		ops:     []Op{OpEmbed, OpForget, OpCentroids, OpReassign, OpMeasure},
		sources: []Source{SourcePage, SourceTask, IndexSource},
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

	// AND A RECORD IS STAMPED BY IT: the base format's at one.
	for _, tc := range []struct {
		op     Op
		source Source
		want   int
	}{
		{OpEmbed, SourcePage, 1},
		{OpForget, SourceTask, 1},
		{OpCentroids, IndexSource, 1},
		{OpReassign, IndexSource, 1},
		{OpMeasure, IndexSource, 1},
	} {
		got, err := VectorRecord{RecordEnvelope: RecordEnvelope{
			Op: tc.op, Subject: Subject{Source: tc.source},
		}}.minimumVersion()
		if err != nil || got != tc.want {
			t.Errorf("a %s record on a %s is stamped %d (%v), want %d",
				tc.op, tc.source, got, err, tc.want)
		}
	}
}
