package topics_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE SUBJECT AND ITS INVERSE AGREE ON EVERY SHAPE THE CHART LOG CARRIES.
//
// Same invariant as the tracker's and the pages log's, and the shape that makes
// it worth restating for a fourth domain is the STRUCTURE: `…log.tree` is a
// kind with no id at all, and it is not the barrier — so "an empty id means the
// barrier" is an assumption this domain would have broken silently.
func TestAChartSubjectRoundTripsThroughItsInverse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ kind, id string }{
		"the structure, which has no id": {"tree", ""},
		"a unit":                         {"unit", "engineering"},
		"a seat":                         {"seat", "sarah-chen"},
		"a key claim":                    {"rekey", "platform"},
		"a key claim whose key has a dot": {"rekey",
			"eng.platform"},
		"the barrier, which has no id either": {"barrier", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			subject := topics.ChartLogSubject(tc.kind, tc.id)
			if !strings.HasPrefix(subject, topics.ChartLogPrefix+".") {
				t.Fatalf("%q is outside the log's own subject space", subject)
			}
			kind, id, ok := topics.ChartLogPath(subject)
			if !ok {
				t.Fatalf("%q is not recognised as a subject on this log", subject)
			}
			if kind != tc.kind || id != tc.id {
				t.Fatalf("%q parses back as (%q, %q), want (%q, %q) — an id "+
					"carrying dots is one id, and splitting on every dot loses "+
					"everything after the first", subject, kind, id, tc.kind, tc.id)
			}
		})
	}
}

// A CHART SUBJECT WITH NO KIND IS NOT PUBLISHABLE, for the tracker's reason: it
// would address the log's own prefix, which is a real subject inside the
// stream's wildcard that the applier's switch has no case for.
func TestAChartSubjectWithNoKindIsRefusedRatherThanBuilt(t *testing.T) {
	t.Parallel()
	if got := topics.ChartLogSubject("", "anything"); got != "" {
		t.Fatalf("a kindless subject built as %q", got)
	}
}

// THE INVERSE REFUSES WHAT THE BUILDER COULD NOT HAVE PRODUCED — and, with four
// domains on one broker, that includes the other three's subjects.
func TestTheChartPathRefusesWhatItDidNotBuild(t *testing.T) {
	t.Parallel()
	for _, subject := range []string{
		"",
		"crewlet.agent.alice.inbox",
		topics.ChartLogPrefix,
		topics.ChartLogPrefix + ".",
		topics.ChartLogPrefix + ".unit.",
		topics.TrackerLogPrefix + ".task.1",
		topics.TrackerVectorsPrefix + ".source.1",
		topics.PagesLogPrefix + ".page.1",
	} {
		if kind, id, ok := topics.ChartLogPath(subject); ok {
			t.Errorf("%q was read as kind %q id %q and is not a subject on the "+
				"chart log", subject, kind, id)
		}
	}
}

// THE CHART WILDCARD COVERS THE GRAMMAR IT IS CREATED FOR.
func TestTheChartWildcardCoversItsOwnGrammar(t *testing.T) {
	t.Parallel()
	prefix, ok := strings.CutSuffix(topics.ChartLogWildcard, ">")
	if !ok {
		t.Fatalf("the wildcard %q is not a wildcard", topics.ChartLogWildcard)
	}
	if prefix != topics.ChartLogPrefix+"." {
		t.Fatalf("the wildcard covers %q and subjects are built under %q",
			prefix, topics.ChartLogPrefix+".")
	}
	// AND EVERY SHAPE THE BUILDER PRODUCES IS INSIDE IT, including the two
	// kinds with no id — which sit one token below the prefix rather than
	// two, and are the shapes a wildcard written as `prefix.*.>` would
	// silently exclude.
	for _, s := range []string{
		topics.ChartLogSubject("tree", ""),
		topics.ChartLogSubject("barrier", ""),
		topics.ChartLogSubject("seat", "sarah-chen"),
	} {
		if !strings.HasPrefix(s, prefix) {
			t.Errorf("%q is built outside the wildcard %q the stream is "+
				"created with — the record would be published to a subject no "+
				"consumer of this log covers", s, topics.ChartLogWildcard)
		}
	}
}
