package llmapi

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/words"
)

// HOW A CANDIDATE CREDENTIAL IS NAMED TO THE PERSON WHO PASTED IT: what it
// is, what is wrong with its characters, and what a refused test means.

// trimValues drops the whitespace around each candidate value. A token or a
// key never begins or ends with it, and a paste or a file read carries it as
// often as not; what is left inside is [strayProblem]'s to refuse.
func trimValues(values map[string]string) {
	for name, value := range values {
		values[name] = strings.TrimSpace(value)
	}
}

// builtinProfile is the shipped profile of the entry's CLI, Claude Code's for
// an entry that names none, and whether there is one.
func builtinProfile(entry *config.LLMProvider) (harnessprofile.Profile, bool) {
	if entry.Harness == "" {
		return harnessprofile.Builtin("claude-code")
	}
	return harnessprofile.Builtin(string(entry.Harness))
}

// credentialWords is how a refusal names the credential a write carries.
type credentialWords struct {
	// agent is the entry's CLI in a sentence ("Claude Code").
	agent string
	// noun is "token" or "API key" for a value in one of the entry's
	// credential slots, "credential" otherwise.
	noun string
	// prefix is how the vendor's credential of that kind begins, "" where
	// the profile names none.
	prefix string
	// mint is the command that makes a new token ("claude setup-token"),
	// "" for a key or where the CLI mints none.
	mint string
}

// wordsFor names the credential names carries for entry under key. A write of
// several values (a cloud provider's) is named as a credential.
func wordsFor(entry *config.LLMProvider, key string, names []string) credentialWords {
	words := credentialWords{agent: agentName(entry), noun: "credential"}
	if len(names) != 1 {
		return words
	}
	sources := entry.CredentialSources(key)
	profile, known := builtinProfile(entry)
	switch names[0] {
	case slotVar(sources.Token):
		words.noun = "token"
		if known {
			words.prefix = profile.TokenPrefix
			if len(profile.CaptureTokenArgs) > 0 {
				words.mint = strings.Join(append([]string{profile.Binary}, profile.CaptureTokenArgs...), " ")
			}
		}
	case slotVar(sources.APIKey):
		words.noun = "API key"
		if known {
			words.prefix = profile.APIKeyPrefix
		}
	}
	return words
}

// refused is the sentence for a test the credential did not pass, by what the
// CLI's own words meant ([harnessprofile.Profile.AuthMeaning]).
//
// NEVER THE STORED CREDENTIAL'S HEALTH. The write stores nothing, so the model
// keeps the credential it had; whether that one still answers is the model's
// own test to say, and an earlier wording that vouched for it was wrong
// whenever it had expired or met its limit.
func (w credentialWords) refused(meaning harnessprofile.AuthMeaning) string {
	var cause string
	switch meaning {
	case harnessprofile.AuthUnread:
		cause = fmt.Sprintf("%s did not use this %s, so the vendor never saw it. "+
			"Copy the whole %s again as one unbroken line", w.agent, w.noun, w.noun)
		if w.prefix != "" {
			cause += " starting with " + w.prefix
		}
		cause += "."
	default:
		cause = fmt.Sprintf("The vendor refused this %s: it is wrong, expired, or revoked.", w.noun)
		if w.mint != "" {
			cause += " Make a new one with " + w.mint + " and paste it here."
		} else {
			cause += " Paste a current one."
		}
	}
	return cause + " Nothing was stored, and the model still uses its current " + w.noun + "."
}

// meaningOf is what the first failing line of a check said about the
// credential it was handed.
func meaningOf(c Check) harnessprofile.AuthMeaning {
	for _, line := range c.Lines {
		if !line.OK {
			return line.Meaning
		}
	}
	return ""
}

// strayRune is a character no credential holds, at its 1-based position.
type strayRune struct {
	at int
	r  rune
}

// strayRunes is every whitespace, control or invisible formatting character
// in value. A terminal that wraps a long token turns the break into spaces
// when it is copied, and Claude Code then ignores the whole token and says it
// is not logged in; a zero-width character comes along from chat and
// documents.
func strayRunes(value string) []strayRune {
	var out []strayRune
	at := 0
	for _, r := range value {
		at++
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			out = append(out, strayRune{at: at, r: r})
		}
	}
	return out
}

// strayProblem is the sentence for a value holding stray characters, "" for
// one that holds none: how many of which kind, where, and what to do.
func strayProblem(noun, value string) string {
	stray := strayRunes(value)
	if len(stray) == 0 {
		return ""
	}
	counts := map[string]int{}
	var kinds []string
	for _, s := range stray {
		kind := strayKind(s.r)
		if counts[kind] == 0 {
			kinds = append(kinds, kind)
		}
		counts[kind]++
	}
	parts := make([]words.Said, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, words.Said(plural(counts[kind], "1 "+kind, strconv.Itoa(counts[kind])+" "+kind+"s")))
	}
	positions := make([]int, 0, len(stray))
	for _, s := range stray {
		positions = append(positions, s.at)
	}
	// THE FIRST THREE POSITIONS and how many more, since a value pasted with a
	// space between every word would otherwise list every one of them.
	where := plural(len(stray), "character ", "characters ") + words.AndMore(positions, 3)
	return fmt.Sprintf("This %s contains %s (%s). %s %s is one unbroken line, so copy it again "+
		"or remove %s.", noun, words.And(parts), where, capitalised(article(noun)), noun,
		plural(len(stray), "it", "them"))
}

// strayKind names a stray character as a person sees it, or does not.
func strayKind(r rune) string {
	switch {
	case r == '\n' || r == '\r' || r == ' ' || r == ' ':
		return "line break"
	case r == '\t':
		return "tab"
	case unicode.IsSpace(r):
		return "space"
	}
	return "invisible character"
}

// capitalised is word with its first letter upper-cased.
func capitalised(word string) string {
	if word == "" {
		return word
	}
	return strings.ToUpper(word[:1]) + word[1:]
}

// article is "a" or "an" before noun, by its first letter.
func article(noun string) string {
	if noun != "" && strings.ContainsRune("AEIOUaeiou", rune(noun[0])) {
		return "an"
	}
	return "a"
}
