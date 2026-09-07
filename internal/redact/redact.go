// Package redact is THE secret-redaction pass for free text leaving the
// engine.
//
// Anything the engine ships to a place a person or a model reads — a tool
// result, a coding-agent transcript, a setup-step failure, an event-store
// payload — can carry a credential it never meant to. A cloned repo URL with
// the token in it, a CLI that echoes its key on failure, a provisioning
// command whose ${VAR} was resolved before it ran.
//
// There is deliberately only one of these: a second copy is how a surface ends
// up redacting a token shape its neighbour already knows about. It imports
// nothing from the rest of Crewlet so every layer can depend on it.
//
// The patterns are a DENYLIST OF KNOWN CREDENTIAL SHAPES, which bounds what
// this can promise: it catches the vendor prefixes and key formats below, not
// an arbitrary opaque secret. It is the last line, not the first — the first
// is not putting a credential in the text.
package redact

import (
	"regexp"
)

// Marker is the prefix every replacement carries, so a reader can tell
// redaction from the original text and so a second pass recognises its own
// work.
const Marker = "[REDACTED:"

type rule struct {
	pattern *regexp.Regexp
	with    string
}

// rules are applied in order, and order matters where one shape is a prefix of
// another: sk-proj- is checked before the bare sk- that would otherwise
// swallow it and label an OpenAI project key as a plain api-key.
//
// Go's regexp is RE2: no backtracking, so matching is linear in the input
// whatever the pattern, and the private-key block's lazy [\s\S]*? cannot
// become the catastrophic case it would be in a backtracking engine. That
// matters here — this pass runs over coding-agent transcripts, the largest
// free text the engine moves. RE2's leftmost-shortest semantics are also what
// stop that pattern running from the first BEGIN to the last END and eating
// every log line between two keys.
var rules = []rule{
	{regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{20,}`), Marker + "api-key]"},
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`), Marker + "api-key]"},
	{regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`), Marker + "api-key]"},
	{regexp.MustCompile(`xox[bpsare]-[A-Za-z0-9-]{20,}`), Marker + "slack-token]"},
	{regexp.MustCompile(`AKIA[A-Z0-9]{16}`), Marker + "aws-key]"},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), Marker + "github-token]"},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{50,}`), Marker + "github-token]"},
	{regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`), Marker + "gitlab-token]"},
	{regexp.MustCompile(`gl(?:rt|soat|ptt)-[A-Za-z0-9_-]{20,}`), Marker + "gitlab-token]"},
	{
		regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----` +
			`[\s\S]*?` +
			`-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`),
		Marker + "private-key]",
	},
	{regexp.MustCompile(`(?i)(?:password|passwd|pwd)\s*[:=]\s*\S+`), Marker + "password]"},
}

// Secrets replaces known credential patterns with markers.
//
// Idempotent: a marker matches no rule, so redacting twice is the same as
// redacting once — which matters because this runs at more than one layer and
// a transcript can pass through both.
func Secrets(text string) string {
	if text == "" {
		return text
	}
	for _, r := range rules {
		// MATCH BEFORE REPLACING, because ReplaceAllString allocates even
		// when it changes nothing: with no match it still copies the
		// whole input into a fresh buffer and then converts that buffer
		// to a string. Eleven rules over a clean transcript is therefore
		// twenty-two full copies of it — and a clean transcript is the
		// overwhelming case, since this runs on every sandbox result and
		// every coding-run transcript whether or not a credential is
		// in it.
		// MatchString allocates nothing.
		if r.pattern.MatchString(text) {
			text = r.pattern.ReplaceAllString(text, r.with)
		}
	}
	return text
}

// Contains reports whether text still holds something this pass would replace.
// For assertions and for a caller that must refuse rather than sanitise.
//
// Asked of the RULES rather than by redacting and comparing. The old form
// built the entire redacted string to throw it away, which is the whole cost
// of the pass paid for an answer that is one bit — and it stopped at the first
// rule only by accident of there being nothing to stop.
func Contains(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range rules {
		if r.pattern.MatchString(text) {
			return true
		}
	}
	return false
}
