package redact

import (
	"encoding/base64"
	"math/rand/v2"
	"regexp/syntax"
	"strings"
	"testing"
)

// base64Rune reports whether r is a byte of base64's standard or url-safe
// alphabet, its padding included.
func base64Rune(r rune) bool {
	return 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9' || strings.ContainsRune("+/-_=", r)
}

// needsOutside reports whether every match of re holds a byte in reports
// false of: a literal or a class of such bytes it cannot match without.
func needsOutside(re *syntax.Regexp, in func(rune) bool) bool {
	switch re.Op {
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			if !in(r) {
				return true
			}
		}
	case syntax.OpCharClass:
		for c := rune(0); c < 128; c++ {
			if !in(c) {
				continue
			}
			for i := 0; i < len(re.Rune); i += 2 {
				if re.Rune[i] <= c && c <= re.Rune[i+1] {
					return false
				}
			}
		}
		return true
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			if needsOutside(sub, in) {
				return true
			}
		}
	case syntax.OpCapture, syntax.OpPlus:
		return needsOutside(re.Sub[0], in)
	case syntax.OpRepeat:
		return re.Min >= 1 && needsOutside(re.Sub[0], in)
	case syntax.OpAlternate:
		for _, sub := range re.Sub {
			if !needsOutside(sub, in) {
				return false
			}
		}
		return true
	}
	return false
}

// base64Spelled are the markers of the rules a run of base64 can spell: the
// tokens a payload's encoded bytes may spell by chance.
var base64Spelled = map[string]bool{
	Marker + "google-refresh-token]": true,
	Marker + "google-client-secret]": true,
	Marker + "api-key]":              true,
	Marker + "slack-token]":          true,
	Marker + "aws-key]":              true,
	Marker + "github-token]":         true,
	Marker + "gitlab-token]":         true,
	Marker + "digitalocean-token]":   true,
}

// THE RULES BASE64 CAN SPELL ARE KNOWN. Every rule whose matches each hold a
// byte outside base64's alphabets is held to that; every rule that need not is
// named ([base64Spelled]): the tokens encoded bytes spell by chance, which a
// session's scrub keeps a payload from (internal/sandbox/executor's
// payloadScrub; its TestAPayloadIsKeptFromTheShapeRulesWhole holds a payload
// spelling each). A key's rule (a password's, an AWS secret's) is spelled in
// base64 only through `=`, which a well-formed run holds only as its last
// padding, with no value after it: held to that by reading runs ending so.
// A rule added that base64 can spell goes red here until it is named, and a
// payload spelling it is added there.
func TestTheRulesBase64CanSpellAreKnown(t *testing.T) {
	t.Parallel()
	for _, r := range rules() {
		if r.value != nil {
			continue
		}
		re, err := syntax.Parse(r.pattern.String(), syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		if spelled := !needsOutside(re.Simplify(), base64Rune); spelled != base64Spelled[r.with] {
			t.Errorf("the rule %s (%s) can be spelled in base64: %v, and is named so: %v", r.with,
				r.pattern, spelled, base64Spelled[r.with])
		}
	}
	keys := append([]string{"SessionToken", "session_token", "session-token", "SESSION_TOKEN",
		"SecurityToken", "security_token", "SECURITY_TOKEN", "SecretAccessKey", "secret_access_key",
		"SECRET_ACCESS_KEY"}, passwordKeys...)
	raw := make([]byte, 3<<10)
	rand.NewChaCha8([32]byte{7}).Read(raw)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawURLEncoding} {
		run := enc.EncodeToString(raw)
		if Secrets(run) != run {
			t.Fatal("the premise: the run alone is redacted")
		}
		for _, key := range keys {
			for _, text := range []string{run + key, run + key + "=", run + key + "==", run[:1<<10] + key +
				strings.ToUpper(key) + run[1<<10:]} {
				if got := Secrets(text); got != text {
					t.Errorf("a well-formed run of base64 holding the key %q is redacted", key)
				}
			}
		}
	}
}

// mediaHeadPartMax mirrors internal/sandbox/executor's: the most bytes a data
// URI's media type or subtype holds where a payload follows.
const mediaHeadPartMax = 19

// restrictedNameRune reports whether r is an RFC 6838 restricted-name byte.
func restrictedNameRune(r rune) bool {
	return 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9' || strings.ContainsRune("!#$&-^_.+", r)
}

// shortestBytes is the fewest bytes re matches, read off its syntax.
func shortestBytes(re *syntax.Regexp) int {
	switch re.Op {
	case syntax.OpLiteral:
		return len(string(re.Rune))
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return 1
	case syntax.OpConcat:
		n := 0
		for _, sub := range re.Sub {
			n += shortestBytes(sub)
		}
		return n
	case syntax.OpAlternate:
		n := -1
		for _, sub := range re.Sub {
			if s := shortestBytes(sub); n < 0 || s < n {
				n = s
			}
		}
		return max(n, 0)
	case syntax.OpCapture, syntax.OpPlus:
		return shortestBytes(re.Sub[0])
	case syntax.OpRepeat:
		return re.Min * shortestBytes(re.Sub[0])
	}
	return 0
}

// NO RULE MATCHES WITHIN A MEDIA HEAD'S PART: every rule either needs a byte
// outside RFC 6838's restricted names, or matches no fewer bytes than one part
// past [mediaHeadPartMax] holds; so shaping a data URI's head that a payload
// follows never changes it (internal/sandbox/executor's mediaRun), and a box
// counting payloads by the head's length counts what the take keeps. Keyed
// rules need a separator (`:`, `=`, a space) outside those names. A rule added
// that fits within a part goes red here.
func TestNoRuleMatchesWithinAMediaHeadsPart(t *testing.T) {
	t.Parallel()
	for _, r := range rules() {
		re, err := syntax.Parse(r.pattern.String(), syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		re = re.Simplify()
		if !needsOutside(re, restrictedNameRune) && shortestBytes(re) <= mediaHeadPartMax {
			t.Errorf("the rule %s (%s) can match within a media head's part of %d bytes", r.with, r.pattern,
				mediaHeadPartMax)
		}
	}
}
