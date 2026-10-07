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
// [keyBlocks] — before every rule below, so a token shape that happens to occur
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

// MaxKeyBlockBytes is the longest a private key's block may run past its BEGIN
// line — its body and its END line, in whatever wrapping the text puts round
// them — and still be read as one.
//
// A key's block is BOUNDED BY ITS STRUCTURE, not by this ([keyBlocks]): it ends
// at its END line or at the first line that is not a key's, and this only says
// how long a run of key-shaped lines can go on. RSA-16384, the largest key
// anybody issues, is about 12.5 KiB of body, and under half as much again with
// a log's timestamp or a reader's line number on every line; sixty-four KiB is
// any key in any wrapping three times over. What it bounds beyond a key is a
// stream of key-shaped lines straight after an armour line — a base64 dump
// under a grep's match — which is read as a key, and held back from a live
// view ([Settled]), for no longer than this.
const MaxKeyBlockBytes = 64 << 10

// minKeyLine is the shortest base64 run read as a line of a key's BODY. Every
// encoder wraps the body at 64 (PEM) or 70 (OpenSSH) characters, so a shorter
// run is a key's last line or no key at all — a single word such as "Done" is
// base64's alphabet too, and a line of prose is a string of such words.
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
func keySpans(text string) [][2]int {
	spans, _ := keyBlocks(text)
	return spans
}

// keyBlocks reads every private key's block in text: the spans to redact, in
// order, and where the last BEGIN starts when its block is still OPEN at the
// end of text — a line written after it could still extend it or close it —
// or -1.
//
// A KEY IS READ BY ITS STRUCTURE, closed or not ([readKey]). Its block is what
// RFC 1421 and RFC 7468 say it is: optional `Proc-Type:` and `DEK-Info:`
// headers, at most one blank line, base64 lines at the encoder's width
// ([minKeyLine]) and at most one shorter last line — and then its END line,
// when it has one. The first line that is none of those ENDS THE BLOCK, so an
// END after it closes nothing. That is the whole defence against a header with
// no key under it: a grep for the armour, a heredoc's first line, a sentence
// about keys. The first rule here ran lazily from a BEGIN to the next END
// anywhere, and the one after it to any END within a bound, WHATEVER lay
// between — so a grep's match for the BEGIN line and a later one for the END
// took every line of the transcript between them with it, and a live view
// held back everything after the first for as long as an END could still
// come.
//
// A key reaches text in every WRAPPING there is, and each is allowed for:
//
//   - a line break is a real one or one escaped inside a string (`\n`), since
//     a key in a JSON value is the commonest way one reaches a log;
//   - a line may carry the wrapping's per-line PREFIX — a log's timestamp, a
//     reader's line number, a grep's `file:N:`, a diff's `+` — shaped like the
//     BEGIN line's own ([prefixOK]);
//   - and its SUFFIX, the text after the armour on the BEGIN line, repeated on
//     every line (an `echo "…" >> key.pem`, a `"…" +` concatenation);
//   - a line of nothing but punctuation between them is the wrapping's own
//     ([glue]): the `" +` of a string concatenated across lines;
//   - and the whole key may be on one line, its body whitespace-separated
//     base64 between the armours, its headers too.
//
// A body line that ends in anything else — the `"}` that closes the string a
// key was written into, the `…` of a cut — ENDS the block at its base64,
// because what follows is the enclosing text.
func keyBlocks(text string) ([][2]int, int) {
	if !strings.Contains(text, keyArmour) {
		return nil, -1
	}
	begins := keyBegin.FindAllStringIndex(text, -1)
	var spans [][2]int
	open := -1
	for _, b := range begins {
		if len(spans) > 0 && b[0] < spans[len(spans)-1][1] {
			// Inside the block before it. Part of that key, not one of
			// its own.
			continue
		}
		block := readKey(text, b)
		if block.end > 0 {
			spans = append(spans, [2]int{b[0], block.end})
		}
		open = -1
		if block.open {
			open = b[0]
		}
	}
	return spans, open
}

// keyBlock is what one BEGIN line's block holds.
type keyBlock struct {
	// end is where its span ends, 0 where nothing under the BEGIN line is a
	// key.
	end int
	// open is whether the text ran out while the block was still being
	// read: what is written next could still extend it or close it.
	open bool
}

