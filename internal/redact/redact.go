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
//
// # Text that is still being written
//
// A live view shows what a running process has written so far, and every
// rule here was written for text that is whole. [Settled] is the other half:
// how much of a growing text has a redaction that nothing written after it
// can change, so a reader is never shown a key's body that its END line,
// arriving a moment later, would have redacted. It lives beside the rules
// because it is a property OF them — a rule added here that can reach across a
// line break is a rule [Settled] has to learn about, and its test feeds every
// shape here through text that arrives a line at a time.
package redact

import (
	"regexp"
	"strings"
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
// whatever the pattern — which matters here, because this pass runs over
// coding-agent transcripts, the largest free text the engine moves.
//
// A PRIVATE KEY IS NOT A RULE HERE. It is the one shape that spans lines, and
// the one whose end may not have been written yet, so it is found by
// [keySpans] — before every rule below, so a token shape that happens to occur
// inside a key's body is taken with the block rather than leaving it split.
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
	{passwordRule, Marker + "password]"},
}

// passwordRule is the one rule besides a private key whose match can cross a
// line break: `\s` is a newline too, so a key on one line and its value on the
// next is one match — which [Settled] has to hold back for.
var passwordRule = regexp.MustCompile(`(?i)(?:password|passwd|pwd)\s*[:=]\s*\S+`)

// pendingPassword is a password key at the very END of a text, with nothing
// but whitespace or its separator after it: the start of a [passwordRule]
// match whose value has not been written yet.
var pendingPassword = regexp.MustCompile(`(?i)(?:password|passwd|pwd)\s*(?:[:=]\s*)?\z`)

