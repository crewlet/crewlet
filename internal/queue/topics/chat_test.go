package topics_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE SUBJECT AND ITS INVERSE AGREE ON EVERY SHAPE THE CHAT LOG CARRIES.
//
// The same invariant every other domain's grammar carries. Chat's ids are a
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

// THE INVERSE REFUSES WHAT THE BUILDER COULD NOT HAVE PRODUCED, including
// every other log's records and a chat room's presence subject.
func TestTheChatPathRefusesWhatItDidNotBuild(t *testing.T) {
	t.Parallel()
	subjects := []string{
		"",
		"crewlet.agent.alice.inbox",
		topics.ChatLogPrefix,
		topics.ChatLogPrefix + ".",
		topics.ChatLogPrefix + ".message.",
		topics.PresenceSubject("chat", uuid.New()),
	}
	// Every other log, read off the one list of them rather than written out
	// again — a list held to the package's own constants — so a ninth log is
	// refused here the day it is declared.
	for _, prefix := range everyLogPrefix() {
		if prefix != topics.ChatLogPrefix {
			subjects = append(subjects, topics.LogSubject(prefix, "message", "c1"))
		}
	}
	for _, subject := range subjects {
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
