package topics_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// everyLogPrefix is every state log's subject prefix.
func everyLogPrefix() []string {
	return []string{
		topics.TrackerLogPrefix,
		topics.TrackerVectorsPrefix,
		topics.PagesLogPrefix,
		topics.UsageLogPrefix,
	}
}

// A RECORD'S SUBJECT ROUND-TRIPS ON EVERY LOG, AND IS READ BY NO OTHER.
//
// The builder is what a publisher writes and the parser is what the wake filter
// and the applier's dispatch read a kind out of. The second half matters as
// much as the first, and is the one a shared head makes easy to break: the
// tracker's mutation log and its vectors share `crewlet.tracker.`, so a parser
// that matched on a looser prefix would dispatch one domain's record as the
// other's.
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

// THE LOGS' OWN BUILDERS ARE THIS GRAMMAR OVER THEIR PREFIXES, BYTE FOR BYTE.
//
// A running fleet's anchors are keyed by these subjects and its streams hold
// records under them, so the prefix-relative grammar must name every subject
// exactly as the domain's own builder always has.
func TestTheLogsBuildersAreThePrefixRelativeGrammar(t *testing.T) {
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
	prefix := topics.TrackerLogPrefix
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
		// The vectors share the tracker's head and are another log.
		topics.TrackerVectorsPrefix + ".task.x",
		topics.PagesLogPrefix + ".task.x",
	} {
		if kind, id, ok := topics.LogPath(prefix, subject); ok {
			t.Errorf("%q was read under %q as (%q, %q)", subject, prefix, kind, id)
		}
	}
	if kind, id, ok := topics.LogPath("", "task.x"); ok {
		t.Errorf("a subject was read under no prefix as (%q, %q)", kind, id)
	}
}

// EACH STATE LOG IS NAMED AS IT HAS ALWAYS BEEN.
//
// A stream's name, its subjects' prefix and the wildcard it is created over
// are durable in every place a fleet keys on them — the stream on the broker,
// every node's durable consumer, the checkpoint and anchor rows, a hold, a
// backup's points — and each domain declares its stream from these constants.
// Pinned against LITERALS, because a constant held only to another constant
// moves with it: a rename here provisions a second, empty stream beside the
// real one and reads a company with nothing in it.
func TestEachStateLogIsNamedAsItAlwaysHasBeen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		log                      string
		stream, prefix, wildcard string
		wantStream, wantPrefix   string
	}{
		{"tracker", topics.TrackerLogStream, topics.TrackerLogPrefix, topics.TrackerLogWildcard,
			"CREWLET_TRACKER_LOG", "crewlet.tracker.log"},
		{"vectors", topics.TrackerVectorsStream, topics.TrackerVectorsPrefix, topics.TrackerVectorsWildcard,
			"CREWLET_TRACKER_VECTORS", "crewlet.tracker.vectors"},
		{"pages", topics.PagesLogStream, topics.PagesLogPrefix, topics.PagesLogWildcard,
			"CREWLET_PAGES_LOG", "crewlet.pages.log"},
		{"usage", topics.UsageLogStream, topics.UsageLogPrefix, topics.UsageLogWildcard,
			"CREWLET_USAGE_LOG", "crewlet.usage.log"},
	} {
		if tc.stream != tc.wantStream || tc.prefix != tc.wantPrefix ||
			tc.wildcard != tc.wantPrefix+".>" {
			t.Errorf("the %s log is (%q, %q, %q), and every fleet holds it as (%q, %q, %q)",
				tc.log, tc.stream, tc.prefix, tc.wildcard,
				tc.wantStream, tc.wantPrefix, tc.wantPrefix+".>")
		}
	}
}
