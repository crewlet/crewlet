package topics_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE SUBJECT AND ITS INVERSE AGREE ON EVERY SHAPE THE PAGES LOG CARRIES.
//
// Same invariant as the tracker's, and a second domain is exactly when it
// stops being obvious: the composed ids here are a revision's "<page>.<n>", a
// comment's "<page>.<comment>" and a title claim's "<CONTAINER>.<title>",
// which is the one whose tail is neither a number nor a uuid.
func TestAPagesSubjectRoundTripsThroughItsInverse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ kind, id string }{
		"a container":                    {"container", "ENG"},
		"a page":                         {"page", "b1b2b3b4-0000-4000-8000-000000000001"},
		"a revision, whose id has a dot": {"revision", "b1b2b3b4-0000-4000-8000-000000000001.7"},
		"a comment, likewise":            {"comment", "b1b2b3b4-0000-4000-8000-000000000001.c9"},
		"a title claim, whose tail is prose": {"title",
			"ENG.deploy-runbook"},
		"the barrier, which has no id": {"barrier", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			subject := topics.PagesLogSubject(tc.kind, tc.id)
			if !strings.HasPrefix(subject, topics.PagesLogPrefix+".") {
				t.Fatalf("%q is outside the log's own subject space", subject)
			}
			kind, id, ok := topics.PagesLogPath(subject)
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

// A PAGES SUBJECT WITH NO KIND IS NOT PUBLISHABLE, for the tracker's reason.
func TestAPagesSubjectWithNoKindIsRefusedRatherThanBuilt(t *testing.T) {
	t.Parallel()
	if got := topics.PagesLogSubject("", "anything"); got != "" {
		t.Fatalf("a kindless subject built as %q", got)
	}
}

// THE INVERSE REFUSES WHAT THE BUILDER COULD NOT HAVE PRODUCED — and, with
// three domains on one broker, that now includes the other two's subjects.
func TestThePagesPathRefusesWhatItDidNotBuild(t *testing.T) {
	t.Parallel()
	for _, subject := range []string{
		"",
		"crewlet.agent.alice.inbox",
		topics.PagesLogPrefix,
		topics.PagesLogPrefix + ".",
		topics.PagesLogPrefix + ".page.",
		topics.TrackerLogPrefix + ".task.1",
		topics.TrackerVectorsPrefix + ".source.1",
	} {
		if kind, id, ok := topics.PagesLogPath(subject); ok {
			t.Errorf("%q was read as kind %q id %q and is not a subject on the "+
				"pages log", subject, kind, id)
		}
	}
}

// THE THREE DOMAINS' SUBJECT SPACES ARE DISJOINT.
//
// They share one broker, and a stream created over a wildcard that overlapped
// another's would take deliveries meant for it — silently, because both
// records decode as bytes and only the applier's dispatch would notice.
func TestNoDomainsSubjectSpaceOverlapsAnothers(t *testing.T) {
	t.Parallel()
	prefixes := map[string]string{
		"tracker": topics.TrackerLogPrefix + ".",
		"vectors": topics.TrackerVectorsPrefix + ".",
		"pages":   topics.PagesLogPrefix + ".",
	}
	for a, pa := range prefixes {
		for b, pb := range prefixes {
			if a == b {
				continue
			}
			if strings.HasPrefix(pa, pb) {
				t.Errorf("%s's subject space %q sits inside %s's %q — one "+
					"stream's wildcard would capture the other's records",
					a, pa, b, pb)
			}
		}
	}
}

// THE PAGES WILDCARD COVERS THE GRAMMAR IT IS CREATED FOR.
func TestThePagesWildcardCoversItsOwnGrammar(t *testing.T) {
	t.Parallel()
	prefix, ok := strings.CutSuffix(topics.PagesLogWildcard, ">")
	if !ok {
		t.Fatalf("the wildcard %q is not a wildcard", topics.PagesLogWildcard)
	}
	if prefix != topics.PagesLogPrefix+"." {
		t.Fatalf("the wildcard covers %q and subjects are built under %q",
			prefix, topics.PagesLogPrefix+".")
	}
}