// readKey reads the block of the BEGIN armour at b (its start and end in
// text). See [keyBlocks] for the structure it reads.
func readKey(text string, b []int) keyBlock {
	shape := prefixShape(strings.Trim(text[segmentStart(text, b[0]):b[0]], " \t"))
	r := keyReader{text: text, from: b[1], shape: shape}

	// THE REST OF THE BEGIN LINE: nothing, the wrapping's per-line suffix,
	// or — for a key on one line — its body.
	restEnd, next := lineAt(text, b[1])
	rest := text[b[1]:restEnd]
	if keyBegin.MatchString(rest) {
		return keyBlock{}
	}
	if e := keyEnd.FindStringIndex(rest); e != nil {
		// BEGIN and END on one line: closed only with a body between, and
		// nothing but its separators after it.
		between := rest[:e[0]]
		h := skipHeaders(between)
		if body, end := runChunk(between[h:]); body && lastBase64(between[h+end:]) < 0 {
			return keyBlock{end: b[1] + e[1]}
		}
		return keyBlock{}
	}
	trimmed := trimLine(rest)
	if trimmed == "" {
		return r.read(next)
	}
	h := skipHeaders(trimmed)
	if body, end := runChunk(trimmed[h:]); body {
		// The body begins on the BEGIN line: a key on one line, or its
		// first line glued to the armour.
		end += h
		r.body, r.end, r.state = 1, b[1]+leadingSpace(rest)+end, inBody
		if lastBase64(trimmed[end:]) >= 0 {
			// The enclosing text resumes on the BEGIN line itself.
			return keyBlock{end: r.end}
		}
		return r.read(next)
	}
	if inBase64(trimmed[0]) {
		// The armour line goes on past the armour, with no key.
		return keyBlock{}
	}
	r.suffix = trimmed
	return r.read(next)
}

// keyReader walks a key's block a line at a time.
type keyReader struct {
	text string
	// from is the end of the BEGIN armour, which [MaxKeyBlockBytes] counts
	// from.
	from int
	// shape is the BEGIN line's prefix as [prefixShape] has it, and suffix
	// its text after the armour where that is the wrapping's per-line
	// suffix rather than a body.
	shape, suffix string

	state keyState
	// blank is whether the block's one blank line has been read.
	blank bool
	// body counts the body lines read, and end is where the last one's
	// base64 ends.
	body, end int
}

type keyState int

const (
	// inHeaders is before the body: headers and one blank line may come.
	inHeaders keyState = iota
	// inBody is among the body's full-width lines.
	inBody
	// afterLast is past the body's short last line: only the END may come.
	afterLast
)

// read walks the block's lines from pos, the start of the line after the BEGIN
// line (-1 for none), to where the block ends.
func (r *keyReader) read(pos int) keyBlock {
	ended := func() keyBlock {
		if r.body == 0 {
			return keyBlock{}
		}
		return keyBlock{end: r.end}
	}
	ranOut := func() keyBlock {
		block := ended()
		block.open = true
		return block
	}
	for pos >= 0 {
		if pos >= len(r.text) {
			return ranOut()
		}
		lineEnd, after := lineAt(r.text, pos)
		if lineEnd-r.from > MaxKeyBlockBytes {
			return ended()
		}
		line := r.text[pos:lineEnd]
		if keyBegin.MatchString(line) {
			return ended()
		}
		if e := keyEnd.FindStringIndex(line); e != nil {
			if r.closes(pos, line[:e[0]]) {
				return keyBlock{end: pos + e[1]}
			}
			return ended()
		}
		trimmed := trimLine(line)
		switch {
		case trimmed == "" && after < 0:
			// The end of the text: the line being written next.
			return ranOut()
		case trimmed == "":
			if r.state != inHeaders || r.blank {
				return ended()
			}
			r.blank = true
		case glue(trimmed):
			// The wrapping's own punctuation, a line of its own.
		case r.state == inHeaders && !r.blank && r.header(trimmed):
		case r.state == inHeaders && !r.blank && trailingRun(trimmed) < minKeyLine && r.prefixOK(trimmed):
			// A line of nothing but its prefix: the blank line, prefixed.
			r.blank = true
		default:
			// A line that is not the body's, or one whose tail is the
			// enclosing text, ends the block.
			if terminated, ok := r.bodyLine(pos+leadingSpace(line), trimmed); !ok || terminated {
				return ended()
			}
		}
		pos = after
	}
	return ranOut()
}

