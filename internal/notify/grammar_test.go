package notify_test

import (
	"regexp"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
)

// The two real grammars, declared the way a vendor package will declare
// them, so these tests exercise the shapes that actually ship.
var (
	markup = notify.MarkupGrammar{
		Name:       "slack",
		User:       regexp.MustCompile(`<@([UW][A-Z0-9_]+)>`),
		Collective: regexp.MustCompile(`<!(?:channel|here)(?:\|[^>]*)?>`),
	}
	literal = notify.LiteralGrammar{
		Name:        "mattermost",
		Collectives: []string{"all", "channel", "here"},
	}
)

func TestMarkupMentionsAreExact(t *testing.T) {
	cases := []struct {
		text, self string
		want       notify.FollowReason
	}{
		{"hey <@U123> can you look", "U123", notify.FollowMention},
		{"hey <@U999> can you look", "U123", ""},
		// A mention of somebody else must not read as one of this seat,
		// which is what capturing the identity rather than searching for
		// a substring buys.
		{"cc <@U1234>", "U123", ""},
		{"<!channel> standup in five", "U123", notify.FollowCollective},
		{"<!here|here> anyone about?", "U123", notify.FollowCollective},
		// A personal mention outranks a collective in the same message.
		{"<!channel> and <@U123> specifically", "U123", notify.FollowMention},
		{"plain text with no mention", "U123", ""},
		// An unresolved identity falls through to the collective check
		// rather than matching everything: identities resolve against
		// the vendor at connect, and every message before that would
		// otherwise read as a personal mention of every seat.
		{"hey <@U123>", "", ""},
		{"<!here> hey <@U123>", "", notify.FollowCollective},
	}
	for _, c := range cases {
		got, ok := markup.Detect(c.text, c.self)
		if c.want == "" {
			if ok {
				t.Errorf("Detect(%q, %q) = %q, want no trigger", c.text, c.self, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("Detect(%q, %q) = %q/%v, want %q", c.text, c.self, got, ok, c.want)
		}
	}
}

func TestLiteralMentionsRespectWordBoundaries(t *testing.T) {
	cases := []struct {
		text, self string
		want       notify.FollowReason
	}{
		{"hey @agent-swe can you look", "agent-swe", notify.FollowMention},
		// Trailing sentence punctuation still names the seat.
		{"thanks @agent-swe!", "agent-swe", notify.FollowMention},
		{"thanks @agent-swe.", "agent-swe", notify.FollowMention},
		{"(@agent-swe)", "agent-swe", notify.FollowMention},
		{"@agent-swe", "agent-swe", notify.FollowMention},
		// A PREFIX must not match: on a backend where a seat is
		// addressed by username, that is one seat reading another's
		// mail. This is the case RE2's missing lookahead costs.
		{"hey @agent-swe2", "agent-swe", ""},
		{"hey @agent-swerve", "agent-swe", ""},
		{"hey @agent-swe.qa", "agent-swe", ""},
		// An email address is not a mention — the missing lookBEHIND.
		{"mail agent-swe@example.com", "example", ""},
		{"reach me at bo@agent-swe.com", "agent-swe", ""},
		// A doubled @ is already consumed by the first.
		{"@@agent-swe", "agent-swe", ""},
		// Case-insensitive, and a leading @ on the identity is
		// tolerated: a transport handing back "@name" is a spelling,
		// not a different seat.
		{"hey @Agent-SWE", "agent-swe", notify.FollowMention},
		{"hey @agent-swe", "@agent-swe", notify.FollowMention},
		{"@all standup", "agent-swe", notify.FollowCollective},
		{"@Channel please read", "agent-swe", notify.FollowCollective},
		{"@here now", "agent-swe", notify.FollowCollective},
		{"@here.", "agent-swe", notify.FollowCollective},
		// A collective word that is part of a longer name is a name.
		{"@channels are noisy", "agent-swe", ""},
		{"@channel-ops please", "agent-swe", ""},
		// A personal mention outranks a collective ANYWHERE in the
		// message, including after it.
		{"@channel and @agent-swe specifically", "agent-swe", notify.FollowMention},
		{"@agent-swe and also @channel", "agent-swe", notify.FollowMention},
		{"nothing here", "agent-swe", ""},
		{"a bare @ sign", "agent-swe", ""},
	}
	for _, c := range cases {
		got, ok := literal.Detect(c.text, c.self)
		if c.want == "" {
			if ok {
				t.Errorf("Detect(%q, %q) = %q, want no trigger", c.text, c.self, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("Detect(%q, %q) = %q/%v, want %q", c.text, c.self, got, ok, c.want)
		}
	}
}

// An unresolved identity must fall through to the collective check rather
// than matching everything.
func TestAnUnresolvedIdentityMatchesNoOne(t *testing.T) {
	for _, g := range []notify.MentionGrammar{markup, literal} {
		if _, ok := g.Detect("hey @agent-swe and <@U123>", ""); ok {
			t.Errorf("%s matched a personal mention with no identity", g.Backend())
		}
	}
	if r, ok := literal.Detect("@here anyone?", ""); !ok || r != notify.FollowCollective {
		t.Errorf("a collective was missed with no identity: %q/%v", r, ok)
	}
	if r, ok := markup.Detect("<!here> anyone?", ""); !ok || r != notify.FollowCollective {
		t.Errorf("a collective was missed with no identity: %q/%v", r, ok)
	}
}

// A name that genuinely ends in '.' or '-' stays matchable — which is why
// the raw token is compared before the trimmed one.
func TestANameEndingInPunctuationStillMatches(t *testing.T) {
	if r, ok := literal.Detect("hey @jr. about that", "jr."); !ok || r != notify.FollowMention {
		t.Fatalf("a name ending in a dot did not match: %q/%v", r, ok)
	}
}

// A multi-byte rune can only END a token, never appear inside one.
func TestNonASCIITextDoesNotConfuseTheScanner(t *testing.T) {
	if r, ok := literal.Detect("héllo @agent-swe — thanks", "agent-swe"); !ok || r != notify.FollowMention {
		t.Fatalf("a mention among non-ASCII text was missed: %q/%v", r, ok)
	}
	if _, ok := literal.Detect("@agent-swé", "agent-swe"); ok {
		t.Fatal("a token ending in a multi-byte rune matched a shorter name")
	}
}

func TestEachGrammarNamesItsBackend(t *testing.T) {
	if markup.Backend() != "slack" || literal.Backend() != "mattermost" {
		t.Fatalf("backends are %q and %q", markup.Backend(), literal.Backend())
	}
}