// The private-key armour lines. ENCRYPTED is PKCS#8's encrypted form, which is
// still the key — under a passphrase that may sit in the same environment.
var (
	keyBegin = regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----`)
	keyEnd   = regexp.MustCompile(`-----END (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----`)
)

// keyArmour is what every BEGIN and END line above ends in, tested before
// either pattern runs: almost no text carries a key, and a substring test
// costs nothing where a pattern execution costs a scan.
const keyArmour = "PRIVATE KEY-----"

// MaxKeyBlockBytes is the furthest a private key's END line may begin past its
// BEGIN line and still close it.
//
// A PEM block is 1.7 KiB (RSA-2048) to about 6.4 KiB (RSA-8192) between its
// armour lines, and twice that once every line carries a prefix — a log
// timestamp, a line number, a diff's `+`. Sixty-four KiB is ten of the largest
// with room for the prefixes. It is a BOUND rather than "the next END", for
// two reasons. A header line with no key under it — a grep for the armour, a
// heredoc's first line — used to run to whatever END came next, however far
// on, and take every line between with it. And a text still being written
// would have to hold back everything after such a header for as long as it
// grows, since an END could close it at any distance ([Settled]): bounded, a
// header no END follows is settled once this much has been written after it.
const MaxKeyBlockBytes = 64 << 10

// minKeyLine is the shortest line read as a key's BODY where no END line says
// where the block stops. Every encoder wraps the body at 64 (PEM) or 70
// (OpenSSH) characters, so a run of shorter base64-looking lines under a
// header is not a key's body — a single word such as "Done" is base64's
// alphabet too, and a block with no END has nothing else to stop at.
const minKeyLine = 40

// Secrets replaces known credential patterns with markers.
//
// Idempotent: a marker matches no rule, so redacting twice is the same as
// redacting once — which matters because this runs at more than one layer and
// a transcript can pass through both.
func Secrets(text string) string {
	if text == "" {
		return text
	}
	if spans := keySpans(text); len(spans) > 0 {
		var b strings.Builder
		b.Grow(len(text))
		at := 0
		for _, s := range spans {
			b.WriteString(text[at:s[0]])
			b.WriteString(Marker + "private-key]")
			at = s[1]
		}
		b.WriteString(text[at:])
		text = b.String()
	}
	for _, r := range rules {
		// MATCH BEFORE REPLACING, because ReplaceAllString allocates even
		// when it changes nothing: with no match it still copies the
		// whole input into a fresh buffer and then converts that buffer
		// to a string. Ten rules over a clean transcript is therefore
		// twenty full copies of it — and a clean transcript is the
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
	if len(keySpans(text)) > 0 {
		return true
	}
	for _, r := range rules {
		if r.pattern.MatchString(text) {
			return true
		}
	}
	return false
}

// keySpans is every private key in text, as [start, end) byte ranges in order.
//
// A KEY RUNS FROM A BEGIN LINE TO THE FIRST END LINE AFTER IT, with no other
// BEGIN between and within [MaxKeyBlockBytes] — and WHATEVER lies between,
// because a key reaches text in every wrapping there is: one line, each line
// prefixed by a log's timestamp or a reader's line numbers, a JSON string
// with its line breaks escaped, a diff. The first rule here matched lazily
// from a BEGIN to the next END anywhere, which took a header nothing followed
// to the next key's END and everything between with it.
//
// A BEGIN NO END CLOSES is a key whose END was never written — a run that
// crashed mid-print, or a stream read while it is still being written — and
// it is the block's own STRUCTURE that says where it stops ([keyBody]): its
// header lines and its base64 body. It used to be no match at all, which
// published the whole body. A BEGIN followed by anything else — a grep's next
// result, a heredoc's next command — is a header with no key under it, and
// it is left as it is.
func keySpans(text string) [][2]int {
	if !strings.Contains(text, keyArmour) {
		return nil
	}
	begins := keyBegin.FindAllStringIndex(text, -1)
	if len(begins) == 0 {
		return nil
	}
	ends := keyEnd.FindAllStringIndex(text, -1)
	var spans [][2]int
	e := 0
	for i, b := range begins {
		if len(spans) > 0 && b[0] < spans[len(spans)-1][1] {
			// Inside the block before it — a header value that quotes an
			// armour line. Part of that key, not one of its own.
			continue
		}
		next := len(text)
		if i+1 < len(begins) {
			next = begins[i+1][0]
		}
		for e < len(ends) && ends[e][0] < b[1] {
			e++
		}
		if e < len(ends) && ends[e][0] < next && ends[e][0]-b[1] <= MaxKeyBlockBytes {
			spans = append(spans, [2]int{b[0], ends[e][1]})
			e++
			continue
		}
		if n := keyBody(text[b[1]:]); n > 0 {
			spans = append(spans, [2]int{b[0], b[1] + n})
		}
	}
	return spans
}

// keyBody is how far a key's block runs past its BEGIN line when no END line
// closes it: the end of its last body line, or 0 where nothing under the
// header is a key's body.
//
// The block is what RFC 1421 and RFC 7468 say it is, line by line: optional
// `Proc-Type:` and `DEK-Info:` headers (the traditional encrypted form), at
// most one blank line, then base64 lines at the encoder's width
// ([minKeyLine]) and at most one shorter last line. A line break is a real one
// or one escaped inside a string (`\n`), since a key in a JSON value is the
// commonest way one reaches a log. Bounded by [MaxKeyBlockBytes], like a block
// an END closes.
func keyBody(rest string) int {
	lineEnd, next := lineAt(rest, 0)
	if next < 0 || strings.TrimSpace(rest[:lineEnd]) != "" {
		// The armour line goes on past the armour, or nothing follows it:
		// not a block.
		return 0
	}
	body, end := 0, 0
	headers, blank := true, false
	for pos := next; pos < len(rest); {
		lineEnd, after := lineAt(rest, pos)
		if lineEnd > MaxKeyBlockBytes {
			break
		}
		// An escaped CRLF leaves its `\r` on the line; a real one, its CR.
		line := strings.Trim(strings.TrimSuffix(rest[pos:lineEnd], `\r`), " \t\r")
		switch {
		case headers && (strings.HasPrefix(line, "Proc-Type:") || strings.HasPrefix(line, "DEK-Info:")):
		case body == 0 && line == "" && !blank:
			blank, headers = true, false
		case base64Line(line) && len(line) >= minKeyLine:
			headers = false
			body++
			end = lineEnd
		case base64Line(line) && body > 0:
			// The short last line of the body, which ends it.
			return lineEnd
		default:
			return end
		}
		if after < 0 {
			break
		}
		pos = after
	}
	return end
}

// lineAt is where the line starting at pos ends, and where the next one
// starts (-1 for none): a line break is "\n", or an escaped one — the two
// characters `\` `n` — inside a string.
func lineAt(s string, pos int) (int, int) {
	for i := pos; i < len(s); i++ {
		switch {
		case s[i] == '\n':
			return i, i + 1
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == 'n':
			return i, i + 2
		}
	}
	return len(s), -1
}

// base64Line reports whether a line is nothing but base64's alphabet.
func base64Line(line string) bool {
	if line == "" {
		return false
	}
	for i := 0; i < len(line); i++ {
		if !inBase64(line[i]) {
			return false
		}
	}
	return true
}

// inBase64 reports whether c is in base64's alphabet, its padding included.
func inBase64(c byte) bool {
	switch {
	case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		return true
	}
	return c == '+' || c == '/' || c == '='
}

// Settled is how much of text — a text still being written, one complete line
// at a time — can be redacted and shown now: the length of its longest prefix,
// ending on a line break, whose redaction nothing written after text can
// change.
//
// THE PROMISE IS EXACT: for every boundary it returns, redacting the text in
// pieces cut there is the same as redacting it whole, however the rest of it
// arrives. So a reader shown each settled piece as it lands sees, in the end,
// exactly what the whole text redacts to — and never a credential in clear
// that a later line would have redacted. Three things hold a line back, and
// each is a match that can cross a line break:
//
//   - a private key's BEGIN that an END written later could still close —
//     until [MaxKeyBlockBytes] have been written after it with no END, after
//     which no END can;
//   - a key whose block, as written so far, runs across the boundary;
//   - a password key at the end with its value not written yet, since the
//     rule takes the value off the next line.
//
// Whatever is after the last line break is not a line yet, and is never
// settled: a credential is matched by its whole shape, and half a token on
// screen is a line that changes when its other half lands.
func Settled(text string) int {
	end := strings.LastIndexByte(text, '\n') + 1
	if end == 0 {
		return 0
	}
	whole := text[:end]
	spans := keySpans(whole)
	open := openKey(whole, spans)
	settled := end
	for {
		moved := false
		if open >= 0 && settled > lineStart(whole, open) {
			settled, moved = lineStart(whole, open), true
		}
		for _, s := range spans {
			if s[0] < settled && settled < s[1] {
				settled, moved = lineStart(whole, s[0]), true
			}
		}
		if loc := pendingPassword.FindStringIndex(whole[:settled]); loc != nil {
			settled, moved = lineStart(whole, loc[0]), true
		}
		if !moved {
			return settled
		}
	}
}

// openKey is where the last private key's BEGIN starts when an END written
// after text could still close it, or -1.
func openKey(text string, spans [][2]int) int {
	if !strings.Contains(text, keyArmour) {
		return -1
	}
	begins := keyBegin.FindAllStringIndex(text, -1)
	if len(begins) == 0 {
		return -1
	}
	last := begins[len(begins)-1]
	if len(text)-last[1] > MaxKeyBlockBytes {
		return -1
	}
	for _, s := range spans {
		if s[0] == last[0] && keyEnd.MatchString(text[last[1]:s[1]]) {
			// Closed by an END already written.
			return -1
		}
	}
	return last[0]
}

// lineStart is the start of the line holding byte i.
func lineStart(text string, i int) int {
	return strings.LastIndexByte(text[:i], '\n') + 1
}
