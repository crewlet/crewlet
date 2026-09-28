package topics_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// everyLogPrefix is a spread of the prefixes the grammar produces: layout 0's
// three logs, and partitioned logs that differ from one another in exactly one
// part each — the layout, the space, the index and the domain.
func everyLogPrefix() []string {
	return []string{
		topics.TrackerLogPrefix,
		topics.TrackerVectorsPrefix,
		topics.PagesLogPrefix,
		topics.PartitionLogPrefix(1, "tracker", 7, "tracker"),
		topics.PartitionLogPrefix(1, "tracker", 7, "vectors"),
		topics.PartitionLogPrefix(1, "tracker", 70, "tracker"),
		topics.PartitionLogPrefix(1, "pages", 7, "tracker"),
		topics.PartitionLogPrefix(2, "tracker", 7, "tracker"),
		topics.PartitionLogPrefix(1, "company", 0, "tracker"),
	}
}

// A RECORD'S SUBJECT ROUND-TRIPS ON EVERY LOG, AND IS READ BY NO OTHER.
//
// The builder is what a publisher writes and the parser is what the wake filter
// and the applier's dispatch read a kind out of. On a partitioned layout a
// domain has one log per partition, so the second half matters as much as the
// first: a subject that parsed under a sibling partition's prefix would be a
// record one partition's applier dispatched as its own — two logs claiming one
// object, which is the one thing a partition exists to rule out.
func TestALogSubjectRoundTripsOnItsOwnLogAndOnNoOther(t *testing.T) {
	t.Parallel()
	prefixes := everyLogPrefix()
	for i, prefix := range prefixes {
		if prefix == "" {
			t.Fatalf("prefix %d is empty: the grammar refused a log this case "+
				"needs, so every assertion below would be about nothing", i)
		}
		for _, tc := range []struct{ kind, id string }{
			{"task", "b1b2b3b4-0000-4000-8000-000000000001"},
			{"alias", "ENG-142.2"},
			{"barrier", ""},
		} {
			subject := topics.LogSubject(prefix, tc.kind, tc.id)
			kind, id, ok := topics.LogPath(prefix, subject)
			if !ok || kind != tc.kind || id != tc.id {
				t.Errorf("%q under %q parses back as (%q, %q, %v), want (%q, %q)",
					subject, prefix, kind, id, ok, tc.kind, tc.id)
			}
			for j, other := range prefixes {
				if j == i {
					continue
				}
				if kind, id, ok := topics.LogPath(other, subject); ok {
					t.Errorf("%q, built on %q, is read by the log under %q as "+
						"(%q, %q)", subject, prefix, other, kind, id)
				}
			}
		}
	}
}

// LAYOUT 0'S BUILDERS ARE THIS GRAMMAR OVER LAYOUT 0'S PREFIXES, BYTE FOR BYTE.
//
// A running fleet's anchors are keyed by these subjects and its streams hold
// records under them, so the prefix-relative grammar must name every layout-0
// subject exactly as the domain's own builder always has.
func TestLayoutZerosBuildersAreThePrefixRelativeGrammar(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, id string }{
		{"task", "b1b2b3b4-0000-4000-8000-000000000001"},
		{"revision", "b1b2b3b4-0000-4000-8000-000000000001.7"},
		{"barrier", ""},
		{"", "orphan"},
		{"two.words", "x"},
	} {
		if got, want := topics.TrackerLogSubject(tc.kind, tc.id),
			topics.LogSubject(topics.TrackerLogPrefix, tc.kind, tc.id); got != want {
			t.Errorf("the tracker names (%q, %q) %q and the grammar %q", tc.kind, tc.id, got, want)
		}
		if got, want := topics.PagesLogSubject(tc.kind, tc.id),
			topics.LogSubject(topics.PagesLogPrefix, tc.kind, tc.id); got != want {
			t.Errorf("the pages log names (%q, %q) %q and the grammar %q", tc.kind, tc.id, got, want)
		}
	}
}

// WHAT THE GRAMMAR CANNOT RECOVER IT DOES NOT BUILD.
//
// No prefix is the grammar's "no such stream", so a subject under it would be a
// path in no log's space. A kind holding a dot would parse back as a shorter
// kind and an id nobody wrote, so the inverse would name another object. Each
// is refused rather than built.
func TestALogSubjectRefusesWhatItsInverseCouldNotRecover(t *testing.T) {
	t.Parallel()
	prefix := topics.PartitionLogPrefix(1, "tracker", 7, "tracker")
	for _, tc := range []struct{ name, prefix, kind, id string }{
		{"no prefix", "", "task", "x"},
		{"no kind", prefix, "", "x"},
		{"a dotted kind", prefix, "task.shadow", "x"},
	} {
		if got := topics.LogSubject(tc.prefix, tc.kind, tc.id); got != "" {
			t.Errorf("%s: built %q", tc.name, got)
		}
	}
	for _, subject := range []string{
		"",
		prefix,
		prefix + ".",
		prefix + ".task.",
		topics.TrackerLogPrefix + ".task.x",
	} {
		if kind, id, ok := topics.LogPath(prefix, subject); ok {
			t.Errorf("%q was read under %q as (%q, %q)", subject, prefix, kind, id)
		}
	}
	if kind, id, ok := topics.LogPath("", "task.x"); ok {
		t.Errorf("a subject was read under no prefix as (%q, %q)", kind, id)
	}
}
