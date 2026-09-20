package notify_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
)

// fullMeta is one chat delivery carrying every key in the vocabulary, which
// is what lets a probe change exactly one of them.
func fullMeta() map[string]string {
	return map[string]string{
		notify.TransportField:    "native",
		notify.ChannelField:      "c-42",
		notify.ChannelTypeField:  "direct",
		notify.ChannelKindField:  "dm",
		notify.ChannelNameField:  "eng",
		notify.MessageIDField:    "m-7",
		notify.ThreadField:       "m-1",
		notify.ThreadAnchorField: "m-1",
		notify.UserField:         "u-ana",
		notify.ActorField:        "u-ana",
		notify.FollowReasonField: string(notify.FollowMention),
		notify.FollowingField:    string(notify.FollowMention),
		notify.RecipientField:    "swe",
		notify.KeyField:          "native:c-42:m-1",
		notify.ReplayedField:     "true",
	}
}

func metaPrompt() notify.ChatPrompt {
	return notify.ChatPrompt{Backend: "native", Label: "Native", Address: nativeRule}
}

// meta applies one change to the full fixture.
func meta(mutate func(map[string]string)) map[string]string {
	m := fullMeta()
	mutate(m)
	return m
}

func build(m map[string]string) string {
	return metaPrompt().Build(notify.Inbound{Source: "native", Metadata: m}, nil)
}

// EVERY CHAT METADATA KEY HAS A READER, and this is the check that keeps it
// true: a key a parser stamps that nothing reads is work that looks done,
// and it fails silently for ever because an unread key raises nothing.
//
// Each case changes ONE key and names the reader whose answer turns on it.
// The two keys with no reader in this package are listed below with the
// package that does read them — an exception a reader can check, rather than
// a gap nobody can see.
func TestEveryChatMetadataKeyHasAReader(t *testing.T) {
	t.Parallel()
	readElsewhere := map[string]string{
		notify.ChannelKindField: "internal/engine's turn-start prefetch, which " +
			"selects on the canonical surface shape",
		notify.KeyField: "the broker's partition function and the event store, " +
			"which read it off the envelope rather than out of this map",
	}
	cases := []struct {
		key    string
		reader string
		probe  func() bool // true when the reader noticed the change
	}{
		{notify.TransportField, "ConversationOf, which refuses a trigger from another backend", func() bool {
			_, ok := notify.ConversationOf(meta(func(m map[string]string) {
				m[notify.TransportField] = "other"
			}), "native")
			return !ok
		}},
		{notify.ChannelField, "ConversationOf, which has no conversation without one", func() bool {
			_, ok := notify.ConversationOf(meta(func(m map[string]string) {
				m[notify.ChannelField] = ""
			}), "native")
			return !ok
		}},
		{notify.ChannelTypeField, "AddressRule.IsDirect", func() bool {
			return !nativeRule.IsDirect(meta(func(m map[string]string) {
				m[notify.ChannelTypeField] = "room"
			}))
		}},
		{notify.ChannelNameField, "ChatPrompt.Build, which names the room a person would name", func() bool {
			return strings.Contains(build(fullMeta()), "**Channel:** eng (`c-42`)") &&
				!strings.Contains(build(meta(func(m map[string]string) {
					m[notify.ChannelNameField] = ""
				})), "**Channel:** eng")
		}},
		{notify.MessageIDField, "ChatPrompt.Build, which gives the agent the id to act on", func() bool {
			return !strings.Contains(build(meta(func(m map[string]string) {
				m[notify.MessageIDField] = ""
			})), "**Message id:**")
		}},
		{notify.ThreadField, "ChatPrompt.RequiresRecon: a thread reply is a pointer", func() bool {
			return !metaPrompt().RequiresRecon(notify.Inbound{Metadata: meta(func(m map[string]string) {
				m[notify.ThreadField] = ""
			})})
		}},
		{notify.ThreadAnchorField, "Anchor, which is where the reply and the indicator both go", func() bool {
			return metaPrompt().ConversationKey(meta(func(m map[string]string) {
				m[notify.ThreadAnchorField] = "m-99"
			}), "") == "c-42:m-99"
		}},
		{notify.UserField, "ChatPrompt.Build, which resolves the sender to a colleague", func() bool {
			return strings.Contains(build(fullMeta()), "**From:** u-ana") &&
				strings.Contains(build(meta(func(m map[string]string) {
					m[notify.UserField] = ""
				})), "**From:** unknown")
		}},
		{notify.ActorField, "ActorOf, behind the self-action guard", func() bool {
			return notify.ActorOf(meta(func(m map[string]string) {
				m[notify.ActorField] = "u-bo"
			})) == "u-bo"
		}},
		{notify.FollowReasonField, "AddressRule.Addressed: why this message reached the seat", func() bool {
			return !nativeRule.Addressed(meta(func(m map[string]string) {
				m[notify.ChannelTypeField] = "room"
				m[notify.FollowReasonField] = string(notify.FollowCollective)
				m[notify.FollowingField] = string(notify.FollowCollective)
			}))
		}},
		{notify.FollowingField, "AddressRule.Addressed: why the seat is in this thread", func() bool {
			return nativeRule.Addressed(meta(func(m map[string]string) {
				m[notify.ChannelTypeField] = "room"
				m[notify.FollowReasonField] = ""
				m[notify.FollowingField] = string(notify.FollowMention)
			}))
		}},
		{notify.RecipientField, "ChatPrompt.Build, which tells the agent its own handle", func() bool {
			return strings.Contains(build(meta(func(m map[string]string) {
				m[notify.RecipientField] = "qa"
			})), "Your handle is `qa`")
		}},
		{notify.ReplayedField, "ChatPrompt.Build, which says the conversation may have moved on", func() bool {
			return !strings.Contains(build(meta(func(m map[string]string) {
				m[notify.ReplayedField] = ""
			})), "re-read after a connection")
		}},
	}

	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.key] = true
		if !c.probe() {
			t.Errorf("%s: %s did not read it", c.key, c.reader)
		}
	}
	for _, key := range notify.ChatKeys() {
		if covered[key] || readElsewhere[key] != "" {
			continue
		}
		t.Errorf("%q is in the vocabulary with no reader: give it one, or name "+
			"the package that reads it in readElsewhere", key)
	}
}

