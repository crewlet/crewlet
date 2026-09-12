package topics_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE SUBJECT AND ITS INVERSE AGREE ON EVERY SHAPE THE LOG CARRIES.
//
// The parser is what the wake filter and the applier's dispatch read a kind
// out of, and the builder is what the publisher writes. A disagreement
// between them is a record published into a subject the filter covers and the
// applier has no case for — which raises nothing on either side.
func TestATrackerSubjectRoundTripsThroughItsInverse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ kind, id string }{
		"a task":                       {"task", "b1b2b3b4-0000-4000-8000-000000000001"},
		"a project":                    {"project", "ENG"},
		"a sprint, whose id has a dot": {"sprint", "ENG.7"},
		"an alias claim, likewise":     {"alias", "ENG-142.2"},
		"a catalogue leaf":             {"catalogue", "fields"},
		"the barrier, which has no id": {"barrier", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			subject := topics.TrackerLogSubject(tc.kind, tc.id)
			if !strings.HasPrefix(subject, topics.TrackerLogPrefix+".") {
				t.Fatalf("%q is outside the log's own subject space", subject)
			}
			kind, id, ok := topics.TrackerLogPath(subject)
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

// A SUBJECT WITH NO KIND IS NOT PUBLISHABLE.
//
// It would be the prefix itself: a real subject inside the stream's wildcard
// that the applier's switch has no case for, so the record lands, is
// delivered, and matches nothing.
func TestASubjectWithNoKindIsRefusedRatherThanBuilt(t *testing.T) {
	t.Parallel()
	if got := topics.TrackerLogSubject("", "anything"); got != "" {
		t.Fatalf("a kindless subject built as %q", got)
	}
}

// THE INVERSE REFUSES WHAT THE BUILDER COULD NOT HAVE PRODUCED.
//
// A false identification is worse than none: the caller is the applier's
// dispatch or the wake filter, and both would then act on a kind that is not
// there.
func TestTheTrackerPathRefusesWhatItDidNotBuild(t *testing.T) {
	t.Parallel()
	for _, subject := range []string{
		"",
		"crewlet.agent.alice.inbox",
		topics.TrackerLogPrefix,
		topics.TrackerLogPrefix + ".",
		topics.TrackerLogPrefix + ".task.",
		topics.TrackerVectorsPrefix + ".source.1",
	} {
		if kind, id, ok := topics.TrackerLogPath(subject); ok {
			t.Errorf("%q was read as kind %q id %q and is not a subject on the "+
				"mutation log", subject, kind, id)
		}
	}
}

// THE STREAM'S WILDCARD COVERS EVERY SUBJECT THE BUILDER CAN PRODUCE.
//
// The stream is created with the wildcard and nothing else, so a subject
// outside it is refused by the broker at publish — which is a loud failure
// rather than a silent one, and is still a company that cannot write.
func TestTheWildcardCoversTheGrammarItIsCreatedFor(t *testing.T) {
	t.Parallel()
	prefix, ok := strings.CutSuffix(topics.TrackerLogWildcard, ">")
	if !ok {
		t.Fatalf("the wildcard %q is not a wildcard", topics.TrackerLogWildcard)
	}
	if prefix != topics.TrackerLogPrefix+"." {
		t.Fatalf("the wildcard covers %q and subjects are built under %q",
			prefix, topics.TrackerLogPrefix+".")
	}
}
