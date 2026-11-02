package topics_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE SUBJECT AND ITS INVERSE AGREE ON EVERY SHAPE THE CODE LOG CARRIES.
//
// The pages log's invariant, on the grammar the code domain composes: a name
// claim's 32-hex token, a repository's UUIDv7, a node's id, a generation's
// decimal, the company's one subject and the barrier's empty id.
func TestACodeSubjectRoundTripsThroughItsInverse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ kind, id string }{
		"a name claim":                 {"name", "0f1e2d3c4b5a69788796a5b4c3d2e1f0"},
		"a repository":                 {"repo", "0190f3c4-5b6a-7c8d-9e0f-0123456789ab"},
		"an eviction":                  {"eviction", "node-a"},
		"a generation":                 {"generation", "2"},
		"the company's activation":     {"company", "activation"},
		"the barrier, which has no id": {"barrier", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			subject := topics.CodeLogSubject(tc.kind, tc.id)
			if !strings.HasPrefix(subject, topics.CodeLogPrefix+".") {
				t.Fatalf("%q is outside the log's own subject space", subject)
			}
			kind, id, ok := topics.CodeLogPath(subject)
			if !ok {
				t.Fatalf("%q is not recognised as a subject on this log", subject)
			}
			if kind != tc.kind || id != tc.id {
				t.Fatalf("%q parses back as (%q, %q), want (%q, %q)",
					subject, kind, id, tc.kind, tc.id)
			}
		})
	}
}

// A CODE SUBJECT WITH NO KIND IS NOT PUBLISHABLE, for the tracker's reason: it
// would address the log's own prefix, a real subject no applier has a case for.
func TestACodeSubjectWithNoKindIsRefusedRatherThanBuilt(t *testing.T) {
	t.Parallel()
	if got := topics.CodeLogSubject("", "anything"); got != "" {
		t.Fatalf("a kindless subject built as %q", got)
	}
}

// THE INVERSE REFUSES WHAT THE BUILDER COULD NOT HAVE PRODUCED — every other
// log's subjects among them, since every domain shares one broker.
func TestTheCodePathRefusesWhatItDidNotBuild(t *testing.T) {
	t.Parallel()
	subjects := []string{
		"",
		"crewlet.agent.alice.inbox",
		topics.CodeLogPrefix,
		topics.CodeLogPrefix + ".",
		topics.CodeLogPrefix + ".repo.",
	}
	// Every other log, read off the one list of them rather than written out
	// again — a list held to the package's own constants — so a log the
	// package gains is refused here the day it is declared, under the kind
	// this log's own records carry.
	for _, prefix := range everyLogPrefix() {
		if prefix != topics.CodeLogPrefix {
			subjects = append(subjects, topics.LogSubject(prefix, "repo", "1"))
		}
	}
	for _, subject := range subjects {
		if kind, id, ok := topics.CodeLogPath(subject); ok {
			t.Errorf("%q was read as kind %q id %q and is not a subject on the "+
				"code log", subject, kind, id)
		}
	}
}

// THE CODE STATUS SUBJECT IS OUTSIDE THE CODE LOG'S STREAM. A stream captures
// whatever is published under the subjects it is created with, a request
// included, and acknowledges it to the request's reply mailbox: a status
// question inside the log's space would be stored as a record no applier has
// a kind for, and answered by the stream's acknowledgement before any git
// node spoke. The name is written out once, since a constant-only assertion
// would pass with every constant renamed.
func TestTheCodeStatusSubjectIsOutsideTheCodeLog(t *testing.T) {
	t.Parallel()
	if topics.CodeStatus != "crewlet.code.status" {
		t.Fatalf("the code status subject is %q", topics.CodeStatus)
	}
	if topics.Match(topics.CodeLogWildcard, topics.CodeStatus) {
		t.Fatalf("the code log's stream, over %q, captures the status question %q",
			topics.CodeLogWildcard, topics.CodeStatus)
	}
}

// THE CODE WILDCARD COVERS THE GRAMMAR IT IS CREATED FOR.
func TestTheCodeWildcardCoversItsOwnGrammar(t *testing.T) {
	t.Parallel()
	prefix, ok := strings.CutSuffix(topics.CodeLogWildcard, ">")
	if !ok {
		t.Fatalf("the wildcard %q is not a wildcard", topics.CodeLogWildcard)
	}
	if prefix != topics.CodeLogPrefix+"." {
		t.Fatalf("the wildcard covers %q and subjects are built under %q",
			prefix, topics.CodeLogPrefix+".")
	}
}
