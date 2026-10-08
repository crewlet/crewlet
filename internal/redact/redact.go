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
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Marker is the prefix every replacement carries, so a reader can tell
// redaction from the original text and so a second pass recognises its own
// work.
const Marker = "[REDACTED:"

type rule struct {
	pattern *regexp.Regexp

	// with is what each match becomes, written as it stands — a marker,
	// never a template — on every rule alike. ReplaceAllString would read a
	// $ in it as a submatch to expand, and a [keyed] rule writes it itself,
	// so the literal reading is the one both paths can give.
	with string

	// keyed is the pattern again, for a rule that opens with no literal RE2
	// can skip ahead on: the rule then runs only where one of its words
	// begins. Nil for a rule that opens with one, which RE2 finds by itself.
	// Set by [keyed.rule] and never by hand, so it is always pattern's own.
	keyed *keyed
}

// in reports whether the rule matches anywhere in text, allocating nothing.
func (r rule) in(text string) bool {
	if r.keyed != nil {
		return r.keyed.index(text) >= 0
	}
	return r.pattern.MatchString(text)
}

// apply is text with every match of the rule replaced, or text itself —
// the same string, nothing copied — when there is none.
//
// MATCH BEFORE REPLACING, because ReplaceAllLiteralString allocates even when
// it changes nothing: with no match it still copies the whole input into a
// fresh buffer and then converts that buffer to a string. Ten rules over a
// clean transcript would be twenty full copies of it — and a clean transcript
// is the overwhelming case, since this runs on every sandbox result and every
// coding-run transcript whether or not a credential is in it.
func (r rule) apply(text string) string {
	if r.keyed == nil {
		if !r.pattern.MatchString(text) {
			return text
		}
		return r.pattern.ReplaceAllLiteralString(text, r.with)
	}
	start, end, found := r.keyed.next(text, 0)
	if !found {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	done := 0
	for found {
		b.WriteString(text[done:start])
		b.WriteString(r.with)
		done = end
		start, end, found = r.keyed.next(text, end)
	}
	b.WriteString(text[done:])
	return b.String()
}

// rules are applied in order, and order matters where one shape is a prefix of
// another: sk-proj- is checked before the bare sk- that would otherwise
// swallow it and label an OpenAI project key as a plain api-key.
//
// Go's regexp is RE2: no backtracking, so matching is linear in the input
// whatever the pattern — which matters here, because this pass runs over
// coding-agent transcripts, the largest free text the engine moves. Every rule
// opens with a literal RE2 skips ahead on, or is [keyed] on the words it opens
// with instead.
//
// A PRIVATE KEY IS NOT A RULE HERE. It is the one shape that spans lines, and
// the one whose end may not have been written yet, so it is found by
// [keyBlocks] — before every rule below, so a token shape that happens to occur
// inside a key's body is taken with the block rather than leaving it split.
var rules = []rule{
	{regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{20,}`), Marker + "api-key]", nil},
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`), Marker + "api-key]", nil},
	{regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`), Marker + "api-key]", nil},
	{regexp.MustCompile(`xox[bpsare]-[A-Za-z0-9-]{20,}`), Marker + "slack-token]", nil},
	{regexp.MustCompile(`AKIA[A-Z0-9]{16}`), Marker + "aws-key]", nil},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), Marker + "github-token]", nil},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{50,}`), Marker + "github-token]", nil},
	{regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`), Marker + "gitlab-token]", nil},
	{regexp.MustCompile(`gl(?:rt|soat|ptt)-[A-Za-z0-9_-]{20,}`), Marker + "gitlab-token]", nil},
	passwordRule.rule(Marker + "password]"),
}

// passwordKeys are the words a password's assignment opens with, and THE ONE
// SPELLING of them: [passwordRule], [pendingPassword] and the scan that finds
// where either can begin are all built from this list, so a word added here is
// redacted, held back by [Settled] and scanned for at once, with no second list
// to drift from it.
var passwordKeys = []string{"password", "passwd", "pwd"}

// passwordRule is the one rule besides a private key whose match can cross a
// line break: `\s` is a newline too, so a key on one line and its value on the
// next is one match — which [Settled] has to hold back for.
var passwordRule = keyedOn(passwordKeys, `\s*[:=]\s*\S+`)

// pendingPassword is a password key at the very END of a text, with nothing
// but whitespace or its separator after it: the start of a [passwordRule]
// match whose value has not been written yet.
var pendingPassword = keyedOn(passwordKeys, `\s*(?:[:=]\s*)?\z`)

// keyed is a case-insensitive pattern every match of which opens with one of a
// fixed list of words, together with the scan that finds where those words
// are — both built from that one list by [keyedOn].
//
// THE SHAPE RE2 CANNOT SKIP AHEAD ON. Every other rule opens with a literal —
// `sk-`, `AKIA`, `xox` — which Go's regexp finds with a byte scan at gigabytes
// a second, starting its machine only where one is. A case-insensitive
// alternation has no literal, so the machine stepped through every byte of
// every text this package was handed: over two mebibytes of a coding run's
// transcript, 42 ms of a 45 ms pass (measured, [BenchmarkSecrets]), and 0.7 s
// of it under the race detector — at every one of the places a run's
// transcript and failure are redacted, and on every read of a live view
// through [Settled].
//
// So the words are found by a byte scan for the runes they can open with, and
// the pattern is run only where one is. A word is read as (?i) reads it: each
// of its runes matches every rune in that rune's [unicode.SimpleFold] orbit,
// which is the set regexp/syntax compiles a case-folded literal to — so
// password's s matches ſ as well as S. The words open a group the rest of the
// pattern cannot reach outside, so no match begins anywhere else, and where
// one does, the pattern held to begin there decides exactly what the
// unanchored one would from there. The matches are the pattern's own —
// leftmost, non-overlapping — found in what a byte scan costs, and a text with
// no word in it runs no pattern at all.
type keyed struct {
	words    []string
	firsts   string         // every rune a word opens with, folds included
	pattern  *regexp.Regexp // the pattern, as it reads anywhere in a text
	anchored *regexp.Regexp // the same pattern, held to begin where it is tried
}

// keyedOn is the pattern that opens with one of words, in any case, followed by
// what after matches: `(?i)(?:words…)(?:after)`.
//
// after is compiled on its own first, so one that is not a whole expression —
// an unbalanced `)|other(` that would close the words' group and let a match
// begin outside it — panics here rather than compiling into a pattern the
// scan does not describe. Like [regexp.MustCompile], it runs at package
// initialisation, so a bad word or a bad after is a binary that does not start.
func keyedOn(words []string, after string) *keyed {
	regexp.MustCompile(after)
	quoted := make([]string, len(words))
	var firsts []rune
	for i, word := range words {
		if word == "" {
			panic("redact: a keyed pattern's word is empty, so every place in a text would be one")
		}
		quoted[i] = regexp.QuoteMeta(word)
		first, _ := utf8.DecodeRuneInString(word)
		for f := first; ; {
			if !slices.Contains(firsts, f) {
				firsts = append(firsts, f)
			}
			if f = unicode.SimpleFold(f); f == first {
				break
			}
		}
	}
	source := `(?i)(?:` + strings.Join(quoted, "|") + `)(?:` + after + `)`
	return &keyed{
		words:    slices.Clone(words),
		firsts:   string(firsts),
		pattern:  regexp.MustCompile(source),
		anchored: regexp.MustCompile(`^(?:` + source + `)`),
	}
}

// rule is k as a rule that replaces each of its matches with with.
func (k *keyed) rule(with string) rule {
	return rule{pattern: k.pattern, with: with, keyed: k}
}

// begins is the first place at or after from where one of k's words opens, as
// (?i) reads it, or -1. from is where a rune starts: the start of text, the
// end of a match, or [beyond] a place already tried.
func (k *keyed) begins(text string, from int) int {
	for from < len(text) {
		i := strings.IndexAny(text[from:], k.firsts)
		if i < 0 {
			return -1
		}
		i += from
		for _, word := range k.words {
			if foldedPrefix(text[i:], word) {
				return i
			}
		}
		from = beyond(text, i)
	}
	return -1
}

// next is the first match of k beginning at or after from: leftmost first, as
// k.pattern's own scan finds it, so matches taken one after another from each
// one's end are exactly the non-overlapping matches ReplaceAllLiteralString
// replaces.
func (k *keyed) next(text string, from int) (start, end int, found bool) {
	for i := k.begins(text, from); i >= 0; i = k.begins(text, beyond(text, i)) {
		if loc := k.anchored.FindStringIndex(text[i:]); loc != nil {
			return i, i + loc[1], true
		}
	}
	return 0, 0, false
}

// index is where k's leftmost match in text begins, or -1, allocating nothing.
func (k *keyed) index(text string) int {
	for i := k.begins(text, 0); i >= 0; i = k.begins(text, beyond(text, i)) {
		if k.anchored.MatchString(text[i:]) {
			return i
		}
	}
	return -1
}

// beyond is the place after the rune at i: where a scan resumes once i has
// been tried, so it never starts inside a rune, which the pattern's own scan
// never does either.
func beyond(text string, i int) int {
	_, size := utf8.DecodeRuneInString(text[i:])
	return i + size
}

// foldedPrefix reports whether text opens with word as (?i) reads it: rune for
// rune, each of text's in the [unicode.SimpleFold] orbit of word's.
func foldedPrefix(text, word string) bool {
	for _, w := range word {
		if text == "" {
			return false
		}
		r, size := utf8.DecodeRuneInString(text)
		if !sameFold(r, w) {
			return false
		}
		text = text[size:]
	}
	return true
}

// sameFold reports whether (?i) reads r as w: whether r is w or another rune
// in w's [unicode.SimpleFold] orbit.
func sameFold(r, w rune) bool {
	for f := w; ; {
		if f == r {
			return true
		}
		if f = unicode.SimpleFold(f); f == w {
			return false
		}
	}
}

// The private-key armour lines. ENCRYPTED is PKCS#8's encrypted form, which is
// still the key — under a passphrase that may sit in the same environment. And
// OpenPGP's (RFC 4880 §6.2), what `gpg --export-secret-keys --armor` prints:
// its block is the shape a PEM one is — headers, a blank line, base64 lines —
// with one checksum line after the body, and it matched neither pattern, so
// its whole body was published.
var (
	keyBegin = regexp.MustCompile(`-----BEGIN (?:(?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY|PGP PRIVATE KEY BLOCK)-----`)
	keyEnd   = regexp.MustCompile(`-----END (?:(?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY|PGP PRIVATE KEY BLOCK)-----`)
)

// pemArmour is any PEM armour line — a private key's, or a certificate's, a
// public key's, any block's (RFC 7468's labels). A key's block never runs
// across one, in either direction: what follows it, or comes before it, is
// that block's, and a certificate's body is base64 at the same width as a
// key's.
var pemArmour = regexp.MustCompile(`-----(?:BEGIN|END) [A-Z0-9][A-Z0-9 ]*-----`)

// keyArmour is what every BEGIN and END line above holds, tested before either
// pattern runs: almost no text carries a key, and a substring test costs
// nothing where a pattern execution costs a scan.
const keyArmour = "PRIVATE KEY"

// MaxKeyBlockBytes is the longest a private key's block may run from its
// armour line — past its BEGIN, its body and its END line; before its END,
// read back from it ([readBack]) — in whatever wrapping the text puts round
// it, and still be read as one.
//
// A key's block is BOUNDED BY ITS STRUCTURE, not by this ([keyBlocks]): it ends
// where the lines stop being a key's, and this only says how long a run of
// key-shaped lines can go on. RSA-16384, the largest key anybody issues, is
// about 12.5 KiB of body, and under half as much again with a log's timestamp
// or a reader's line number on every line, or doubled when it is printed
// double-spaced; sixty-four KiB is any key in any wrapping more than twice
// over. What it bounds beyond a key is a stream of key-shaped lines beside an
// armour line, or that an END not written yet could still read back — a
// base64 dump — which is read as a key, and held back from a live view
// ([Settled]), for no further than this.
const MaxKeyBlockBytes = 64 << 10

// minKeyLine is the shortest base64 run read as a FULL line of a key's body.
// Every encoder wraps the body at 64 (PEM) or 70 (OpenSSH) characters, so a
// shorter run is a key's last line or no key at all — a single word such as
// "Done" is base64's alphabet too, and a line of prose is a string of such
// words.
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
		text = r.apply(text)
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
		if r.in(text) {
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
// A KEY IS READ BY ITS STRUCTURE, from whichever armour line anchors it. Its
// block is what RFC 1421 and RFC 7468 say it is: optional `Proc-Type:` and
// `DEK-Info:` headers, a blank line, base64 lines at the encoder's width
// ([minKeyLine]) and one shorter last line — and then its END line, when it
// has one. A key is read FORWARD from its BEGIN ([readKey]), and BACK from an
// END line that no BEGIN's block reached ([readBack]): a body cut off from its
// BEGIN by lines a forward read could not cross, or whose BEGIN is not in the
// text at all — a stream's end read from inside a key.
//
// WHAT ENDS A BLOCK IS PROSE BEFORE ITS BODY, and two interrupting lines in
// it. A line that is not a key's straight after a BEGIN ends that BEGIN's
// block before it has a body, so an END after it closes nothing: that is the
// whole defence against a header with no key under it — a grep for the armour,
// a heredoc's first line, a sentence about keys — and read back from the END,
// the same prose is where the read stops. The first rule here ran lazily from
// a BEGIN to the next END anywhere, and the one after it to any END within a
// bound, WHATEVER lay between — so a grep's match for the BEGIN line and a
// later one for the END took every line of the transcript between them with
// it, and a live view held back everything after the first for as long as an
// END could still come.
//
// Inside a body a block is as forgiving as that defence allows, because what
// a person or another process writes into the middle of a key is not a reason
// to publish the rest of it:
//
//   - any number of BLANK lines, anywhere — a key printed double-spaced, its
//     escaped breaks doubled in a string;
//   - ONE INTERRUPTING LINE between two lines of the key — two full lines,
//     the last full line and the short last one, the short last line and
//     the END — whether it is a log line another process wrote mid-print or
//     a stray word. Two in a row end the block, and an END after them is
//     read back from instead.
//
// A key reaches text in every WRAPPING there is, and each is allowed for:
//
//   - a line break is a real one or one escaped inside a string (`\n`), since
//     a key in a JSON value is the commonest way one reaches a log;
//   - a line may carry the wrapping's per-line PREFIX — a log's timestamp, a
//     reader's line number, a grep's `file:N:`, a diff's `+` — shaped like the
//     armour line's own ([wrapping.prefixOK]);
//   - and its SUFFIX, the text after the armour on that line, repeated on
//     every line (an `echo "…" >> key.pem`, a `"…" +` concatenation);
//   - a line of nothing but punctuation between them is the wrapping's own
//     ([glue]): the `" +` of a string concatenated across lines;
//   - and the whole key may be on one line, its body whitespace-separated
//     base64 between the armours, its headers too.
//
// Read forward, a body line that ends in anything else — the `"}` that closes
// the string a key was written into, the `…` of a cut — ENDS the block at its
// base64, because what follows is the enclosing text.
func keyBlocks(text string) ([][2]int, int) {
	if !strings.Contains(text, keyArmour) {
		return nil, -1
	}
	var spans [][2]int
	// closed is where each END a BEGIN's block reached begins, in order.
	var closed []int
	open := -1
	begins := keyBegin.FindAllStringIndex(text, -1)
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
		if block.closed {
			closed = append(closed, block.endArmour)
		}
		open = -1
		if block.open {
			open = b[0]
		}
	}
	// prev is where the last key armour before the END being read ends — a
	// BEGIN's or an END's — which is as far back as [readBack] has to look
	// for the start of the END's line.
	prev := 0
	for _, e := range keyEnd.FindAllStringIndex(text, -1) {
		for len(begins) > 0 && begins[0][1] <= e[0] {
			prev, begins = max(prev, begins[0][1]), begins[1:]
		}
		for len(closed) > 0 && closed[0] < e[0] {
			closed = closed[1:]
		}
		if len(closed) == 0 || closed[0] != e[0] {
			if start, ok := readBack(text, e, prev); ok {
				spans = append(spans, [2]int{start, e[1]})
			}
		}
		prev = e[1]
	}
	return mergeSpans(spans), open
}

// mergeSpans is spans in order, those that overlap made one: a block read
// back from its END that reached into a block read forward from its BEGIN is
// one key.
func mergeSpans(spans [][2]int) [][2]int {
	if len(spans) < 2 {
		return spans
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s[0] < last[1] {
			last[1] = max(last[1], s[1])
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// keyBlock is what one BEGIN line's block holds.
type keyBlock struct {
	// end is where its span ends, 0 where nothing under the BEGIN line is a
	// key.
	end int
	// open is whether the text ran out while the block was still being
	// read: what is written next could still extend it or close it.
	open bool
	// closed is whether an END line closed it, and endArmour where that
	// END's armour begins.
	closed    bool
	endArmour int
}

// readKey reads the block of the BEGIN armour at b (its start and end in
// text), forward. See [keyBlocks] for the structure it reads.
func readKey(text string, b []int) keyBlock {
	// THE REST OF THE BEGIN LINE — nothing, the wrapping's per-line suffix,
	// or, for a key on one line, its body — read only as far as the FIRST of
	// three places: the line's break, the next armour on the line, and
	// [MaxKeyBlockBytes] past this armour. Every scan here runs once per
	// BEGIN, so each has to stop where the next BEGIN's starts. Scanned to
	// the line's end, a line of many armours costs the armour count times the
	// line: two megabytes of flattened key material, nearly two minutes. The
	// bound alone is not enough either, because a long line reaches it from
	// every armour on it: the armour count times the bound, over thirty
	// seconds for the same two megabytes under the race detector. Stopped at
	// the next armour, the scans of a line's BEGINs cover it once between
	// them — which is why the armour is found first, and the break looked for
	// only in what lies before it.
	hi := min(len(text), b[1]+MaxKeyBlockBytes)
	stop, armour := hi, []int(nil)
	if a := pemArmour.FindStringIndex(text[b[1]:hi]); a != nil {
		armour = []int{b[1] + a[0], b[1] + a[1]}
		stop = armour[0]
	}
	// One byte past stop, so that a break AT the bound ends the line: a rest
	// of exactly [MaxKeyBlockBytes] is one a key may have.
	restEnd, next := breakWithin(text, b[1], min(len(text), stop+1))
	overlong := false
	switch {
	case restEnd >= 0:
		// The line ends before the next armour and the bound.
	case armour != nil:
		return oneLineKey(text, b, armour)
	case hi == len(text):
		// The text runs out on the BEGIN line: what is written next may
		// still go on with it.
		restEnd = len(text)
	default:
		// THE LINE GOES ON PAST THE BOUND, with no break and no armour in
		// it — which is not the text running out. No line after it can be
		// this block's, since a block runs no further than the bound
		// ([keyReader.read] ends one at the first line past it), so the
		// block is what this line holds of a body, and it ENDS here. Read as
		// the text running out — which is what a scan stopped at the bound
		// looks like — a BEGIN followed by a long line of anything would
		// hold every line after it back from a live view for good
		// ([Settled]).
		restEnd, overlong = hi, true
	}
	rest := text[b[1]:restEnd]
	trimmed := trimLine(rest)
	// A MULTI-LINE BLOCK, or a one-line key with no armour after it on its
	// line: now the wrapping's per-line prefix is needed, so it is read here
	// rather than above, where a line of many armours would pay for one per
	// BEGIN.
	r := keyReader{
		wrapping: wrapping{shape: prefixShape(strings.Trim(text[segmentStart(text, b[0]):b[0]], " \t"))},
		text:     text,
		from:     b[1],
	}
	h := skipHeaders(trimmed)
	switch body, end := runChunk(trimmed[h:]); {
	case body:
		// The body begins on the BEGIN line: a key on one line, or its
		// first line glued to the armour.
		r.body, r.end = 1, b[1]+leadingSpace(rest)+h+end
		if lastBase64(trimmed[h+end:]) >= 0 {
			// The enclosing text resumes on the BEGIN line itself.
			return keyBlock{end: r.end}
		}
	case trimmed == "":
	case inBase64(trimmed[0]):
		// The armour line goes on past the armour, with no key.
		return keyBlock{}
	default:
		r.suffix = trimmed
	}
	if overlong {
		return r.ended()
	}
	return r.read(next)
}

// oneLineKey is the block of the BEGIN armour at b when the first thing after
// it on its line that is not its body is another armour, the one at armour: a
// key written on one line, its breaks turned to spaces or removed.
//
// THE FIRST ARMOUR DECIDES, whatever it is, so that two keys flattened onto one
// line (`K1END K2`) each close at their own END. This key's END, with nothing
// but separators between it and the body, CLOSES the block. Any other armour —
// the next key's BEGIN, a certificate's, a public key's — ENDS the block at its
// body, exactly as one on a later line does ([keyReader.read]), and so does
// this key's END once the enclosing text has resumed before it: what follows
// the body on its line does not make the body any less a key's. Giving up on
// the block instead published the whole body — of a key whose END was lost
// and which the next key followed, of one a certificate followed, of one whose
// END came after a word.
func oneLineKey(text string, b, armour []int) keyBlock {
	between := text[b[1]:armour[0]]
	h := skipHeaders(between)
	body, end := runChunk(between[h:])
	switch {
	case !body:
		return keyBlock{}
	case lastBase64(between[h+end:]) < 0 && keyEnd.MatchString(text[armour[0]:armour[1]]):
		return keyBlock{end: armour[1], closed: true, endArmour: armour[0]}
	}
	return keyBlock{end: b[1] + h + end}
}

// wrapping is what a text puts round every line of a key: the armour line's
// per-line prefix, as [prefixShape] has it, and its suffix — the text after
// the armour on that line, where that is the wrapping's rather than a body.
type wrapping struct{ shape, suffix string }

// lineKind is what one line of a key's block is.
type lineKind int

const (
	// otherLine is not a line of the body: prose, a command, a header after
	// the body.
	otherLine lineKind = iota
	// fullLine is a line of the body at the encoder's width ([minKeyLine]).
	fullLine
	// shortLine is a shorter one: the body's last line, or a word.
	shortLine
)

// bodyLine reads one trimmed line, starting at at in the text, as a line of a
// key's body under this wrapping: what kind of line it is, where its base64
// begins and ends, and whether what follows the base64 is the enclosing text
// rather than the wrapping's suffix.
func (w wrapping) bodyLine(at int, line string) (kind lineKind, start, end int, terminated bool) {
	core := line
	if w.suffix != "" && strings.HasSuffix(core, w.suffix) {
		core = strings.TrimRight(core[:len(core)-len(w.suffix)], " \t")
	} else if i := lastBase64(core); i < len(core)-1 {
		// A tail that is not the wrapping's: the enclosing text resumes
		// after the base64.
		core, terminated = core[:i+1], true
	}
	run := trailingRun(core)
	if run == 0 || !w.prefixOK(strings.TrimRight(core[:len(core)-run], " \t")) {
		return otherLine, 0, 0, false
	}
	end = at + len(core)
	if run >= minKeyLine {
		return fullLine, end - run, end, terminated
	}
	return shortLine, end - run, end, terminated
}

// prefixOK reports whether p — the text before a line's base64, trimmed — is
// the wrapping's per-line prefix: nothing, punctuation and spaces alone (a
// string's quote, an indent, a table's bar), or text SHAPED LIKE THE ARMOUR
// LINE'S OWN PREFIX with such punctuation after it. Shaped like, because a
// wrapping repeats its prefix with its numbers moved on: a line number, a
// timestamp, a grep's `file:N:` against its context lines' `file-N-`. And the
// armour line's prefix may go on past the gutter every line shares — a log's
// timestamp and then its message, a reader's line number and then the code
// the key was assigned in — so a line's gutter need only begin it.
//
// What this refuses is the line of PROSE: text before the base64 that the
// armour line did not start with — a command, a sentence — which is what a
// grep's next result or a run's next step looks like.
func (w wrapping) prefixOK(p string) bool {
	last := lastBase64(p)
	if last < 0 {
		return true
	}
	return strings.HasPrefix(w.shape, prefixShape(p[:last+1]))
}

// keyReader walks a key's block forward from its BEGIN line, a line at a time.
type keyReader struct {
	wrapping
	text string
	// from is the end of the BEGIN armour, which [MaxKeyBlockBytes] counts
	// from.
	from int

	// body counts the full lines read, and end is where the block's last
	// line of key ends.
	body, end int
	// sinceFull counts the lines read since the last full line, and
	// interrupted is whether the line read last is one interrupting the key
	// rather than a line of it.
	sinceFull   int
	interrupted bool
}

// read walks the block's lines from pos, the start of the line after the BEGIN
// line (-1 for none), to where the block ends.
func (r *keyReader) read(pos int) keyBlock {
	for pos >= 0 {
		if pos >= len(r.text) {
			return r.ranOut()
		}
		// The line's break is looked for no further than one byte past the
		// bound: a line that has not ended by then ends the block whatever
		// is after it, so reading on to its end would buy nothing but a scan
		// of all of it.
		limit := min(len(r.text), r.from+MaxKeyBlockBytes+1)
		lineEnd, after := breakWithin(r.text, pos, limit)
		if lineEnd < 0 {
			lineEnd, after = limit, -1
		}
		if lineEnd-r.from > MaxKeyBlockBytes {
			return r.ended()
		}
		line := r.text[pos:lineEnd]
		if strings.Contains(line, "-----") {
			if e := keyEnd.FindStringIndex(line); e != nil {
				if !pemArmour.MatchString(line[:e[0]]) && r.closes(pos, line[:e[0]]) {
					return keyBlock{end: pos + e[1], closed: true, endArmour: pos + e[0]}
				}
				return r.ended()
			}
			if pemArmour.MatchString(line) {
				return r.ended()
			}
		}
		trimmed := trimLine(line)
		switch {
		case trimmed == "" || r.blankUnder(trimmed):
			// A blank line, real or escaped or in the wrapping: any number
			// of them, anywhere in the block.
		case glue(trimmed):
			// The wrapping's own punctuation, a line of its own.
		case r.body == 0 && r.header(trimmed):
			// RFC 1421's headers, before the body — and before or after
			// the blank line that should follow them, since a key printed
			// double-spaced has one after each.
		default:
			if !r.take(pos+leadingSpace(line), trimmed) {
				return r.ended()
			}
		}
		pos = after
	}
	return r.ranOut()
}

// take reads one line that is not blank, glue or a header, and reports whether
// the block goes on past it: a full line of the body, its short last line, or
// a line interrupting it. What ends the block is that it has no body yet — the
// defence against a header with no key under it — or that the line would put
// two lines that are not the key's between two that are: a full line and the
// one before it, the short last line and the full line before that.
//
// A short line is read as the body's last for as long as it can be — the
// block's span reaches it — and as a line interrupting the body when a full
// line follows it.
func (r *keyReader) take(at int, line string) bool {
	kind, _, end, terminated := r.bodyLine(at, line)
	switch {
	case r.body == 0:
		if kind != fullLine {
			return false
		}
		r.body, r.end = 1, end
	case kind == fullLine && r.sinceFull <= 1:
		r.body++
		r.end, r.sinceFull, r.interrupted = end, 0, false
	case kind == shortLine && r.sinceFull <= 1:
		r.end, r.interrupted = end, false
		r.sinceFull++
	case !r.interrupted:
		// A line interrupting the key: taken only if a line of it, or the
		// END, follows.
		r.sinceFull++
		r.interrupted = true
		return true
	default:
		return false
	}
	return !terminated
}

// ended is the block as read when a line ends it.
func (r *keyReader) ended() keyBlock {
	if r.body == 0 {
		return keyBlock{}
	}
	return keyBlock{end: r.end}
}

// ranOut is the block as read when the text ends inside it.
func (r *keyReader) ranOut() keyBlock {
	block := r.ended()
	block.open = true
	return block
}

// closes reports whether an END line whose text before the armour is pre ends
// this block: its prefix, or the body's last line written up to the armour.
func (r *keyReader) closes(pos int, pre string) bool {
	trimmed := trimLine(pre)
	switch {
	case trimmed == "" || r.prefixOK(trimmed):
		return r.body > 0
	case inBase64(trimmed[len(trimmed)-1]):
		// Only a line of the body: prose before the armour — a command
		// that names it — is no key's END.
		at := pos + leadingSpace(pre)
		if kind, _, _, terminated := r.bodyLine(at, trimmed); kind == otherLine || terminated {
			return false
		}
		return r.take(at, trimmed) && r.body > 0
	}
	return false
}

// blankUnder reports whether a trimmed line is a blank one in the wrapping:
// nothing but its prefix and its suffix — a reader's line number, a grep's
// context marker, an `echo "" >> key.pem`.
func (r *keyReader) blankUnder(line string) bool {
	if r.suffix != "" && strings.HasSuffix(line, r.suffix) {
		line = strings.TrimRight(line[:len(line)-len(r.suffix)], " \t")
	}
	return trailingRun(line) < minKeyLine && r.prefixOK(line)
}

// keyHeaders are the header names a key's block may carry before its body:
// RFC 1421's, on a traditional encrypted PEM key, and OpenPGP's armour headers
// (RFC 4880 §6.2).
var keyHeaders = []string{"Proc-Type:", "DEK-Info:", "Version:", "Comment:", "MessageID:", "Hash:", "Charset:"}

// header reports whether a line is one of a key's headers ([keyHeaders]),
// under the wrapping's prefix.
func (r *keyReader) header(line string) bool {
	for _, name := range keyHeaders {
		if i := strings.Index(line, name); i >= 0 && r.prefixOK(strings.TrimRight(line[:i], " \t")) {
			return true
		}
	}
	return false
}

// readBack reads the block an END armour at e (its start and end in text)
// closes, BACK from it, where no BEGIN's block reached it: where the span to
// redact begins, and whether there is a key before the END at all.
//
// The same structure as [readKey] reads, from the other end: the body's short
// last line and its full lines, at most one line that is not the key's between
// two that are, blank and glue lines anywhere, in the wrapping of the END
// line's own prefix and suffix. It stops at the first place that structure
// breaks, at another armour line, and [MaxKeyBlockBytes] before the END; the
// span begins at the base64 of the earliest full line, so whatever wrapped it
// — its prefix, the lines before it — is kept.
//
// One line is told apart differently read back: a blank line in the
// wrapping, a line number with nothing after it, is one interrupting the key
// rather than a blank line — because a live view has to hold back every line
// an END not written yet could read back over ([backHold]), and what that
// END's prefix will be is not known until it is written.
//
// READ BACK, A FULL LINE IS ALSO BASE64 THAT READS LIKE A KEY'S ([keyLike]):
// upper and lower case letters and digits together, as the encoding of random
// bytes almost always is. That is what makes reading back affordable for a
// live view ([Settled]): an END not written yet can read back over every line
// that could be its body, so each such line waits to be shown until enough
// lines that cannot be follow it — and a path, a hash or a package name, the
// long base64-alphabet runs a coding run actually prints, carries no case or
// no digits. A real key's line lacks one about once in fifty thousand, and the
// one interrupting line a block may carry is what that costs.
//
// prev is where the last key armour before this END ends, BEGIN or END, or 0:
// the start of the END's line is looked for no further back than that, since
// a line holding another key armour is answered without it. Every unclosed END
// reads back, so on a line of many of them a scan back to the line's start —
// or to the bound, which a long line reaches from every END on it — would cost
// the armour count times the line, or times the bound.
func readBack(text string, e []int, prev int) (int, bool) {
	lineS := segmentStartAbove(text, e[0], max(prev, e[0]-MaxKeyBlockBytes, 0))
	if prev > 0 && lineS == prev {
		// No break since the last key armour: the line holds it, and a key
		// on one line is read from its BEGIN.
		return 0, false
	}
	pre := text[lineS:e[0]]
	if strings.Contains(pre, "-----") && pemArmour.MatchString(pre) {
		// The line holds another armour: a key on one line is read from
		// its BEGIN.
		return 0, false
	}
	restEnd, _ := lineAt(text, e[1])
	suffix := trimLine(text[e[1]:restEnd])
	if suffix != "" && inBase64(suffix[0]) {
		// The armour line goes on past the armour.
		return 0, false
	}
	gutter := strings.Trim(pre, " \t")
	if gutter != "" && inBase64(gutter[len(gutter)-1]) {
		// The body's last line written up to the armour — or a prefix that
		// ends in a letter or a digit, a log's timestamp, which the read
		// after this one takes it for.
		run := trailingRun(gutter)
		end := lineS + leadingSpace(pre) + len(gutter)
		w := wrapping{shape: prefixShape(strings.TrimRight(gutter[:len(gutter)-run], " \t")), suffix: suffix}
		var c backChain
		c.add(backKind(text[end-run:end]), end-run)
		if start, ok := w.readBack(text, lineS, e[0], c); ok {
			return start, true
		}
	}
	w := wrapping{shape: prefixShape(gutter), suffix: suffix}
	return w.readBack(text, lineS, e[0], backChain{})
}

// readBack reads a block back from the line before the one starting at lineS,
// whose END armour begins at armour, continuing the chain c.
func (w wrapping) readBack(text string, lineS, armour int, c backChain) (int, bool) {
	for start := lineS; start > 0; {
		var end int
		start, end = lineBefore(text, start)
		if armour-start > MaxKeyBlockBytes {
			break
		}
		line := text[start:end]
		if strings.Contains(line, "-----") && pemArmour.MatchString(line) {
			break
		}
		trimmed := trimLine(line)
		if trimmed == "" || glue(trimmed) {
			continue
		}
		kind, runStart, runEnd, _ := w.bodyLine(start+leadingSpace(line), trimmed)
		if kind == fullLine && !keyLike(text[runStart:runEnd]) {
			kind = otherLine
		}
		if !c.add(kind, runStart) {
			break
		}
	}
	return c.start, c.body > 0
}

// backKind is what a run of base64 is as a line of a key read back from its
// END ([readBack]).
func backKind(run string) lineKind {
	switch {
	case len(run) >= minKeyLine && keyLike(run):
		return fullLine
	case len(run) >= minKeyLine:
		return otherLine
	}
	return shortLine
}

// backChain is a key's block as read back from its END, a line at a time.
type backChain struct {
	// body counts the full lines read, and start is where the earliest
	// one's base64 begins.
	body, start int
	// tail is what was read before the first full line, nearest the END
	// first: the body's short last line, and lines interrupting the key.
	tail []lineKind
	// gap is whether the line read last since a full line was not one.
	gap bool
}

// add takes the next line back, and reports whether the block can go on past
// it.
func (c *backChain) add(kind lineKind, start int) bool {
	switch {
	case kind == fullLine:
		c.body++
		c.start, c.gap = start, false
	case c.body > 0 && !c.gap:
		c.gap = true
	case c.body > 0:
		return false
	default:
		c.tail = append(c.tail, kind)
		return validTail(c.tail)
	}
	return true
}

// validTail reports whether the lines between an END and the full line before
// it, read back from the END, can be the key's: its short last line and at
// most one line that is not the key's on either side of it. So no more than
// three, never two interruptions in a row, and of three, the middle one the
// short last line. Which short line is the last is not decided by where it is:
// of two, either may be, and the other interrupts. A tail this refuses cannot
// become one it accepts by reading further.
func validTail(tail []lineKind) bool {
	switch len(tail) {
	case 0, 1:
		return true
	case 2:
		return tail[0] == shortLine || tail[1] == shortLine
	case 3:
		return tail[1] == shortLine
	}
	return false
}

// keyLike reports whether a run of base64 reads like the encoding of random
// bytes, as a key's body does: upper and lower case letters and digits
// together. A sixty-four-character line of uniformly random base64 lacks a
// digit with odds of about 2 in 100,000, and either case with odds below one
// in a trillion.
func keyLike(run string) bool {
	var upper, lower, digit bool
	for i := 0; i < len(run); i++ {
		switch c := run[i]; {
		case 'A' <= c && c <= 'Z':
			upper = true
		case 'a' <= c && c <= 'z':
			lower = true
		case '0' <= c && c <= '9':
			digit = true
		}
	}
	return upper && lower && digit
}

// backHold is where the earliest line of text — a text ending on a line break —
// begins that an END not written yet could still read back over ([readBack]),
// or -1.
//
// Every line such an END could take, under ANY wrapping, since the END's
// prefix and suffix are not written yet either. A line with a run of base64 at
// a body's width that reads like a key's ([keyLike]) is one it could take as a
// full line — or, in a wrapping its prefix or suffix does not fit, as a line
// interrupting the key; any other line only as its short last line or an
// interruption, and which of those two does not matter here, since any three
// lines before a full one can be an interruption, the short last line and
// another. So this reads back from the end of text along EVERY reading at once
// — the set of places the real read could be in — and holds from the earliest
// line any of them takes as a full one: never a shorter reach than the real
// read's, whatever the END turns out to be or whatever is written before it.
// One reading is not enough: a key-shaped line taken as full puts the lines
// before it under the body's rule of one interruption, where the same line
// taken as an interruption leaves them the tail's three.
//
// A line it holds is released once four lines that cannot be a body's follow
// it, or two follow the earliest full line of every reading.
func backHold(text string) int {
	// The places a read back can be in: before any full line, with up to
	// three lines read; after one, with no interruption since or one.
	const (
		tail0 uint8 = 1 << iota
		tail1
		tail2
		tail3
		body0
		body1
	)
	interrupted := func(live uint8) uint8 {
		// One more line before a full one — a fourth ends that reading —
		// or the one interruption after a full line, and a second ends
		// that.
		next := (live & (tail0 | tail1 | tail2)) << 1
		if live&body0 != 0 {
			next |= body1
		}
		return next
	}
	live, hold := tail0, -1
	for start := len(text); start > 0 && live != 0; {
		var end int
		start, end = lineBefore(text, start)
		if len(text)-start > MaxKeyBlockBytes {
			break
		}
		line := text[start:end]
		if strings.Contains(line, "-----") && pemArmour.MatchString(line) {
			break
		}
		trimmed := trimLine(line)
		if trimmed == "" || glue(trimmed) {
			continue
		}
		next := interrupted(live)
		if hasKeyRun(trimmed) {
			next |= body0
			hold = start
		}
		live = next
	}
	return hold
}

// hasKeyRun reports whether a line holds a run of base64 at a body's width
// ([minKeyLine]) that reads like a key's ([keyLike]).
func hasKeyRun(line string) bool {
	for i := 0; i < len(line); {
		if !inBase64(line[i]) {
			i++
			continue
		}
		j := i
		for j < len(line) && inBase64(line[j]) {
			j++
		}
		if j-i >= minKeyLine && keyLike(line[i:j]) {
			return true
		}
		i = j
	}
	return false
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
// after them, the body's last, and OpenPGP's checksum after that. It reports
// whether there is such a body, and where it ends. A shorter run before any
// full-width one is a word, and no key's.
func runChunk(s string) (bool, int) {
	long, end := false, 0
	for i := 0; i < len(s); {
		start, j := nextRun(s, i)
		if start == j {
			break
		}
		if j-start < minKeyLine {
			if !long {
				return false, 0
			}
			if k, l := nextRun(s, j); armourChecksum(s[k:l]) {
				return true, l
			}
			return true, j
		}
		long, end, i = true, j, j
	}
	return long, end
}

// nextRun is where the next run of base64 in s at or after i begins and ends,
// both len(s) when there is none.
func nextRun(s string, i int) (int, int) {
	for i < len(s) && !inBase64(s[i]) {
		i++
	}
	j := i
	for j < len(s) && inBase64(s[j]) {
		j++
	}
	return i, j
}

// armourChecksum reports whether a run is OpenPGP's armour checksum: `=` and
// the four characters of a CRC-24 in base64 (RFC 4880 §6.1).
func armourChecksum(run string) bool {
	return len(run) == 5 && run[0] == '='
}

// segmentStart is where the line holding byte i starts, a line break being a
// real one or an escaped one, as [lineAt] has it.
//
// BOUNDED to [MaxKeyBlockBytes] back: the line it starts is an armour line's,
// whose prefix is a wrapping's per-line gutter, and a gutter longer than a key
// block is none. Without the bound, every BEGIN that has no armour after it on
// its line — the last on each line, and each whose next armour is past the
// bound — scans back to the line start they all share, so a long line of
// armours set far apart costs their count times the line. A real wrapping's
// gutter is a handful of bytes, so the bound never bites a key; it only stops a
// pathological flattened line being quadratic. An END looks back no further
// than the key armour before it ([readBack]).
func segmentStart(s string, i int) int {
	return segmentStartAbove(s, i, max(0, i-MaxKeyBlockBytes))
}

// segmentStartAbove is [segmentStart] looking back no further than floor, and
// floor when no break lies between it and i.
func segmentStartAbove(s string, i, floor int) int {
	for j := i - 1; j >= floor; j-- {
		switch {
		case s[j] == '\n':
			return j + 1
		case s[j] == 'n' && j > 0 && s[j-1] == '\\':
			return j + 1
		}
	}
	return floor
}

// lineAt is where the line starting at pos ends, and where the next one
// starts (-1 for none): a line break is "\n", or an escaped one — the two
// characters `\` `n` — inside a string. An escaped break written at the end of
// a real line (a quoted scalar broken across lines with its `\n` kept) is ONE
// break, or every line of it would be followed by an empty one.
func lineAt(s string, pos int) (int, int) {
	if end, next := breakWithin(s, pos, len(s)); end >= 0 {
		return end, next
	}
	return len(s), -1
}

// breakWithin is where the first line break that begins in s[pos:limit] begins,
// and where the line after it starts, a break being what [lineAt] reads as one
// — or -1 and -1 when none begins there. Only where a break BEGINS is bounded:
// the bytes that finish it are read from s whole, so a bound that falls inside
// an escaped break does not split it.
func breakWithin(s string, pos, limit int) (int, int) {
	for i := pos; i < limit; i++ {
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
	return -1, -1
}

// lineBefore is the line before the one starting at start (start > 0): where
// it starts and where it ends, its break as [lineAt] reads breaks — a real
// one, an escaped one, or an escaped one at the end of a real line.
func lineBefore(s string, start int) (int, int) {
	end := start
	switch {
	case s[start-1] == '\n' && start >= 4 && s[start-4:start] == "\\n\r\n":
		end = start - 4
	case s[start-1] == '\n' && start >= 3 && s[start-3:start] == "\\n\n":
		end = start - 3
	case s[start-1] == '\n':
		end = start - 1
	case start >= 2 && s[start-2:start] == `\n`:
		end = start - 2
	}
	return segmentStart(s, end), end
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
// that a later line would have redacted. Four things hold a line back, and
// each is a match that can cross a line break:
//
//   - a private key's block still OPEN — its BEGIN line, and every line
//     after it that a key's block could hold, while the next line written
//     could still be its body or its END ([keyBlocks]). Prose straight after
//     a BEGIN ends its block, so a mere mention of the armour holds back no
//     more than itself; after a body, two lines that are not a key's do;
//   - lines an END NOT WRITTEN YET could read back over ([backHold]): a run
//     of base64 that reads like a key's, and the lines after it a key's
//     block could hold before its END — three that are not a key's release
//     it, and almost nothing a coding run prints is such a run;
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
	back := backHold(whole)
	settled := end
	for {
		moved := false
		if open >= 0 && settled > lineStart(whole, open) {
			settled, moved = lineStart(whole, open), true
		}
		if back >= 0 && settled > lineStart(whole, back) {
			settled, moved = lineStart(whole, back), true
		}
		for _, s := range spans {
			if s[0] < settled && settled < s[1] {
				settled, moved = lineStart(whole, s[0]), true
			}
		}
		if at := pendingPassword.index(whole[:settled]); at >= 0 {
			settled, moved = lineStart(whole, at), true
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