// THE REQUIRED SET IS A SUBSET OF THE VOCABULARY, and the vocabulary names
// each key once. A duplicate would make a conformance check pass twice on
// one key and never notice the one it displaced.
func TestTheChatVocabularyIsOneSet(t *testing.T) {
	t.Parallel()
	keys := notify.ChatKeys()
	if len(slices.Compact(slices.Sorted(slices.Values(keys)))) != len(keys) {
		t.Fatalf("the vocabulary repeats a key: %v", keys)
	}
	for _, key := range notify.RequiredChatKeys() {
		if !slices.Contains(keys, key) {
			t.Errorf("%q is required of every parser but is not in the vocabulary", key)
		}
	}
	// The fixture above is the vocabulary, which is what makes the reader
	// probes exhaustive rather than a list somebody last updated once.
	fixture := slices.Sorted(maps.Keys(fullMeta()))
	if got := slices.Sorted(slices.Values(keys)); !slices.Equal(fixture, got) {
		t.Fatalf("the fixture carries %v, the vocabulary %v", fixture, got)
	}
}

// THE ANCHOR IS ONE DERIVATION. It had five — both parsers, the indicator's
// conversation, the coalescer's key and the trigger's reply target — and a
// disagreement between them does not fail: it puts the spinner in one place
// and the reply in another.
func TestTheAnchorIsOneDerivation(t *testing.T) {
	t.Parallel()
	// A producer that stated where a reply goes is believed, because it
	// is the only code that knows.
	stated := map[string]string{
		notify.ThreadAnchorField: "seq-9",
		notify.ThreadField:       "m-1",
		notify.MessageIDField:    "m-7",
	}
	if got := notify.Anchor(stated); got != "seq-9" {
		t.Errorf("a stated anchor resolved to %q", got)
	}
	// A thread reply anchors on its thread…
	reply := map[string]string{notify.ThreadField: "m-1", notify.MessageIDField: "m-7"}
	if got := notify.Anchor(reply); got != "m-1" {
		t.Errorf("a thread reply anchors on %q", got)
	}
	// …and a TOP-LEVEL message on its own id, which becomes the thread
	// the moment anybody answers under it.
	top := map[string]string{notify.MessageIDField: "m-7"}
	if got := notify.Anchor(top); got != "m-7" {
		t.Errorf("a top-level message anchors on %q", got)
	}
	if got := notify.Anchor(nil); got != "" {
		t.Errorf("an empty delivery anchors on %q", got)
	}
	// And the indicator and the trigger land in the same place.
	conv, ok := notify.ConversationOf(map[string]string{
		notify.TransportField: "native", notify.ChannelField: "c-42",
		notify.ThreadAnchorField: "seq-9", notify.MessageIDField: "m-7",
	}, "native")
	if !ok || conv.Thread != "seq-9" {
		t.Fatalf("the indicator sits in %+v", conv)
	}
}
