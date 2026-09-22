package topics_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE SUBJECT AND ITS INVERSE AGREE ON EVERY SHAPE THE IAM LOG CARRIES.
//
// Same invariant as the other four domains', and the shape that makes it worth
// restating for a fifth is the LOGIN: internal/iam REQUIRES a dot in a person's
// login, so `jane.doe` is not an occasional awkward id here, it is what every
// login claim's subject looks like. A parser splitting on every dot would
// recover `jane` — a claim on a different address, which somebody else may
// legitimately hold.
func TestAnIamSubjectRoundTripsThroughItsInverse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ kind, id string }{
		"a person, by the id nothing renames":  {"person", "018f3a9c-0000-7000-8000-000000000001"},
		"an email claim, by its keyed blind":   {"email", "9f8e7d6c5b4a39281706f5e4d3c2b1a0"},
		"a login claim, whose id carries dots": {"login", "jane.doe"},
		"a machine handle, which carries a colon": {"login",
			"ci:release"},
		"a seat binding":                      {"seat", "sarah-chen"},
		"a session lineage":                   {"session", "018f3a9c-0000-7000-8000-00000000abcd"},
		"a retention sweep, by its bucket":    {"sweep", "17"},
		"a reanchor, by its generation":       {"generation", "3"},
		"an eviction, by the node it gates":   {"eviction", "node-a.example"},
		"the bootstrap, which has no id":      {"bootstrap", ""},
		"the barrier, which has no id either": {"barrier", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			subject := topics.IamLogSubject(tc.kind, tc.id)
			if !strings.HasPrefix(subject, topics.IamLogPrefix+".") {
				t.Fatalf("%q is outside the log's own subject space", subject)
			}
			kind, id, ok := topics.IamLogPath(subject)
			if !ok {
				t.Fatalf("%q is not recognised as a subject on this log", subject)
			}
			if kind != tc.kind || id != tc.id {
				t.Fatalf("%q parses back as (%q, %q), want (%q, %q) — an id "+
					"carrying dots is one id, and a login carries one in every "+
					"case", subject, kind, id, tc.kind, tc.id)
			}
		})
	}
}

// AN IAM SUBJECT WITH NO KIND IS NOT PUBLISHABLE, for the tracker's reason: it
// would address the log's own prefix, which is a real subject inside the
// stream's wildcard that the applier's switch has no case for.
func TestAnIamSubjectWithNoKindIsRefusedRatherThanBuilt(t *testing.T) {
	t.Parallel()
	if got := topics.IamLogSubject("", "anything"); got != "" {
		t.Fatalf("a kindless subject built as %q", got)
	}
}

// THE INVERSE REFUSES WHAT THE BUILDER COULD NOT HAVE PRODUCED — and, with five
// domains on one broker, that includes the other four's subjects.
func TestTheIamPathRefusesWhatItDidNotBuild(t *testing.T) {
	t.Parallel()
	for _, subject := range []string{
		"",
		"crewlet.agent.alice.inbox",
		topics.IamLogPrefix,
		topics.IamLogPrefix + ".",
		topics.IamLogPrefix + ".person.",
		topics.TrackerLogPrefix + ".task.1",
		topics.TrackerVectorsPrefix + ".source.1",
		topics.PagesLogPrefix + ".page.1",
		topics.ChartLogPrefix + ".seat.1",
	} {
		if kind, id, ok := topics.IamLogPath(subject); ok {
			t.Errorf("%q was read as kind %q id %q and is not a subject on the "+
				"iam log", subject, kind, id)
		}
	}
}

// THE IAM WILDCARD COVERS THE GRAMMAR IT IS CREATED FOR.
func TestTheIamWildcardCoversItsOwnGrammar(t *testing.T) {
	t.Parallel()
	prefix, ok := strings.CutSuffix(topics.IamLogWildcard, ">")
	if !ok {
		t.Fatalf("the wildcard %q is not a wildcard", topics.IamLogWildcard)
	}
	if prefix != topics.IamLogPrefix+"." {
		t.Fatalf("the wildcard covers %q and subjects are built under %q",
			prefix, topics.IamLogPrefix+".")
	}
	// AND EVERY SHAPE THE BUILDER PRODUCES IS INSIDE IT, including the two
	// kinds with no id — which sit one token below the prefix rather than
	// two, and are the shapes a wildcard written as `prefix.*.>` would
	// silently exclude.
	for _, s := range []string{
		topics.IamLogSubject("bootstrap", ""),
		topics.IamLogSubject("barrier", ""),
		topics.IamLogSubject("login", "jane.doe"),
	} {
		if !strings.HasPrefix(s, prefix) {
			t.Errorf("%q is built outside the wildcard %q the stream is "+
				"created with — the record would be published to a subject no "+
				"consumer of this log covers", s, topics.IamLogWildcard)
		}
	}
}
