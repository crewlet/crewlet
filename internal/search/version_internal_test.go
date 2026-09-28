package search

import (
	"testing"
)

// EVERY OPERATION AND EVERY SUBJECT KIND STATES THE VERSION IT WAS INTRODUCED
// AT, and the version this build reads is the highest of them.
//
// A record is written at the higher of its operation's and its kind's
// versions ([VersionOf]). That is a mechanism only while the table covers
// every member: a kind added to [Sources] with no row here would be written at
// a version the builds before it READ — and refused by every one of them as a
// writer fault, retried on every redelivery, where the rolling upgrade's
// contract is that they defer it. So the table must name exactly the members
// of [Ops], [Sources] and [IndexSource], and [RecordVersion] must be its
// maximum: a kind introduced above it would be written at a version this very
// build refuses to read.
func TestEveryKindStatesTheVersionItWasIntroducedAt(t *testing.T) {
	t.Parallel()
	highest := 0
	for _, op := range Ops {
		v, ok := kindVersions.ops[op]
		if !ok {
			t.Errorf("operation %q has no introduction version — decide it in "+
				"kindVersions, above every build that cannot read it", op)
		}
		highest = max(highest, v)
	}
	for _, source := range append(append([]Source{}, Sources...), IndexSource) {
		v, ok := kindVersions.sources[source]
		if !ok {
			t.Errorf("subject kind %q has no introduction version — decide it in "+
				"kindVersions, above every build that cannot read it", source)
		}
		highest = max(highest, v)
	}
	if len(kindVersions.ops) != len(Ops) {
		t.Errorf("kindVersions names %d operations and this build writes %d",
			len(kindVersions.ops), len(Ops))
	}
	if len(kindVersions.sources) != len(Sources)+1 {
		t.Errorf("kindVersions names %d subject kinds and this build writes %d",
			len(kindVersions.sources), len(Sources)+1)
	}
	if highest != RecordVersion {
		t.Errorf("the highest introduction version is %d and this build reads %d "+
			"— a kind above it is one this build writes and cannot read, and "+
			"one below it leaves a version nothing was introduced at", highest,
			RecordVersion)
	}
	// AND A KIND THIS BUILD HAS NOT HEARD OF IS ABOVE EVERYTHING IT READS, so
	// a record naming one is retained rather than refused.
	if got := VersionOf(OpEmbed, Subject{Source: "file", ID: "f"}); got <= RecordVersion {
		t.Errorf("an embed of an unknown kind is version %d, which this build "+
			"reads", got)
	}
}