// closes reports whether an END line whose text before the armour is pre ends
// this block: its prefix, or the body's last line written up to the armour.
func (r *keyReader) closes(pos int, pre string) bool {
	trimmed := trimLine(pre)
	switch {
	case trimmed == "" || r.prefixOK(trimmed):
		return r.body > 0
	case inBase64(trimmed[len(trimmed)-1]):
		terminated, ok := r.bodyLine(pos+leadingSpace(pre), trimmed)
		return ok && !terminated && r.body > 0
	}
	return false
}

// bodyLine reads one line of the body — the trimmed line, starting at at in
// the text — and reports whether it is one and whether it ends the block.
func (r *keyReader) bodyLine(at int, line string) (terminated, ok bool) {
	core := line
	if r.suffix != "" && strings.HasSuffix(core, r.suffix) {
		core = strings.TrimRight(core[:len(core)-len(r.suffix)], " \t")
	} else if i := lastBase64(core); i < len(core)-1 {
		// A tail that is not the wrapping's: the enclosing text resumes
		// after the base64.
		core, terminated = core[:i+1], true
	}
	run := trailingRun(core)
	if run == 0 || !r.prefixOK(strings.TrimRight(core[:len(core)-run], " \t")) {
		return false, false
	}
	end := at + len(core)
	switch {
	case run >= minKeyLine && r.state != afterLast:
		r.body++
		r.state = inBody
	case run < minKeyLine && r.state == inBody:
		// The body's short last line, which ends it.
		r.state = afterLast
	default:
		return false, false
	}
	r.end = end
	return terminated, true
}

// header reports whether a line is one of RFC 1421's, under the wrapping's
// prefix.
func (r *keyReader) header(line string) bool {
	for _, name := range []string{"Proc-Type:", "DEK-Info:"} {
		if i := strings.Index(line, name); i >= 0 && r.prefixOK(strings.TrimRight(line[:i], " \t")) {
			return true
		}
	}
	return false
}

// prefixOK reports whether p — the text before a line's base64, trimmed — is
// the wrapping's per-line prefix: nothing, punctuation and spaces alone (a
// string's quote, an indent, a table's bar), or text SHAPED LIKE THE BEGIN
// LINE'S OWN PREFIX with such punctuation after it. Shaped like, because a
// wrapping repeats its prefix with its numbers moved on: a line number, a
// timestamp, a grep's `file:N:` against its context lines' `file-N-`. And the
// BEGIN line's prefix may go on past the gutter every line shares — a log's
// timestamp and then its message, a reader's line number and then the code
// the key was assigned in — so a line's gutter need only begin it.
//
// What this refuses is the line of PROSE: text before the base64 that the
// BEGIN line did not start with — a command, a sentence — which is what a
// grep's next result or a run's next step looks like.
func (r *keyReader) prefixOK(p string) bool {
	last := lastBase64(p)
	if last < 0 {
		return true
	}
	return strings.HasPrefix(r.shape, prefixShape(p[:last+1]))
}

