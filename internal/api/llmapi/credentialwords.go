package llmapi

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
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

// builtinProfile is the shipped profile of the entry's harness, Claude Code's for
// an entry that names none, and whether there is one.
func builtinProfile(entry *config.LLMProvider) (harnessprofile.Profile, bool) {
	if entry.Harness == "" {
		return harnessprofile.Builtin("claude-code")
	}
	return harnessprofile.Builtin(string(entry.Harness))
}

// credentialWords is how a refusal names the credential a write carries.
type credentialWords struct {
	// agent is the entry's harness in a sentence ("Claude Code").
	agent string
	// noun is "token" or "API key" for a value in one of the entry's
	// credential slots, "credential" otherwise.
	noun string
	// prefix is how the vendor's credential of that kind begins, "" where
	// the profile names none.
	prefix string
	// mint is the command that makes a new token ("claude setup-token"),
	// "" for a key or where the harness mints none.
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
// harness's own words meant ([harnessprofile.Profile.AuthMeaning]).
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
