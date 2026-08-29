package notify

import (
	"regexp"
	"strings"
)

// The two shapes a chat backend's mentions come in.
//
// A backend either REWRITES a mention into markup before delivery (Slack
// turns every one into `<@U123>` / `<!channel>`), or leaves it LITERAL in
// the message text (Mattermost stores `@agent-swe` exactly as typed). Those
// are the only two, they need entirely different matching, and both are
// data-driven here so a vendor package declares its constants rather than
// writing a third matcher.

// MarkupGrammar matches a backend that rewrites mentions into structured
// tokens before delivery.
//
// Matching is a pair of patterns over that markup rather than a search for a
// literal name, which is what makes it exact: the backend has already
// resolved who was meant, so there is no ambiguity left to guess at.
type MarkupGrammar struct {
	// Name is the backend namespace follows are stored under.
	Name string

	// User captures the mentioned identity in its first submatch, so a
	// mention of somebody else cannot be mistaken for one of this seat.
	User *regexp.Regexp

	// Collective matches an everyone-in-the-room address.
	Collective *regexp.Regexp
}

// Backend implements [MentionGrammar].
func (g MarkupGrammar) Backend() string { return g.Name }

// Detect implements [MentionGrammar].
//
// A DIRECT MENTION OUTRANKS A COLLECTIVE ADDRESS, on both grammars: being
// named personally is a stronger signal than being in the room when somebody
// shouted, and the reason is what an operator reads to understand why a seat
// answered.
func (g MarkupGrammar) Detect(text, selfIdentity string) (FollowReason, bool) {
	if selfIdentity != "" && g.User != nil {
		for _, m := range g.User.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 && m[1] == selfIdentity {
				return FollowMention, true
			}
		}
	}
	if g.Collective != nil && g.Collective.MatchString(text) {
		return FollowCollective, true
	}
	return "", false
}

// LiteralGrammar matches a backend that leaves `@username` in the text.
//
// # Why a scanner and not a regular expression
//
// The rule needs two things Go's regexp cannot express, because RE2 has no
// lookaround: the `@` must not be preceded by a word character or another
// `@` (so `email@example.com` and `@@all` are not mentions), and the
// username must not be a PREFIX of a longer one (so `@ana` does not match in
// `@anabel`). Writing it as a scan over the text says both rules plainly and
// runs in one pass, where a lookaround-free regex would need alternations
// that encode the same rules unreadably and get them subtly wrong.
type LiteralGrammar struct {
	// Name is the backend namespace follows are stored under.
	Name string

	// Collectives are the everyone-in-the-room words, without the `@` and
	// matched case-insensitively — "all", "channel", "here".
	Collectives []string
}

// Backend implements [MentionGrammar].
func (g LiteralGrammar) Backend() string { return g.Name }

// Detect implements [MentionGrammar].
func (g LiteralGrammar) Detect(text, selfIdentity string) (FollowReason, bool) {
	self := strings.ToLower(strings.TrimPrefix(selfIdentity, "@"))
	var collective bool
	for token := range mentionTokens(text) {
		if self != "" && matchesName(token, self) {
			// Return on the FIRST personal mention rather than
			// remembering it: nothing outranks it, so there is
			// nothing left to find.
			return FollowMention, true
		}
		for _, c := range g.Collectives {
			if matchesName(token, strings.ToLower(c)) {
				// Remembered, not returned — a personal mention
				// later in the same message still outranks it.
				collective = true
			}
		}
	}
	if collective {
		return FollowCollective, true
	}
	return "", false
}

// matchesName compares one scanned token to a name.
//
// Two spellings are accepted: the token as scanned, and the token with
// trailing `.` and `-` stripped. Both characters are legal INSIDE a name on
// the backends that use this grammar, but at the end of one they are far
// more often sentence punctuation — "thanks @agent-swe." names the seat.
// Comparing the raw token first is what keeps a name that genuinely ends in
// one of them matchable at all.
func matchesName(token, name string) bool {
	if token == name {
		return true
	}
	return strings.TrimRight(token, ".-") == name
}

// mentionTokens yields the lowercased name after each `@` that begins a
// mention.
//
// A token is the maximal run of name characters, so a mention is compared
// WHOLE. Matching a prefix instead is how `@ana` fires on `@anabel` — and on
// a backend where a seat is addressed by username, that is one seat reading
// another's mail.
func mentionTokens(text string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for i := 0; i < len(text); i++ {
			if text[i] != '@' {
				continue
			}
			// The `@` must start a mention: a word character before
			// it means an address or an identifier, and another `@`
			// means the first one already consumed the mention.
			if i > 0 && (isNameByte(text[i-1]) || text[i-1] == '@') {
				continue
			}
			j := i + 1
			for j < len(text) && isNameByte(text[j]) {
				j++
			}
			if j == i+1 {
				continue // a bare `@`
			}
			if !yield(strings.ToLower(text[i+1 : j])) {
				return
			}
			i = j - 1
		}
	}
}

// isNameByte reports whether b may appear in a chat username.
//
// ASCII only, and that is the backends' own rule rather than a shortcut:
// Slack ids and Mattermost usernames are both drawn from this set, so a
// multi-byte rune can only ever END a token — which the loop handles by
// treating it as a non-name byte.
func isNameByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b == '.', b == '-':
		return true
	}
	return false
}