// prefixShape is a prefix with what a wrapping moves on from line to line made
// alike: every run of digits one 0, every `-` a `:`, every run of spaces one
// space.
func prefixShape(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case '0' <= c && c <= '9':
			for i+1 < len(p) && '0' <= p[i+1] && p[i+1] <= '9' {
				i++
			}
			b.WriteByte('0')
		case c == '-':
			b.WriteByte(':')
		case c == ' ' || c == '\t':
			for i+1 < len(p) && (p[i+1] == ' ' || p[i+1] == '\t') {
				i++
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// oneLineHeader is one of RFC 1421's headers written on the same line as the
// body it precedes — a key with its line breaks turned to spaces, or removed —
// with whatever separates it from what came before. The values are what
// OpenSSL writes: `4,ENCRYPTED`, and a cipher with an IV of its block size,
// eight bytes or sixteen, in hex — which is what ends a DEK-Info glued to the
// body with no break at all.
var oneLineHeader = regexp.MustCompile(`^[^A-Za-z0-9+/=]*(?:Proc-Type:\s*4,ENCRYPTED|DEK-Info:\s*[A-Za-z0-9-]+,(?:[0-9A-Fa-f]{32}|[0-9A-Fa-f]{16}))`)

// skipHeaders is how much of s, a key's body written on one line, is its RFC
// 1421 headers: the one-line reader ([runChunk]) would take a header's first
// word for a word of prose, and the line for no key at all.
func skipHeaders(s string) int {
	at := 0
	for {
		loc := oneLineHeader.FindStringIndex(s[at:])
		if loc == nil {
			return at
		}
		at += loc[1]
	}
}

// runChunk reads a key's body written on one line, from the start of s:
// base64 runs at the encoder's width ([minKeyLine]), with nothing but
// punctuation and spaces before and between them — one space, or the quotes
// and commas of a list of strings (`' '`, `", "`) — and at most one shorter run
// after them, the body's last. It reports whether there is such a body, and
// where it ends. A shorter run before any full-width one is a word, and no
// key's.
func runChunk(s string) (bool, int) {
	long, end := false, 0
	for i := 0; i < len(s); {
		if !inBase64(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && inBase64(s[j]) {
			j++
		}
		if j-i < minKeyLine {
			if long {
				end = j
			}
			return long, end
		}
		long, end, i = true, j, j
	}
	return long, end
}

// segmentStart is where the line holding byte i starts, a line break being a
// real one or an escaped one, as [lineAt] has it.
func segmentStart(s string, i int) int {
	for j := i - 1; j >= 0; j-- {
		switch {
		case s[j] == '\n':
			return j + 1
		case s[j] == 'n' && j > 0 && s[j-1] == '\\':
			return j + 1
		}
	}
	return 0
}

// lineAt is where the line starting at pos ends, and where the next one
// starts (-1 for none): a line break is "\n", or an escaped one — the two
// characters `\` `n` — inside a string. An escaped break written at the end of
// a real line (a quoted scalar broken across lines with its `\n` kept) is ONE
// break, or every line of it would be followed by an empty one.
func lineAt(s string, pos int) (int, int) {
	for i := pos; i < len(s); i++ {
		switch {
		case s[i] == '\n':
			return i, i + 1
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == 'n':
			next := i + 2
			if strings.HasPrefix(s[next:], "\r\n") {
				next += 2
			} else if strings.HasPrefix(s[next:], "\n") {
				next++
			}
			return i, next
		}
	}
	return len(s), -1
}

// trimLine is a line without its surrounding spaces and the debris of its line
// break: a real CR, an escaped one (`\r`), and the backslashes a string escaped
// twice or continued onto the next line leaves before it.
func trimLine(line string) string {
	for {
		trimmed := strings.TrimRight(line, " \t\r\\")
		trimmed = strings.TrimSuffix(trimmed, `\r`)
		if trimmed == line {
			return strings.TrimLeft(line, " \t")
		}
		line = trimmed
	}
}

// leadingSpace is how many spaces and tabs s begins with.
func leadingSpace(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

// glue reports whether a line holds nothing a key or a reader wrote: no letter
// and no digit, only the punctuation a wrapping puts between lines.
func glue(line string) bool {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' {
			return false
		}
	}
	return true
}

// trailingRun is how many bytes at the end of s are base64.
func trailingRun(s string) int {
	n := 0
	for n < len(s) && inBase64(s[len(s)-1-n]) {
		n++
	}
	return n
}

// lastBase64 is the index of the last base64 byte in s, or -1.
func lastBase64(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if inBase64(s[i]) {
			return i
		}
	}
	return -1
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
//   - a private key's block still OPEN — its BEGIN line, and every line
//     after it that a key's block could hold, while the next line written
//     could still be its body or its END ([keyBlocks]). The first line that
//     is not a key's ends the block, so a mere mention of the armour holds
//     back no more than itself;
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
	spans, open := keyBlocks(whole)
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

// lineStart is the start of the line holding byte i.
func lineStart(text string, i int) int {
	return strings.LastIndexByte(text[:i], '\n') + 1
}
