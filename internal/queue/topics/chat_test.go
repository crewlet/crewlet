package topics_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE SUBJECT AND ITS INVERSE AGREE ON EVERY SHAPE THE CHAT LOG CARRIES.
//
// The same invariant the tracker's and the wiki's carry. Chat's ids are a
// channel's uuid, a 16-byte name token and the empty id the barrier uses, and
// the message kind's id is the CHANNEL's — which is the shape a reader is
// most likely to mistake for the message's own.
func TestAChatSubjectRoundTripsThroughItsInverse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ kind, id string }{
		"a channel":                    {"channel", "c1c2c3c4-0000-4000-8000-000000000001"},
		"a name claim, a hex token":    {"channelname", "9f86d081884c7d65" + "9a2feaa0c55ad015"},
		"a message, on its channel":    {"message", "c1c2c3c4-0000-4000-8000-000000000001"},
		"an eviction, on a node":       {"eviction", "node-a"},
		"a generation":                 {"generation", "CREWLET_CHAT_LOG"},
		"the barrier, which has no id": {"barrier", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			subject := topics.ChatLogSubject(tc.kind, tc.id)
			if !strings.HasPrefix(subject, topics.ChatLogPrefix+".") {
				t.Fatalf("%q is outside the log's own subject space", subject)
			}
			kind, id, ok := topics.ChatLogPath(subject)
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

// A CHAT SUBJECT WITH NO KIND IS NOT PUBLISHABLE, for the tracker's reason: it
// would be a real subject inside the wildcard that no applier arm covers. Nor
// is a DOTTED kind, which the inverse would read back as a shorter kind and an
// id nobody wrote — the shared grammar refuses it, and chat's builder is that
// grammar.
func TestAChatSubjectWithNoKindIsRefusedRatherThanBuilt(t *testing.T) {
	t.Parallel()
	if got := topics.ChatLogSubject("", "anything"); got != "" {
		t.Fatalf("a kindless subject built as %q", got)
	}
	if got := topics.ChatLogSubject("message.shadow", "c1"); got != "" {
		t.Fatalf("a dotted kind built as %q", got)
	}
}

// THE INVERSE REFUSES WHAT THE BUILDER COULD NOT HAVE PRODUCED, including the
// other four domains' subjects and chat's OWN presence space.
func TestTheChatPathRefusesWhatItDidNotBuild(t *testing.T) {
	t.Parallel()
	for _, subject := range []string{
		"",
		"crewlet.agent.alice.inbox",
		topics.ChatLogPrefix,
		topics.ChatLogPrefix + ".",
		topics.ChatLogPrefix + ".message.",
		topics.ChatPresence,
		topics.ChatPerson,
		topics.TrackerLogPrefix + ".task.1",
		topics.TrackerVectorsPrefix + ".source.1",
		topics.PagesLogPrefix + ".page.1",
		topics.UsageLogPrefix + ".seat.node-a.2026-09-23.a1",
	} {
		if kind, id, ok := topics.ChatLogPath(subject); ok {
			t.Errorf("%q was read as kind %q id %q and is not a subject on the "+
				"chat log", subject, kind, id)
		}
	}
}

// THE CHAT WILDCARD COVERS THE GRAMMAR IT IS CREATED FOR.
func TestTheChatWildcardCoversItsOwnGrammar(t *testing.T) {
	t.Parallel()
	prefix, ok := strings.CutSuffix(topics.ChatLogWildcard, ">")
	if !ok {
		t.Fatalf("the wildcard %q is not a wildcard", topics.ChatLogWildcard)
	}
	if prefix != topics.ChatLogPrefix+"." {
		t.Fatalf("the wildcard covers %q and subjects are built under %q",
			prefix, topics.ChatLogPrefix+".")
	}
}

// THE TWO SCATTER SUBJECTS ARE OUTSIDE THE LOG'S WILDCARD.
//
// This is the assertion that keeps typing and cursor flushes off the disk. A
// presence or person subject inside `crewlet.chat.log.>` would be captured by
// a durable, strictly ordered, identity-claiming stream, where every probe and
// every push is an undecodable record — the one failure that stops an applier
// and, past the deferral grace, sheds the node's seats.
func TestTheScatterSubjectsAreOutsideTheChatLogsWildcard(t *testing.T) {
	t.Parallel()
	for name, subject := range map[string]string{
		"presence": topics.ChatPresence,
		"person":   topics.ChatPerson,
	} {
		if strings.HasPrefix(subject, topics.ChatLogPrefix+".") {
			t.Errorf("the %s subject %q sits inside the chat log's wildcard %q",
				name, subject, topics.ChatLogWildcard)
		}
		if _, _, ok := topics.ChatLogPath(subject); ok {
			t.Errorf("the %s subject %q parses as a record on the chat log", name, subject)
		}
	}
	if topics.ChatPerson == topics.ChatPresence {
		t.Errorf("a person push and a presence probe share %q, so each node's "+
			"answerer would be handed the other's payload", topics.ChatPerson)
	}
}
