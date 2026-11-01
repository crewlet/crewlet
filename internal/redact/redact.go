// Package redact is THE secret-redaction pass for free text leaving the
// engine.
//
// Anything the engine ships to a place a person or a model reads — a tool
// result, a harness transcript, a setup-step failure, an event-store
// payload — can carry a credential it never meant to. A cloned repo URL with
// the token in it, a harness that echoes its key on failure, a provisioning
// command whose ${VAR} was resolved before it ran.
//
// There is deliberately only one of these: a second copy is how a surface ends
// up redacting a token shape its neighbour already knows about. It imports
// nothing from the rest of Crewlet so every layer can depend on it.
//
// The patterns are a DENYLIST OF KNOWN CREDENTIAL SHAPES, which bounds what
// this can promise: it catches the vendor prefixes, the engine's own per-run
// tokens and the key formats below, not an arbitrary opaque secret. It is the
// last line, not the first — the first is not putting a credential in the
// text.
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
//
// # What a password's rule still publishes
//
// A password's value is read on its key's line, or on a later one where the
// wrapping every line of the text shares puts it (readValue). What neither
// reading finds is published:
//
//   - a value under a quoted key whose name is more than a name — letters,
//     digits, `_`, `-` and `.` — such as `{"Admin Password": "x"}`: a quote
//     closes the key only where it closes the key's own string
//     ([quotedElsewhere]);
//   - a value `printf '%s\n'` writes a line at a time, every line broken at
//     the escaped break in its own prefix;
//   - a value under a blank line of a wrapping that lost the tab or the space
//     after its gutter — nl's number, a log's timestamp — which is read as the
//     value;
//   - a value shaped like the word the key's line opens with, a space after
//     it on both (`pass2 next` under `pass1 Retype password:`), which is read
//     as the gutter, as one timestamp is under another;
//   - a value on a line that opens with the punctuation the key's line has
//     straight before its key, after words that line does not repeat — a
//     list's dash after a logged message (`msg: - Password:` over
//     `- hunter2x9`), the first line of a string
//     (`{"text": "> Password:\n> hunter2x9"}`) — which is read as the value:
//     the gutter is read from the line's start, and ends where the two lines'
//     words part; and where the markup that closes the key ends its piece of
//     a string (`{"text": "| Password: |\n| hunter2x9 |"}`), no value is read
//     at all;
//   - a value written over several lines — a YAML block scalar, a YAML
//     sequence under the key, a heredoc — of which the first word at most is
//     read, and a diff whose key is on a context line and whose value
//     changed, of which the removed line's first word is read (where the
//     values are indented, that word is the `-` alone, and both are
//     published);
//   - a value under a gutter that holds an armour, or under a blank line of
//     the wrapping whose gutter holds a password's key;
//   - under a wrapping's suffix, a value on a line that holds a password's key
//     of its own (`echo "hunter2 password:" >> f` under
//     `echo "Password:" >> f`), the key's line taking its own punctuation for
//     its value instead, or the closing of the markup that alone ends it
//     (`| Password: |` over `| > Retype password: |`): redacted, the value's
//     line would no longer end as the wrapping's does when the second key's
//     value is replaced, and a second pass would read the first key's value
//     elsewhere;
//   - the part of a value after an escaped break inside it when that part is
//     punctuation alone (`pwd=abc\n!!`), which reads as the string's;
//   - and a key's piece on the line of a value read below a password's key
//     that a key's END line goes on to straight after its armour
//     (`-----END RSA PRIVATE KEY-----|passwd:`). The keys are read with that
//     value claimed, in the text as written, so the piece is no line of a
//     key; the rule reads the value in the text the keys' blocks leave, where
//     the END's line is a marker that can join the value's line to it or
//     take the gutter the END's line opened with, and reads another value,
//     or none. A second pass, the block gone, can read the piece as a key's
//     and redact it: there, redacting twice is not redacting once.
package redact

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// Marker is the prefix every replacement carries, so a reader can tell
// redaction from the original text and so a second pass recognises its own
// work.
const Marker = "[REDACTED:"

// FieldMask is what a masked credential FIELD reads as on every surface that
// serves configuration.
//
// # Why it is here rather than beside the config document
//
// Two packages need it and neither can import the other's copy: the config
// layer's masking pass writes it through [internal/secrets], which config is
// built on, and config's own restore has to recognise it — a write that stored
// it would replace a working credential with these twelve characters,
// silently, and the failure would surface hours later at a vendor naming
// nothing. A constant spelled twice is one that drifts, and the drift here is
// exactly that outage — so it lives in the leaf that already owns what
// redaction looks like on the wire.
//
// A DISTINCTIVE LITERAL rather than an empty string, because the two mean
// opposite things: an operator who deliberately stored an EMPTY credential has
// said something, and a round trip that erased the difference would turn "no
// credential" into "the credential I could not see".
const FieldMask = "__redacted__"

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

	// value is, for a rule whose match names what it replaces rather than
	// being it, where that is: given where a match begins and ends, and every
	// value the rule replaced before it, where the value it names begins and
	// ends — on a later line, it may be — or the match's end twice when it
	// names none, and then the match is none. Nil for a rule that replaces its
	// match. A value is replaced PIECE BY PIECE ([rule.write]).
	value func(text string, start, end int, prior [][2]int) (int, int)
}

// in reports whether the rule replaces anything in text, allocating nothing
// where it matches nothing.
func (r rule) in(text string) bool {
	if r.keyed != nil {
		if r.keyed.index(text) < 0 {
			return false
		}
	} else if !r.pattern.MatchString(text) {
		return false
	}
	_, _, found := r.next(text, 0, nil)
	return found
}

// apply is text with everything the rule replaces replaced, or text itself —
// the same string, nothing copied — when there is nothing.
//
// MATCH BEFORE REPLACING, because a replacement allocates even when it changes
// nothing: ReplaceAllLiteralString with no match still copies the whole input
// into a fresh buffer and then converts that buffer to a string. Ten rules over a
// clean transcript would be twenty full copies of it — and a clean transcript
// is the overwhelming case, since this runs on every sandbox result and every
// coding-run transcript whether or not a credential is in it.
func (r rule) apply(text string) string {
	if r.keyed == nil && !r.pattern.MatchString(text) {
		return text
	}
	// prior is every value replaced so far, which the next one is read
	// beside ([readValue]).
	var prior [][2]int
	start, end, found := r.next(text, 0, nil)
	if !found {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	done := 0
	for found {
		b.WriteString(text[done:start])
		r.write(&b, text[start:end])
		done = end
		if r.value != nil {
			prior = append(prior, [2]int{start, end})
		}
		start, end, found = r.next(text, end, prior)
	}
	b.WriteString(text[done:])
	return b.String()
}

// write writes what the rule makes of replaced, one thing it replaces: its
// marker — or, for a rule with a value, a marker for each PIECE of the value
// between the escaped line breaks in it, the breaks written as they stand.
//
// A value runs on through an escaped break, as `\S+` read it: a password can
// hold a backslash and an `n`, and whether the two are one is not the text's to
// say — `export PASSWORD=Xk9\nQ2#vL8` holds one password, `"pwd=hunter2\nnext"`
// a password and the next line. Replaced whole, the value took the break into
// its marker, two lines became one, and a second pass read a key across them
// that the first had read apart; ended at the break, the rest of a password
// that holds one was published. In pieces, nothing the whole value's marker
// covered is published and every line stays where it was — what a second pass
// reads a key across is what the first did. What it costs is the word after an
// escaped break that is a line's, which is redacted as the whole value took it
// before.
func (r rule) write(b *strings.Builder, replaced string) {
	if r.value == nil {
		b.WriteString(r.with)
		return
	}
	writeValue(b, r.with, replaced)
}

// writeValue writes replaced, a value, as a rule with a value writes it: with
// for each piece of it between its escaped line breaks, the breaks as they
// stand ([rule.write]).
func writeValue(b *strings.Builder, with, replaced string) {
	for replaced != "" {
		piece, brk := replaced, ""
		if i, next := escapedBreak(replaced); i >= 0 {
			piece, brk = replaced[:i], replaced[i:next]
		}
		if piece != "" {
			b.WriteString(with)
		}
		b.WriteString(brk)
		replaced = replaced[len(piece)+len(brk):]
	}
}

// escapedBreak is where the first escaped line break in s begins and ends, or
// -1 and -1: the two characters `\` `n`, as [lineAt] reads one, with the `\r`
// written before them when there is one — CRLF escaped, a break of its own.
func escapedBreak(s string) (int, int) {
	i := strings.Index(s, `\n`)
	if i < 0 {
		return -1, -1
	}
	if i >= 2 && s[i-2:i] == `\r` {
		return i - 2, i + 2
	}
	return i, i + 2
}

// next is what the rule replaces first at or after from: where it begins and
// ends, and whether there is any, prior being every value it replaced before
// from. Taken one after another, each from the end of the one before, these
// are the pattern's own leftmost, non-overlapping matches — or, for a rule
// with a value, the value after each.
//
// WHAT A RULE REPLACES NEVER TAKES WHAT A KEY IS READ ACROSS — a line break,
// an armour line's armour, a key already redacted — because the rules run
// after the key blocks are read, over text a second pass reads them in again.
// A replacement that took a line break made two lines one, and one that took
// an armour or a key's marker took the place a read had stopped at; the
// second pass then read a key across lines the first had read apart, and
// redacting twice was not redacting once. So a password's rule replaces its
// value, never the key or the separator before it, which can run across lines,
// nor the syntax of the wrapping a value on a later line is written in
// ([passwordValue]), and the value around the escaped breaks in it
// ([rule.write]); and a match that runs into an armour — a token's dashes can
// run on into a `-----END` written straight after it — is the pattern's match
// as if the text ended at the armour.
func (r rule) next(text string, from int, prior [][2]int) (int, int, bool) {
	for from <= len(text) {
		start, end, found := r.find(text, from)
		if !found {
			return 0, 0, false
		}
		if r.value != nil {
			if valueStart, valueEnd := r.value(text, start, end, prior); valueEnd > valueStart {
				return valueStart, valueEnd, true
			}
			from = beyond(text, start)
			continue
		}
		at := armourIn(text, start, end)
		if at == end {
			return start, end, true
		}
		// The match as if the text ended at the armour: the pattern's
		// first match there, which no place before start has, since none
		// had one in the whole text. None, and the search goes on past it.
		if loc := r.pattern.FindStringIndex(text[start:at]); loc != nil {
			return start + loc[0], start + loc[1], true
		}
		from = at + pemArmourAt.FindStringIndex(text[at:])[1]
	}
	return 0, 0, false
}

// find is the pattern's first match at or after from.
func (r rule) find(text string, from int) (int, int, bool) {
	if r.keyed != nil {
		return r.keyed.next(text, from)
	}
	loc := r.pattern.FindStringIndex(text[from:])
	if loc == nil {
		return 0, 0, false
	}
	return from + loc[0], from + loc[1], true
}

// armourIn is where the first armour line's armour ([pemArmour]) beginning in
// text[start:end] begins, or end. The armour is read from text whole, past
// end: it holds spaces, which end every match a rule makes, so a match runs at
// most into its first word.
func armourIn(text string, start, end int) int {
	for i := start; i < end; {
		d := strings.Index(text[i:end], "-----")
		if d < 0 {
			return end
		}
		if armourAt(text, i+d) {
			return i + d
		}
		i += d + 1
	}
	return end
}

// armourAt reports whether an armour line's armour ([pemArmour]) begins at i.
func armourAt(text string, i int) bool {
	return strings.HasPrefix(text[i:], "-----") && pemArmourAt.MatchString(text[i:])
}

// rules are applied in order, and order matters where one shape is a prefix of
// another: sk-proj- is checked before the bare sk- that would otherwise
// swallow it and label an OpenAI project key as a plain api-key. It matters
// too where one shape can HOLD another, which is why the run token is first
// ([runTokenRule]).
//
// Go's regexp is RE2: no backtracking, so matching is linear in the input
// whatever the pattern — which matters here, because this pass runs over
// harness transcripts, the largest free text the engine moves. Every rule
// opens with a literal RE2 skips ahead on, or is [keyed] on the words it opens
// with instead.
//
// A PRIVATE KEY IS NOT A RULE HERE. It is the one shape that spans lines, and
// the one whose end may not have been written yet, so it is found by
// [keyBlocks] — before every rule below, so a token shape that happens to occur
// inside a key's body is taken with the block rather than leaving it split.
var rules = []rule{
	runTokenRule,
	token(`sk-proj-[A-Za-z0-9_-]{20,}`, "api-key"),
	token(`sk-ant-[A-Za-z0-9_-]{20,}`, "api-key"),
	token(`sk-[A-Za-z0-9_-]{20,}`, "api-key"),
	token(`xox[bpsare]-[A-Za-z0-9-]{20,}`, "slack-token"),
	token(`AKIA[A-Z0-9]{16}`, "aws-key"),
	token(`gh[pousr]_[A-Za-z0-9]{36,}`, "github-token"),
	token(`github_pat_[A-Za-z0-9_]{50,}`, "github-token"),
	token(`glpat-[A-Za-z0-9_-]{20,}`, "gitlab-token"),
	token(`gl(?:rt|soat|ptt)-[A-Za-z0-9_-]{20,}`, "gitlab-token"),
	passwordRule.valued(passwordMarker, passwordValue),
}

// runTokenRule is the engine's OWN per-run token (internal/runtoken): the
// credential the telemetry receiver's endpoint, the tool bridge's URL and an
// artifact view ticket each carry, `crewlet_rt1.<tag>.<subject>.<expiry>.<sig>`.
//
// BY ITS SHAPE, as a vendor's key is, because there is no value to redact it
// by: no node that collects an executor holds the tokens its launch minted —
// the run's row carries none — and a harness that prints its environment
// or its MCP configuration prints its run's. `crewlet_rt1` is the literal
// runtoken opens every token with for this rule to key on, and runtoken's
// TestAMintedTokenIsRedacted holds the two spellings to one.
//
// EACH SEGMENT AS RUNTOKEN WRITES IT: an eight-character tag (its `tagLen`),
// a Unix expiry, and the HMAC-SHA256 in unpadded base64url, 43 characters. The
// subject is anything but the dot that ends it, whitespace and `[`, and may be
// empty: a token rides a URL path, so no issuer's subject holds whitespace or
// a bracket, and runtoken signs an empty one. Never a bracket, because a
// marker opens with one ([Marker]): a value another rule redacted where a
// subject would stand is then never read, on a second pass, as a subject, so
// redacting twice stays redacting once. And never a line break, so the rule
// is nothing [Settled] has to hold back.
//
// FIRST IN THE TABLE, before the vendors' rules, because its tag, subject and
// signature are base64url or freer, which can spell a vendor's prefix and body
// — an artifact view ticket's subject is base64url of its claims, and any three
// of its characters are `sk-` once in 262 144 — and a token can be written
// straight after a vendor's key. Taken after them, a vendor's rule would
// replace the piece it knows and leave the rest of the token, its signature
// among it, in the text — or read the token's own `crewlet_rt1` as the end of
// the key before it, and leave the rest.
var runTokenRule = token(`crewlet_rt1\.[A-Za-z0-9_-]{8}\.[^.\s\[]*\.[0-9]+\.[A-Za-z0-9_-]{43}`, "run-token")

// token is the rule that replaces each match of pattern with the marker for
// what it is.
func token(pattern, what string) rule {
	return rule{pattern: regexp.MustCompile(pattern), with: Marker + what + "]"}
}

// passwordMarker is what a password's value reads as once it is redacted.
const passwordMarker = Marker + "password]"

// passwordKeys are the words a password's assignment opens with, and THE ONE
// SPELLING of them: [passwordRule], [pendingKey] and the scan that finds where
// either can begin are all built from this list, so a word added here is
// redacted, held back by [Settled] and scanned for at once, with no second list
// to drift from it.
var passwordKeys = []string{"password", "passwd", "pwd"}

// passwordClose is what closes a password's key before its separator, and
// passwordSeparator the separator: the quote or the markup a key is written
// in, and the operator a language assigns or compares it with.
//
// THE SEPARATOR IS WHAT THE TEXT WRITES, and a key and its value are written
// with more than a colon or an equals sign. A JSON object's key is quoted
// (`{"password": "x"}`), a Markdown label bold (`**Password**: x`), and Go
// declares with `:=`, a comparison is `==` and PHP and Ruby map with `=>`.
// Read with a bare `:` or `=` straight after the key, the quoted key and the
// bold one were no key at all, and the value was published; and the second
// character of an operator was taken for the value — `password :=
// "hunter2"` became `password :[REDACTED:password] "hunter2"`. Each operator
// is tried before the one character it opens with, so `==` is never `=`
// followed by a value `=`.
const (
	passwordClose     = "[\"'`*_]{0,2}"
	passwordSeparator = `(?::=|==|=>|[:=])`
)

// quotedOwn is k, a password's pattern, refusing a key closed by a quote that
// does not close the key's own string ([quotedElsewhere]).
func quotedOwn(k *keyed) *keyed {
	k.refuses = quotedElsewhere
	return k
}

// quotedElsewhere reports whether the password's key at at is closed by a
// quote that is not the end of the key's own string: one whose opening quote
// does not stand before the key, or before a name run into it — letters,
// digits, `_`, `-` and `.`, as `"db_password"` and `'reset-password'` are.
//
// A QUOTED KEY IS A STRING THAT IS A NAME. Read wherever a quote closed it,
// every string ending in the key's word was one, so a separator after it named
// the next word as its value: `"POST /auth/password": () =>` lost its `()`,
// `x ? "Choose your password" : "Choose a new password"` the string after it,
// and a translation table every translation of `"... without a password":`.
// Markup is not held to it: `**Admin password**:` is a label with a space in
// it, and so are most. What it costs is a name in a string, or in a code
// span, that is no key where it stands: a choice between two strings that
// opens with one (`masked ? "password" : "text"`) has the other redacted, and
// a shell's pwd in backquotes before a colon, a container's mount, the path
// after it.
func quotedElsewhere(text string, at int) bool {
	i := at
	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !unicode.IsLetter(r) {
			break
		}
		i += size
	}
	q := byte(0)
	for j := i; j < len(text) && j < i+2 && strings.IndexByte("\"'`*_", text[j]) >= 0; j++ {
		if text[j] == '"' || text[j] == '\'' {
			q = text[j]
			break
		}
	}
	if q == 0 {
		return false
	}
	j := at
	for j > 0 && keyName(text[j-1]) {
		j--
	}
	return j == 0 || text[j-1] != q
}

// keyName reports whether b is a byte of a name a quoted key may run into:
// an ASCII letter or digit, `_`, `-` or `.`.
func keyName(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || b == '_' || b == '-' || b == '.'
}

// passwordRule is a password's key and the separator after it: what names the
// value its rule replaces ([passwordValue]). It is the one rule besides a
// private key whose match can cross a line break — `\s` is a newline too, and
// so is an escaped one inside a string (`\n`, or `\r` before it) — and the one
// whose value can be on a later line than its match, so [Settled] has to hold
// it back until that line is written ([valueHolds]).
//
// THE VALUE IS WHAT IS REPLACED, and the key and the separator are left as
// they were, line breaks and all. The rule used to replace the whole match,
// so a value on the next line took the break with it: two lines became one,
// and a second pass read a key across them that the first had read apart.
var passwordRule = quotedOwn(keyedOn(passwordKeys, passwordClose+`\s*`+passwordSeparator+`(?:\s|\\[nr])*`))

// pendingKey is a password key at the very END of a text with nothing but
// what closes it and whitespace after it: a [passwordRule] match whose
// separator may still be written, across the line break (`Enter password`
// and then `= swordfish`).
var pendingKey = quotedOwn(keyedOn(passwordKeys, passwordClose+`\s*\z`))

// passwordPrompt is a password key and its separator at the very end of a line
// ([promptAt]).
var passwordPrompt = quotedOwn(keyedOn(passwordKeys, passwordClose+`\s*`+passwordSeparator+`\s*\z`))

// passwordValue is where the value a [passwordRule] match names begins and
// ends, the match beginning at start and ending at end and prior being every
// value the rule replaced before it, in order — or end twice where it names
// none: [readValue], with nothing written after text.
func passwordValue(text string, start, end int, prior [][2]int) (int, int) {
	v := readValue(text, start, end, prior)
	return v.from, v.to
}

// readValue is the value a [passwordRule] match names, the match beginning at
// start and ending at end, prior being every value the rule replaced before
// it, in order: see [aValue].
//
// A VALUE IS READ WHERE ITS WRAPPING PUTS IT. A password's key and its value a
// line apart are two lines of whatever wraps every line of the text — a
// reader's line numbers, a grep's context marks, a diff's `+`, a log's
// timestamp, a line `echo`ed into a file, a string concatenated across lines,
// a list of strings, a table's rows — and the rule took the value from the
// first word past the separator wherever it stood, across line breaks, so it
// took the wrapping's own syntax for the value: the quote closing an `echo`'s
// string, the next line's number, a blank numbered line, the `"` that opens a
// concatenated string's next line. The value on the line after was published,
// with no key anywhere in the text. So the value is read as the key reader
// reads a key's line, by its machinery ([lineAt], [shapeAt]):
//
//   - What follows the separator on its line — past the markup that closes
//     what the key's line opened before the key, which is the key's
//     ([pastMarkup]) — is the value when its first word holds a letter or a
//     digit, in any script, or opens with an empty quoted string
//     (`password=""`), which is a value. Punctuation alone is the wrapping's
//     SUFFIX — what it writes after every line's content: `"`, `",`,
//     `" >> key.pem`, ` |` — when the first line after it that is not blank
//     ends in the same text, or in what a list's or a concatenation's last
//     line ends in instead ([closingOf]); and the value is on that line,
//     before it. Otherwise the punctuation is the value, as `***` is. Nothing,
//     or an escaped break, sends the read to the lines after, and the rest of
//     a line an escaped break left that opens with punctuation is the suffix
//     too (`"password:\n" >> key.pem`). Markup alone, closing the key's line,
//     is the suffix of a line that ends in its outermost closing too ([outermost])
//     — the rows of a table that closes every cell, `|Password:|` over
//     `|hunter2x9|` — and a line that does not end in it is read as though
//     nothing had followed the key.
//   - The read passes over a blank line, a blank line of the wrapping — its
//     gutter and its suffix alone, `    10→`, `echo "" >> key.pem` — and the
//     punctuation that closes a real line after an escaped break, the `" +` of
//     a string concatenated across lines.
//   - The line it stops at opens with the wrapping's GUTTER, the start of what
//     the key's own line has before the password's key ([gutterLen]), and the
//     value is the first word after it, ended before the suffix.
//   - A line holding a password's key where the value would be — in the
//     gutter, or in the first word after it — ENDS THE READ with no value,
//     since the key on it names what is there: taken as the first key's
//     value, `password:` over `  password: hunter2` hid the second key from
//     the rule, and hunter2 was published. Under a suffix, a line holding a
//     key of its own anywhere ends it, the punctuation being the value: the
//     first pass's replacement of that line's own suffix, the second key's
//     value, would leave a line that no longer ends as the key's did, and a
//     second pass would read the first key's value elsewhere. So does a
//     marker, an armour or a key's marker where the value would begin, none
//     of which a password holds.
//
// THE VALUE ALONE IS REPLACED: the gutter, the suffix and the blank lines
// between stay, so the text keeps its lines and a second pass finds a marker
// where the value was.
func readValue(text string, start, end int, prior [][2]int) aValue {
	sep := start + strings.IndexAny(text[start:end], ":=") + 1
	r := valueRead{text: text, start: start, end: end, from: end, prior: prior}
	if !strings.Contains(text[sep:end], "\n") && !strings.Contains(text[sep:end], `\n`) {
		// The separator's line goes on: what follows it there, past the
		// markup that closes what the line opened, its first word read as
		// the rule's `\S+` read it.
		r.from = pastMarkup(text, start, sep-1, end, prior)
		word := wordEnd(text, r.from)
		switch {
		case word > r.from && isValue(text[r.from:word]):
			return aValue{from: r.from, to: valueEnd(text, r.from), decided: end}
		case word > r.from:
			lineEnd, named := lineEndUnnamed(text, word)
			if named {
				// Another key on the line: a suffix holds none.
				return aValue{from: r.from, to: valueEnd(text, r.from), decided: end}
			}
			r.setSuffix(trimLine(text[r.from:lineEnd]))
		case r.from > end && !strings.HasPrefix(text[r.from:], `\n`):
			// The markup alone ends the key's line.
			markup := strings.TrimRight(text[end:r.from], " \t")
			r.setSuffix(outermost(trimLine(markup)))
			r.markupOnly = true
			r.closer = [2]int{end + len(markup) - len(r.suffix), end + len(markup)}
		}
	}
	lineEnd, next := lineAt(text, sep)
	escaped := escapedOnly(lineEnd, next)
	if r.suffix == "" && escaped {
		var named bool
		if next, escaped, named = r.restOfLine(next); named {
			// Another key on the rest of the line: a suffix holds none.
			return aValue{from: r.from, to: valueEnd(text, r.from), decided: end}
		}
	}
	return r.lines(next, escaped)
}

// outermost is the last closing of markup, the run of its last byte: the
// table cell's `|` of `** |`, or `**` alone. It is what closes the first thing
// the key's line opened, and so the one closing a line of the same wrapping
// writes whatever its content: read as the whole of the markup, the suffix of
// `| **Password:** |` was `** |`, so a table's blank row `|  |` was no blank
// line of it, its pipe was taken for the value and the value on the row
// below published.
func outermost(markup string) string {
	last := markup[len(markup)-1]
	return markup[len(strings.TrimRight(markup, string(last))):]
}

// wordEnd is where the word that begins at from ends, as the rule's `\S+` read
// it: at the next space, or at an escaped break.
func wordEnd(text string, from int) int {
	for from < len(text) && !valueSpace(text[from]) && !strings.HasPrefix(text[from:], `\n`) {
		from++
	}
	return from
}

// pastMarkup is where what follows a password's separator, which begins at
// sep, is read from, the match beginning at start and ending at end: past the
// markup that closes what the key's line opened before the key's label — a
// bold label's `**`, a code span's backtick, a table cell's `|` — and the
// spaces after it, or end where none does. The label is the words the key
// ends ([labelStart]), so `**Root password:**` closes the `**` before Root;
// what closes the key before its separator closes the innermost of what the
// line opened, so `**Root password**: **x**` leaves the value its own; the
// closing mirrors the opening, so `| **Password:** |` closes both; and it ends
// where the markup does, so `"**Password:**",` closes its `**` and its quote
// is what follows.
//
// THE MARKUP IS THE KEY'S, as the quote or the markup closing it before its
// separator is ([passwordClose]): it is what the line says closes around the
// key, not what stands for its value. Read as the value, `**Password:**
// hunter2` and `| Password: | hunter2 |` had the markup replaced and the value
// published; read as the value only where the opening stood straight before
// the key, so did `**Root password:** hunter2`; and read as the line's suffix
// where nothing followed it, the value under `**Password:**`,
// `- **Password:**` or a code span of `password:` was on a line that did not
// end in it, the markup was taken for the value, and the value on the next
// line published. Markup alone, and only the markup the key's own line
// opened: a quote is no markup here even where one opens the key's line —
// `msg="Enter password:" level=info` closes a logged string, and what follows
// it is the next field — and a pipe the line did not open is the shell's
// (`echo Password: | tee -a log`).
//
// The opening is read back from the key over its label alone, and from there
// a byte for each byte of the closing, and not from the start of its line:
// read from the line's start, every key read again all of the line before it,
// so 1,024 keys on a line that opens with 256 KiB cost 43 to 58 times what
// they do after 2 KiB. And it is read as the rule leaves it, the last value
// replaced before the key, prior's, a marker that opens nothing: read as
// written, `| Password: |` under `password:` closed the pipe the first key
// took for its value, and a second pass, a marker where the pipe was, read
// the line's last pipe as the second key's value instead.
func pastMarkup(text string, start, sep, end int, prior [][2]int) int {
	floor := 0
	if len(prior) > 0 {
		floor = prior[len(prior)-1][1]
	}
	j := labelStart(text, floor, start)
	closed := strings.TrimRight(text[start:sep], " \t\r\n")
	k := len(closed)
	for k > 0 && strings.IndexByte("\"'`*_", closed[k-1]) >= 0 {
		k--
	}
	for ; k < len(closed); k++ {
		// The key's own close, innermost first, takes the opening nearest
		// its label.
		for j > floor && isSpace(text[j-1]) {
			j--
		}
		if j == floor || text[j-1] != closed[k] {
			break
		}
		j--
	}
	from := end
	for i := end; ; {
		for i < len(text) && isSpace(text[i]) {
			i++
		}
		for j > floor && isSpace(text[j-1]) {
			j--
		}
		if i == len(text) || j == floor || text[i] != text[j-1] || strings.IndexByte("*_`|", text[i]) < 0 {
			break
		}
		i, j = i+1, j-1
		from = i
	}
	if from == end {
		return end
	}
	for from < len(text) && isSpace(text[from]) {
		from++
	}
	return from
}

// labelStart is where the label a password's key at start ends begins: back
// over the words of it — letters and digits in any script, the spaces between
// them, and the `-`, `'`, `.`, `(`, `)` and `/` a name is written with, and
// `_` inside a word — to floor at most. `**Root password:**` is one label, and
// `db_password` one word, whose `_` opens nothing.
//
// Every key reads back no further than the punctuation before its label, and
// a key's separator is punctuation, so a line of many keys is read back once.
func labelStart(text string, floor, start int) int {
	j := start
	for j > floor {
		r, size := utf8.DecodeLastRuneInString(text[floor:j])
		switch {
		case r == ' ' || r == '\t' || unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-'.()/", r):
		case r == '_' && wordRune(utf8.DecodeLastRuneInString(text[floor:j-size])) && wordRune(utf8.DecodeRuneInString(text[j:])):
		default:
			return j
		}
		j -= size
	}
	return j
}

// aValue is where the value a [passwordRule] match names begins and ends — or
// where the match ends, twice, where it names none — and where the line that
// DECIDED it begins: a text cut at a line start after the key and at or before
// that line reads the value as though nothing were written after the cut
// ([valueHolds]). unread is whether it reads the same there, the value being
// the one the read gives with no line after the key's to decide it
// ([valueRead.unread]). A read no line written yet decides is PENDING: every
// line after the match is one the value may still come after, so a line
// written next can still be the value, and decided is len(text).
type aValue struct {
	from, to, decided int
	unread            bool
}

// valueRead is a [readValue] in progress.
type valueRead struct {
	text string
	// start and end are where the password's match begins and ends, from
	// where what follows its separator is read ([pastMarkup]), and prior the
	// values the rule replaced before it.
	start, end, from int
	prior            [][2]int
	// suffix is the wrapping's suffix, or "": a line that does not end in it
	// is none of the wrapping's lines, and what the key's line holds after its
	// separator is the value, `***` as much as any. closing is what a list's
	// or a concatenation's last line ends in instead ([closingOf]), and
	// appending what a line after the first of a file written a line at a
	// time ends in ([appendingOf]), each or "".
	suffix, closing, appending string
	// markupOnly is whether the suffix is the markup that closes the key's
	// line ([outermost]) and nothing else: the suffix of a line that ends in
	// it, and of no other, which is read as though nothing had followed the
	// key. closer is where that closing stands on the key's line.
	markupOnly bool
	closer     [2]int
	// gutters are what the key's line has before the key ([valueRead.gutter]),
	// read once each: from its piece's start and from its real line's.
	gutters [2]string
	read    [2]bool
}

// setSuffix makes s the read's suffix.
func (r *valueRead) setSuffix(s string) {
	r.suffix, r.closing, r.appending = s, closingOf(s), appendingOf(s)
}

// appendingOf is what every line after the first of a file written a line at a
// time ends in where the first ended in suffix: suffix with each `>` that
// stands alone doubled, or "" where none does.
//
// A file written a line at a time is opened by its first line's `>` and
// appended to by every line after it with `>>`, so read by the suffix alone,
// the value under `echo "Password:" > creds.txt` was on a line that said it
// was no wrapping's (`echo "hunter2x9" >> creds.txt`), the key's quote was
// taken for the value and the value published.
func appendingOf(suffix string) string {
	if strings.IndexByte(suffix, '>') < 0 {
		// The common case, and a suffix can be as long as its line.
		return ""
	}
	var b strings.Builder
	doubled := false
	for i := 0; i < len(suffix); i++ {
		b.WriteByte(suffix[i])
		if suffix[i] == '>' && (i == 0 || suffix[i-1] != '>') && (i+1 == len(suffix) || suffix[i+1] != '>') {
			b.WriteByte('>')
			doubled = true
		}
	}
	if !doubled {
		return ""
	}
	return b.String()
}

// closingOf is what the last line of a list or of a concatenation ends in where
// every line before it ended in suffix: suffix without the `,` or `+` that
// carries it on to the next line, or "" where it ends in neither.
//
// The last element of a list and the last string of a concatenation close
// with the quote alone, so read by the suffix alone, the value on one —
// `"Password:",` over `"hunter2x9"` — was on a line that said it was no
// wrapping's, and the key's quote was taken for the value and the value
// published. And read as any line that closes a string after an escaped
// break, it took a line that does not close as the key's would have: `text
// := "Enter password:\n"` over `fmt.Fprintf(w, "done\n")` lost
// `fmt.Fprintf(w,`.
func closingOf(suffix string) string {
	s := strings.TrimRight(suffix, " \t")
	if s == "" || s[len(s)-1] != ',' && s[len(s)-1] != '+' {
		return ""
	}
	return strings.TrimRight(s[:len(s)-1], " \t")
}

// restOfLine reads the rest of a real line an escaped break ended the key's
// line on, the line starting at pos: the wrapping's suffix when it opens with
// punctuation and holds no key of its own, and the read goes on past it; and
// otherwise a line like any other, the read going on from it. It returns where
// the read goes on, whether the break before that is an escaped one, and
// whether the rest opens with punctuation before a key of its own — which
// makes the punctuation the value, as it is before another key on the key's
// own line ([lineEndUnnamed]).
func (r *valueRead) restOfLine(pos int) (int, bool, bool) {
	lineEnd, next := lineAt(r.text, pos)
	if next >= 0 && r.text[lineEnd] != '\n' {
		// The string goes on to another escaped line: one of its lines.
		return pos, true, false
	}
	rest := trimLine(r.text[pos:lineEnd])
	word, _, _ := strings.Cut(rest, " ")
	if rest == "" || isValue(word) {
		return pos, true, false
	}
	if passwordRule.index(rest) >= 0 {
		return pos, true, true
	}
	r.setSuffix(rest)
	return next, false, false
}

// escapedOnly reports whether the line break ending a line at lineEnd, the
// line after it starting at next ([lineAt]), is an escaped one with no real
// one after it: the string's next line, on the same line of the text. An
// escaped break written at the end of a real line — `password:\n` and then
// the line ends — is the real one: read as the string's, the next real line
// was the rest of the key's, and its punctuation the suffix every line after
// had to end in, so the value two lines down under a diff's `+` was
// published.
func escapedOnly(lineEnd, next int) bool { return next == lineEnd+2 }

// lines reads on from the line starting at pos, escaped being whether the break
// before it is an escaped one with no real one after it: see [readValue].
func (r *valueRead) lines(pos int, escaped bool) aValue {
	// crossed is whether the read has crossed a real line break: whether the
	// line it reads is another line of the wrapping, rather than the next
	// piece of the key's own string ([valueRead.gutter]).
	crossed := !escaped
	for pos >= 0 && pos < len(r.text) {
		lineEnd, next := lineAt(r.text, pos)
		line := r.text[pos:lineEnd]
		trimmed := trimLine(line)
		key := passwordRule.index(line)
		suffixed := r.suffix != ""
		if suffixed && r.markupOnly && trimmed != "" {
			_, suffixed = r.ending(trimmed, lineEnd, next)
		}
		switch {
		case trimmed == "":
		case escaped && wordless(trimmed) && (next < 0 || !escapedOnly(lineEnd, next)):
			// What closes a real line after an escaped break: the `" +` of a
			// string concatenated across lines. The real line may end in an
			// escaped break of its own, as [ending] reads it: read as a line
			// with a value in it, `"+\n` under `Passwd:+` gave its `"` to the
			// value after a line [ending] had read as a blank one of the
			// wrapping because it ended there, and once a pass had replaced
			// the `"`, that line ended in no suffix, and a second pass took
			// the key's own `+`.
		case key >= 0 && suffixed && r.markupOnly:
			// So may it here, and the key's line takes the markup's
			// closing for its value, as it takes punctuation. The markup
			// is the key's, but taken for no value, the key named none:
			// once a pass had replaced the line's own closing, the line
			// below no longer ended as the key's did, and a second pass
			// read the key's value in it, the first word after the
			// gutter (`|Password:|` over `= pwd:|` lost its `=`).
			return r.decided(r.closer[0], r.closer[1], pos, false)
		case key >= 0 && suffixed:
			// The key on the line may replace the word its end begins
			// with, which is what said the line was the wrapping's.
			return r.decided(r.end, r.end, pos, true)
		default:
			lead := leadingSpace(line)
			at := pos + lead
			content := trimDebris(line[lead:])
			if suffixed {
				var ok bool
				if content, ok = r.ending(trimmed, lineEnd, next); !ok {
					return r.decided(r.end, r.end, pos, true)
				}
			}
			rest := strings.TrimLeft(content[gutterLen(content, r.gutter(crossed)):], " \t")
			blank := strings.TrimSpace(rest) == ""
			from := at + len(content) - len(rest)
			to := from
			if !blank {
				to = valueEnd(r.text, from)
			}
			if suffixed {
				to = min(to, at+len(content))
			}
			if key >= 0 && pos+key < to {
				// The key on the line is where the value would be: in
				// the gutter, which the key's own line can have spelled
				// the same way, or in the first word after it.
				return r.decided(r.end, r.end, pos, false)
			}
			if blank {
				// The wrapping's blank line.
				break
			}
			// A marker or an armour where the value would begin ends it
			// there ([valueEnd]), and the value is none.
			return r.decided(from, to, pos, false)
		}
		escaped = escapedOnly(lineEnd, next)
		crossed = crossed || !escaped
		pos = next
	}
	return r.decided(r.end, r.end, len(r.text), true)
}

// ending is a trimmed line's content, the line ending at lineEnd and the next
// starting at next ([lineAt]), when it ends as the key's line did — in the
// wrapping's suffix, in what closes it on a list's or a concatenation's last
// line, or in what appends to the file the key's line opened — and whether it
// does. Where an escaped break ends the line and what follows the break, up to
// the end of the real line, is punctuation alone, that is what it ends in, and
// the line is content up to the break: `\t"hunter2x9\n" +` ends in `" +`, its
// content `"hunter2x9`.
//
// UP TO THE END OF THE REAL LINE, because punctuation a further escaped break
// follows is the string's next line, and may be the value: read as the line's
// ending, `+` in `#\n+\n+` under `#passwd:+` made `#` a blank line of the
// wrapping and was then taken for the value, and once a pass had replaced it,
// `#` ended in no suffix, so a second pass took the key's own `+`.
func (r *valueRead) ending(trimmed string, lineEnd, next int) (string, bool) {
	ends, cut := trimmed, true
	if escapedOnly(lineEnd, next) {
		end, after := lineAt(r.text, next)
		if rest := trimLine(r.text[next:end]); rest != "" && wordless(rest) && (after < 0 || !escapedOnly(end, after)) {
			ends, cut = rest, false
		}
	}
	for _, s := range [...]string{r.suffix, r.closing, r.appending} {
		if s != "" && strings.HasSuffix(ends, s) {
			if cut {
				trimmed = trimmed[:len(trimmed)-len(s)]
			}
			return strings.TrimRight(trimmed, " \t"), true
		}
	}
	return "", false
}

// gutter is what the key's line has before the key ([keyPrefix]) that a line
// the read stops at opens with: from the start of the key's real line where
// the read has crossed a real line break to it, whose gutter is the
// wrapping's, and from the start of the key's piece of its string, after the
// last escaped break, where the read goes on in the same string. Read from the
// piece alone, the key in `"user: bob\npassword:\n"` had nothing before it, so
// on the next real line under a reader's line numbers the number was the
// value, and under a concatenation's lines the `"` that opens the string.
func (r *valueRead) gutter(crossed bool) string {
	k := 0
	if crossed {
		k = 1
	}
	if !r.read[k] {
		lo := segmentStart(r.text, r.start)
		if crossed {
			lo = lineStartAbove(r.text, r.start)
		}
		r.gutters[k], r.read[k] = keyPrefix(r.text, lo, r.start, r.prior), true
	}
	return r.gutters[k]
}

// keyPrefix is text from lo to start — what a key's line has before the key at
// start — AS THE RULE LEAVES IT, every value it replaced there before the key
// (prior) written as the marker it becomes, and with its leading space
// trimmed. Its trailing space stays: whether the key's line has a space
// between its gutter and its key is what says where a word of it ends
// ([gutterLen]).
//
// As the rule leaves it, because a second pass reads it so. Read as written, a
// key whose value is on the next line read that line against the value beside
// it — `old_password="hunter2", new_password=` over `"hunter3x9"`, whose
// opening quote the first value's quote said was the gutter's — and a second
// pass, a marker where the first value was, read the quote as the value and
// replaced it. So the line's earlier values stand in it as their markers, and
// a value under a key is read beside another's: read as written, the value
// under `Password:` on a line that goes on to `Retype password:` was given up
// on, since its first pass changed the gutter `Retype password:` read the
// line below by, and was published.
func keyPrefix(text string, lo, start int, prior [][2]int) string {
	i, _ := slices.BinarySearchFunc(prior, lo, func(v [2]int, at int) int {
		if v[1] <= at {
			return -1
		}
		return 1
	})
	if i == len(prior) || prior[i][0] >= start {
		return strings.TrimLeft(text[lo:start], " \t")
	}
	var b strings.Builder
	at := lo
	for ; i < len(prior) && prior[i][0] < start; i++ {
		from, to := max(prior[i][0], lo), min(prior[i][1], start)
		b.WriteString(text[at:from])
		writeValue(&b, passwordMarker, text[from:to])
		at = to
	}
	b.WriteString(text[at:start])
	return strings.TrimLeft(b.String(), " \t")
}

// lineStartAbove is where the real line holding i starts, looking back no
// further than [MaxKeyBlockBytes], as [segmentStart] does.
func lineStartAbove(s string, i int) int {
	floor := max(0, i-MaxKeyBlockBytes)
	return floor + strings.LastIndexByte(s[floor:i], '\n') + 1
}

// decided is a read's value — from and to, or the unread one
// ([valueRead.unread]) where unread is true — decided by the line beginning at
// pos ([aValue]).
func (r *valueRead) decided(from, to, pos int, unread bool) aValue {
	unreadFrom, unreadTo := r.unread()
	if unread {
		from, to = unreadFrom, unreadTo
	}
	return aValue{from: from, to: to, decided: pos,
		unread: from == to && unreadFrom == unreadTo || from == unreadFrom && to == unreadTo}
}

// unread is the value where no line after the key's holds one: the suffix's
// first word, which is a value where nothing repeats it, or none.
func (r *valueRead) unread() (int, int) {
	if r.suffix == "" {
		return r.end, r.end
	}
	return r.from, valueEnd(r.text, r.from)
}

// isValue reports whether word, the first after a password's separator, is a
// value rather than a wrapping's punctuation: it holds a letter or a digit, in
// any script, or opens with an empty quoted string. In any script, because a
// password's characters are a person's: read in ASCII alone, `Пароль` was
// punctuation, a line below that ended as its own did was its wrapping's,
// and the value was published.
func isValue(word string) bool {
	return strings.HasPrefix(word, `""`) || strings.HasPrefix(word, "''") || !wordless(word)
}

// wordless reports whether s holds no letter and no digit, in any script: the
// punctuation a wrapping writes around a line's content. ASCII is read a byte
// at a time, because what is asked of is often a line's whole rest, and every
// key on such a line asks.
func wordless(s string) bool {
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if isAlnum(rune(c)) {
				return false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
		i += size
	}
	return true
}

// wordRune reports whether r, a rune and its size as utf8 decodes one, is a
// letter or a digit in any script.
func wordRune(r rune, _ int) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isAlnum reports whether r is an ASCII letter or digit.
func isAlnum(r rune) bool {
	return 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9'
}

// valueSpace reports whether c ends a password's value as the rule's `\S+` read
// one.
func valueSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

// lineEndUnnamed is where the line holding from ends, and whether a password's
// key and separator begin on it first.
//
// One scan for both, because a line of many keys asks it of each: a scan to the
// line's end for every key cost the keys times the line, and this one stops at
// the next key.
func lineEndUnnamed(text string, from int) (int, bool) {
	for i := from; ; {
		k := passwordRule.begins(text, i)
		limit := len(text)
		if k >= 0 {
			limit = k
		}
		if b, _ := breakWithin(text, i, limit); b >= 0 {
			return b, false
		}
		switch {
		case k < 0:
			return len(text), false
		case passwordRule.matchesAt(text, k):
			return k, true
		}
		i = beyond(text, k)
	}
}

// gutterLen is how much of content — a trimmed line that may hold a password's
// value — is the wrapping's gutter, prefix being what the key's own line has
// before its password's key ([valueRead.gutter]): the longest start of the
// line SHAPED LIKE the start of prefix ([gutterShapeAt]) that ends where a
// word of both does, and after it the quote the key's line has too.
//
// Shaped like, because a wrapping moves its numbers on: `    10→` under
// `     9→`, `a.pem-10-` under `a.pem:9:`, one timestamp under another, its
// fraction written or not. A start of the key's prefix, because the key's
// line may have words before the key (`Enter the password:`) that the gutter
// every line shares does not.
//
// ENDING WHERE A WORD OF BOTH DOES. A gutter ends at punctuation of its own —
// `→`, `-`, `+`, `:` — or at a space both lines have, and the start of a
// value can be shaped like the key's line: `4567x9` under `3 password:` lost
// `4567`, read as that number's shape, `Enter2x9` under `Enter the` its first
// letters, `2024.Summer!` under `1. Password:` its `2024.`, `10:00:02Xk9…`
// under `10:00:01 password:` its `10:00:`. So where the line spells a word of
// the key's line whole and runs on past the space the key's line has after it,
// nothing inside that word is a gutter's, and the gutter is what ends before
// it. And a line the gutter would be all of, its last character a letter or a
// digit, is no blank line of the wrapping: a value of digits alone, under a
// key whose line opens with a number, read as that number's shape — `839201`
// under `1. Password:`, behind a reader's line number, a diff's `+` or a
// grep's mark as much as bare — was a blank numbered line, read past, and
// published; and a live view held it back for as long as such lines went on.
// A blank line of a wrapping ends in its own punctuation (`    10→`) or in the
// space it writes after its gutter (`    32\t` under `nl`, a log's
// timestamp and its space).
//
// A MARKER IS MATCHED WHOLE, by the same marker, or ends the gutter: a token a
// wrapping writes on every line — a log's prefix holding one — is a marker on
// every line by the time the rule reads them. Matched by its shape, the
// `[REDACTED:api-key]` of the key's line matched the start of the
// `[REDACTED:password]` a first pass wrote over the value below it, and a
// second pass replaced the rest of that marker; stopped at, it read every such
// line's value as none, and published it. A password's marker ends it
// whatever stands below it: it is a value the rule replaced on the key's line,
// never a wrapping's, and matched by the marker the first pass wrote over the
// value below, the value's line read as a blank one of the wrapping on the
// second, and the read went on to the line after it. An armour ends it too,
// since no gutter takes one: taken by a gutter that ends in a dash, `-----END`
// lost its first dash and the rest was the value, which broke the armour with
// a marker.
//
// THE QUOTE AFTER IT, because a string's opening quote is on every line of a
// concatenation (`\t"`) where the key's line opened it after more (`const key
// = "`). A quote and nothing else: read as any punctuation the key's line
// holds, `(9)Xk9…` under `(1) password:` lost its `(`; and never one inside a
// marker, which a key's line holds only where a pass wrote one.
func gutterLen(content, prefix string) int {
	// n is the gutter's end so far, and at where prefix's own reading of it
	// ends; word is where it ended at the last space both lines have, before
	// the word of prefix the reading is in.
	n, at, word := 0, 0, [2]int{0, 0}
	i, g := 0, 0
	for i < len(content) && g < len(prefix) && !armourAt(content, i) {
		if m := markerLen(content[i:]); m > 0 || markerLen(prefix[g:]) > 0 {
			// A marker is matched whole by the same marker, or ends it.
			if m == 0 || strings.HasPrefix(content[i:], passwordMarker) || !strings.HasPrefix(prefix[g:], content[i:i+m]) {
				break
			}
			i, g = i+m, g+m
			n, at = i, g
			continue
		}
		c, ci := gutterShapeAt(content, i)
		w, gi := gutterShapeAt(prefix, g)
		if c != w {
			break
		}
		i, g = ci, gi
		switch {
		case c == ' ':
			// A space both lines have: a word of each ends before it.
			n, at = i, g
			word = [2]int{i, g}
		case i < len(content) && !utf8.RuneStart(content[i]):
			// Inside a rune.
		case !isAlnum(rune(content[i-1])):
			// The gutter's own punctuation ends a word of it.
			n, at = i, g
		}
	}
	if i < len(content) && g < len(prefix) && isSpace(prefix[g]) && !isSpace(content[i]) {
		// The line spells a word of the key's line whole and runs on past it.
		n, at = word[0], word[1]
	}
	q := n
	for q < len(content) && isSpace(content[q]) {
		q++
	}
	k := q
	for k < len(content) && strings.IndexByte("\"'`\\", content[k]) >= 0 {
		k++
	}
	if k > q && strings.Contains(unmarked(prefix[at:]), content[q:k]) {
		n = k
	}
	return n
}

// gutterShapeAt is [shapeAt] as a password's value is read past a gutter by
// ([gutterLen]): a run of digits takes with it the fraction after it, a `.`
// and digits.
//
// Because a log writes a timestamp's fraction only where it is not zero — Go's
// RFC 3339 with nanoseconds drops a zero one, and every trailing zero — so
// one line's stamp is `09:00:00Z` and the next one's `09:00:00.5Z`. Read run
// for run, the gutter ended inside the second, the rest of the stamp was the
// value and the value after it was published.
func gutterShapeAt(p string, i int) (byte, int) {
	c, next := shapeAt(p, i)
	if c == '0' && next+1 < len(p) && p[next] == '.' && '0' <= p[next+1] && p[next+1] <= '9' {
		_, next = shapeAt(p, next+1)
	}
	return c, next
}

// unmarked is s with every marker in it removed.
func unmarked(s string) string {
	if !strings.Contains(s, Marker) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, Marker)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		m := markerLen(s[i:])
		if m == 0 {
			return b.String()
		}
		s = s[i+m:]
	}
}

// markerLen is how long the marker s opens with is ([Marker]), or 0.
func markerLen(s string) int {
	if !strings.HasPrefix(s, Marker) {
		return 0
	}
	return strings.IndexByte(s, ']') + 1
}

// isSpace reports whether c is a space or a tab.
func isSpace(c byte) bool { return c == ' ' || c == '\t' }

// valueEnd is where a password's value that begins at at ends: at the next
// space, as the rule's `\S+` read it, ENDED EARLY at what no password holds and
// a key is read across ([rule.next]) — an armour line's armour, and a marker, a
// key's or a value's already redacted. Taking an armour or a key's marker took
// the place a read had stopped at, and taking a value's own marker replaced it
// again on every pass, so the text never stopped holding something the rule
// would replace ([Contains]).
//
// An escaped line break ends it only where what follows the break, to the next
// space, holds no letter or digit — the quote that closes the string, the
// `" +` of a concatenation, a JSON value's `",` or a string escaped twice's
// `\"}"` — and the backslashes written before the break are the string's too;
// or where the word after the break holds a password's key the value would run
// into ([keyAfterBreak]). Elsewhere the value runs on across one and is
// replaced around it ([rule.write]), since whether the break is a password's
// two characters or the string's next line the text does not say. But
// punctuation alone after it is no password's, and replaced with the value it
// was the wrapping's syntax taken for one: run on across such a break, more of
// the generator's texts were redacted other than exactly, and a second pass
// changed transcripts heavy in password keys.
func valueEnd(text string, at int) int {
	for i := at; i < len(text); i++ {
		switch c := text[i]; {
		case valueSpace(c):
			return i
		case c == '-' && armourAt(text, i), c == '[' && strings.HasPrefix(text[i:], Marker):
			return i
		case c == '\\':
			if n := escapedBreakAt(text, i); n > 0 && (!wordAfter(text, i+n) || keyAfterBreak(text, i+n)) {
				for i > at && text[i-1] == '\\' {
					i--
				}
				return i
			}
		}
	}
	return len(text)
}

// escapedBreakAt is how long the escaped line break beginning at i is — `\n`,
// or `\r` and `\n` — or 0.
func escapedBreakAt(text string, i int) int {
	switch {
	case strings.HasPrefix(text[i:], `\n`):
		return 2
	case strings.HasPrefix(text[i:], `\r\n`):
		return 4
	}
	return 0
}

// keyAfterBreak reports whether the word starting at i, after an escaped break
// inside a value, holds a password's key the value would run into: run on into
// it, as `"pwd=hunter2\npassword: x"` did, the value took the key into its
// marker, and the rule found no key before `x`. A key further along that line
// is the line's own, and the value runs on past the break to the word before
// it: stopped at every line holding a key, `Xk9\nQ2#vL8 pwd=x` published the
// half of its password after the break, and `ab\ncd pwd="` its `cd`.
func keyAfterBreak(text string, i int) bool {
	end, _ := lineAt(text, i)
	line := text[i:end]
	key := passwordRule.index(line)
	if key < 0 {
		return false
	}
	word := strings.IndexFunc(line, func(r rune) bool { return r < utf8.RuneSelf && valueSpace(byte(r)) })
	return word < 0 || key < word
}

// wordAfter reports whether text from i to the next space holds a letter or a
// digit, in any script ([isValue]).
func wordAfter(text string, i int) bool {
	j := i
	for j < len(text) && !valueSpace(text[j]) {
		j++
	}
	return !wordless(text[i:j])
}

// trimDebris is s without the debris of its line break — a real CR, an escaped
// one, the backslashes a string escaped twice leaves — as [trimLine] has it, and
// with its trailing spaces: a blank line of a wrapping that writes one after
// its gutter is that gutter alone only with it (`    32\t` under `nl`, a log's
// timestamp and a space).
func trimDebris(s string) string {
	for {
		t := strings.TrimSuffix(strings.TrimRight(s, "\r\\"), `\r`)
		if t == s {
			return s
		}
		s = t
	}
}

// valueHolds are the password keys in text whose value is decided on a line
// after the key's, or not yet: where each begins, and its value as
// [readValue] reads it. text is what the rule reads, the keys' blocks and the
// tokens already replaced ([passwordInput]); a value decided on the key's own
// line is cut from it by no line start, and is no hold. The keys are read one
// after another as the rule takes them ([rule.next]), each beside the values
// read before it.
//
// ONE READ OF THE WHOLE TEXT, AND NOT ONE OF EACH PREFIX A HOLD CUTS IT AT.
// [Settled] used to ask which key a prefix left pending, and a key decided by
// the line after it — the next line holding a key, where its own ended in
// punctuation — was pending there, so the hold moved up a line, made the key
// above pending in its turn, and held back the whole text — a live view of it
// showed nothing — reading it again for each line: over 32 KiB in lines of 256
// bytes, 47 to 75 times what [Secrets] costs over it.
func valueHolds(text string) []valueHold {
	valueHoldsRead.Add(int64(len(text)))
	var holds []valueHold
	var prior [][2]int
	for from := 0; ; {
		start, end, ok := passwordRule.next(text, from)
		if !ok {
			break
		}
		v := readValue(text, start, end, prior)
		if strings.IndexByte(text[start:v.decided], '\n') >= 0 {
			holds = append(holds, valueHold{start: start, decided: v.decided, unread: v.unread})
		}
		from = beyond(text, start)
		if v.to > v.from {
			prior = append(prior, [2]int{v.from, v.to})
			from = v.to
		}
	}
	if k := pendingKey.indexAtEnd(text); k >= 0 {
		holds = append(holds, valueHold{start: k, decided: len(text)})
	}
	return holds
}

// valueHoldsRead counts the bytes [valueHolds] has been handed, for the test
// that holds [Settled] to one read of its text. That property was timed
// first, against [Secrets] and then against Settled itself over a text eight
// times smaller, and both ratios were a claim about how busy the runner was:
// each passed on a quiet machine and failed in CI's race job beside the other
// packages. A count of what the text cost to read is the property itself. It
// is one atomic add a call, and Settled makes one call, or two where the keys
// were read beside the values.
var valueHoldsRead atomic.Int64

// valueHold is a password key [valueHolds] reports: where it begins, where the
// line that decided its value begins, and whether a text cut before that line
// reads the same value ([aValue]).
type valueHold struct {
	start, decided int
	unread         bool
}

// passwordInput is text as the password's rule reads it in [Secrets]: its keys'
// blocks — spans, [keyBlocks] — and every token the rules before the
// password's match replaced by their markers, in their order.
//
// READ AS THE RULE READS IT, because a marker is what decides a read: a line
// a key's block was replaced on ends in the wrapping's suffix where the raw
// line did not (`[REDACTED:private-key]" >> key.pem`), and the value a
// password's key named there was none, where read off the raw text it was the
// suffix's first word — so a live view cut between the two showed `"` replaced
// that the whole text left as it was.
func passwordInput(text string, spans [][2]int) written {
	w := written{text: text}.replace(spans, keyMarker)
	for _, r := range rules {
		if r.value != nil {
			break
		}
		if !r.pattern.MatchString(w.text) {
			continue
		}
		var matches [][2]int
		for start, end, found := r.next(w.text, 0, nil); found; start, end, found = r.next(w.text, end, nil) {
			matches = append(matches, [2]int{start, end})
		}
		w = w.replace(matches, r.with)
	}
	return w
}

// written is a text some ranges of which were replaced by markers, and where
// each marker stands: where it begins and ends in text, and where the range it
// replaced began and ended in the text it was written from.
type written struct {
	text  string
	edits [][4]int
}

// replace is w with each of ranges — in order, apart, and none of them in a
// marker — replaced by with.
func (w written) replace(ranges [][2]int, with string) written {
	if len(ranges) == 0 {
		return w
	}
	var b strings.Builder
	b.Grow(len(w.text))
	edits := make([][4]int, 0, len(w.edits)+len(ranges))
	old, shift, done := w.edits, 0, 0
	for _, r := range ranges {
		for ; len(old) > 0 && old[0][0] < r[0]; old = old[1:] {
			edits = append(edits, [4]int{old[0][0] + shift, old[0][1] + shift, old[0][2], old[0][3]})
		}
		edits = append(edits, [4]int{r[0] + shift, r[0] + shift + len(with), w.at(r[0]), w.at(r[1])})
		shift += len(with) - (r[1] - r[0])
		b.WriteString(w.text[done:r[0]])
		b.WriteString(with)
		done = r[1]
	}
	for _, e := range old {
		edits = append(edits, [4]int{e[0] + shift, e[1] + shift, e[2], e[3]})
	}
	b.WriteString(w.text[done:])
	return written{text: b.String(), edits: edits}
}

// at is where place i of w.text was written from: in the text a marker
// replaced, where what it replaced began.
func (w written) at(i int) int {
	k, _ := slices.BinarySearchFunc(w.edits, i+1, func(e [4]int, i int) int { return cmp.Compare(e[0], i) })
	if k == 0 {
		return i
	}
	e := w.edits[k-1]
	if i < e[1] {
		return e[2]
	}
	return i - e[1] + e[3]
}

// keyed is a case-insensitive pattern every match of which opens with one of a
// fixed list of words, together with the scan that finds where those words
// are — both built from that one list by [keyedOn].
//
// THE SHAPE RE2 CANNOT SKIP AHEAD ON. Every other rule opens with a literal —
// `sk-`, `AKIA`, `xox` — which Go's regexp finds with a byte scan at gigabytes
// a second, starting its machine only where one is. A case-insensitive
// alternation has no literal, so the machine stepped through every byte of
// every text this package was handed: over two mebibytes of an executor's
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
	widest   int            // the most bytes a word takes, written in any case
	pattern  *regexp.Regexp // the pattern, as it reads anywhere in a text
	anchored *regexp.Regexp // the same pattern, held to begin where it is tried
	// refuses, where it is set, is whether a match the pattern finds at a
	// place is no match all the same — what a regular expression cannot say,
	// since it is about what stands before the match ([quotedElsewhere]).
	refuses func(text string, at int) bool
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
	widest := 0
	for i, word := range words {
		if word == "" {
			panic("redact: a keyed pattern's word is empty, so every place in a text would be one")
		}
		quoted[i] = regexp.QuoteMeta(word)
		width := 0
		for _, r := range word {
			most := 0
			for f := r; ; {
				most = max(most, utf8.RuneLen(f))
				if f = unicode.SimpleFold(f); f == r {
					break
				}
			}
			width += most
		}
		widest = max(widest, width)
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
		widest:   widest,
		pattern:  regexp.MustCompile(source),
		anchored: regexp.MustCompile(`^(?:` + source + `)`),
	}
}

// rule is k as a rule that replaces each of its matches with with.
func (k *keyed) rule(with string) rule {
	return rule{pattern: k.pattern, with: with, keyed: k}
}

// valued is k as a rule that replaces with with the value after each of its
// matches, where value says that ends ([rule.value]).
func (k *keyed) valued(with string, value func(text string, start, end int, prior [][2]int) (int, int)) rule {
	r := k.rule(with)
	r.value = value
	return r
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
// replaces — less any it refuses ([keyed.refuses]), after which the scan goes
// on from the next place, as it does from a place no match begins at.
func (k *keyed) next(text string, from int) (start, end int, found bool) {
	for i := k.begins(text, from); i >= 0; i = k.begins(text, beyond(text, i)) {
		if loc := k.anchored.FindStringIndex(text[i:]); loc != nil && !k.refused(text, i) {
			return i, i + loc[1], true
		}
	}
	return 0, 0, false
}

// matchesAt reports whether k's match begins at i in text, allocating nothing.
func (k *keyed) matchesAt(text string, i int) bool {
	return k.anchored.MatchString(text[i:]) && !k.refused(text, i)
}

// refused reports whether k refuses a match the pattern finds at i.
func (k *keyed) refused(text string, i int) bool {
	return k.refuses != nil && k.refuses(text, i)
}

// index is where k's leftmost match in text begins, or -1, allocating nothing.
func (k *keyed) index(text string) int {
	for i := k.begins(text, 0); i >= 0; i = k.begins(text, beyond(text, i)) {
		if k.matchesAt(text, i) {
			return i
		}
	}
	return -1
}

// indexAtEnd is [keyed.index] for a pattern held to the end of the text whose
// words each end in a letter and whose after matches no letter or digit, as
// [passwordPrompt]'s and [pendingKey]'s do: the word of a match then ends at
// the text's last letter or digit, so a word opening further back than the
// widest word's width of it is never tried.
//
// TRIED AT THE END ALONE, because the pattern held to begin at a word is run
// once a word: a line of keys, each with a value after it, ran it at every key
// to find the one at its end, and [Settled] asks that of every line it reads
// back across ([backHold], [promptAt]) and of the whole text ([valueHolds]) —
// over 32 KiB in lines of `x pwd=`, half of what it cost.
func (k *keyed) indexAtEnd(text string) int {
	last := strings.LastIndexFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
	if last < 0 {
		return -1
	}
	from := max(0, last+1-k.widest)
	for from > 0 && !utf8.RuneStart(text[from]) {
		from--
	}
	for i := k.begins(text, from); i >= 0 && i <= last; i = k.begins(text, beyond(text, i)) {
		if k.matchesAt(text, i) {
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

// pemArmourAt is [pemArmour] held to begin where it is tried.
var pemArmourAt = regexp.MustCompile(`^` + pemArmour.String())

// keyMarker is what a private key's block reads as once it is redacted, and it
// stands where the block's armour stood: a block never runs across it either,
// in either direction ([armoured]). Read as any other line, it let a second
// pass over redacted text read a key back from a later END across the place
// the first pass had stopped at — the END line the marker replaced — and
// redact the lines above it with the key, so redacting twice was not
// redacting once.
const keyMarker = Marker + "private-key]"

// armoured reports whether s holds what a key's block does not run across: a
// PEM armour line's armour ([pemArmour]), or a redacted key's [keyMarker].
func armoured(s string) bool {
	return strings.Contains(s, keyMarker) || strings.Contains(s, "-----") && pemArmour.MatchString(s)
}

// marked reports whether s, the text before an armour on its line or after
// it, holds a marker: an ARMOUR LINE HOLDING ONE HOLDS NO KEY read across its
// lines — only one written on it, read back from an END as far as the marker,
// whose words are no piece of a key ([lineBack]).
//
// A marker beside an armour is what a pass leaves around an armour it read no
// key from — had it read one, the armour would be inside the key's marker — and
// a second pass has to read none there either: redacting twice is redacting
// once ([Secrets]). Read again, the line is in a wrapping a rule or another
// key's redaction has rewritten. Where a password's value or a token stood
// before the armour, or another key's body and END, is now a marker, so the
// prefix every line of the wrapping is read under is another text: a line the
// first pass read as that prefix alone — a blank line in the wrapping — the
// second read as a line of a body, and redacted a key the first had not.
// Taking the prefix only as far as the marker was not enough, for the same
// reason: a shorter prefix leaves fewer lines blank in the wrapping, and more
// of them lines of a body. A text no pass has redacted holds a marker only
// where it quotes one, so in a text of a key this reads nothing less.
func marked(s string) bool { return strings.Contains(s, Marker) }

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
// A key's body is wrapped at 64 characters (PEM as OpenSSL and Go write it,
// PKCS#8, OpenPGP), 70 (OpenSSH) or 76 (MIME's base64), so a shorter run is a
// key's last line or no key at all — a single word such as "Done" is base64's
// alphabet too, and a line of prose is a string of such words. Read forward
// from a BEGIN, a full line is any run this long, whatever wrote it; only a
// read back from an END asks for a width ([atWidth]).
const minKeyLine = 40

// maxKeyLine is the widest line an encoder writes a key's body in, MIME's 76
// under a diff's `+` ([atWidth]).
const maxKeyLine = 76 + 1

// Secrets replaces known credential patterns with markers.
//
// REDACTING TWICE IS REDACTING ONCE, which matters because this runs at more
// than one layer and a transcript can pass through both — the coordinator
// redacts what a runner already did. The keys are read before the rules run,
// so what the rules write has to leave a second pass nothing new to read: no
// rule replaces a marker; a key's marker ends a block where the armour it
// replaced did ([keyMarker]); what a rule replaces takes no line break, no
// armour and no marker ([rule.next]); and an armour line holding a marker,
// before its armour or after it, holds no key read across its lines
// ([marked]), and one written on it is read back from its END no further
// than the marker ([lineBack]). The one exception is the last the package
// doc lists as still published: a key's piece on the line of a value read
// below a password's key that a key's END line goes on to, which a second
// pass can redact.
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
			b.WriteString(keyMarker)
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
//     read back from instead — across them, to a line of the key at an
//     encoder's width above them ([backReading]), so a key read from inside,
//     its BEGIN not in the text, is redacted whole broken by two lines as
//     well, and by two and one more elsewhere. That bounds what this can
//     promise: where three lines in a row, two whose nearest full line above
//     is at no encoder's width, or two and two more with nothing of the key
//     but its short last line between them part a key, the part above them
//     is published when its BEGIN is not in the text, and the part below
//     them when it holds no full line — the body's short last line alone,
//     BEGIN or none.
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
// Read forward, the short line that ends in anything else — the `"}` that
// closes the string a key was written into, the `…` of a cut — ENDS the block
// at its base64, because it is the body's last and what follows is the
// enclosing text; after a full line, what follows it on its line ends nothing
// ([keyReader.take]). Except a password's key and its separator
// (`Password:`), which is a line interrupting the key, so its value is never
// cut off from it ([wrapping.bodyLine]).
//
// A CUT IS READ WHEREVER IT LANDS. A tool that bounds what it shows keeps a
// key's text up to a point, from a point, or both with its mark between them —
// `[...truncated]`, `(truncated)`, `[… 812 bytes truncated]`, `…` — on one
// line, on every line, or every line cut short with no mark at all; so a line
// of the key can carry text that is no key's after its base64, before it, or
// in the middle of it, and so can a key written on one line. What tells that
// line from a line of prose is the piece of the key it still holds
// ([claims.piece]) where the wrapping puts a key's line, at either end of it
// ([wrapping.cutInto]) — never the mark, which nothing spells the same way
// twice — and once a piece has said so, the key's text runs to the line's
// other end where that reads like a key's ([wrapping.cutStart]). Such a line
// is a full line of the body however narrow, read forward and back, and so is
// a short line that is a piece where the body's short last line cannot stand:
// straight after the BEGIN, with text after it, or above another such line,
// read forward ([keyReader.take]); anywhere, read back, at the width the cut
// hid ([asAnyWidth]). And a key on one line is read back from its END on that
// line as it is read forward from its BEGIN ([lineBack]). That bounds what this
// can promise about a cut: a piece that does not read like a key's — shorter
// than eight characters, or holding no digit or only one case — is published
// where it is all a line holds of the key, and the line is one that is not the
// key's, so straight after the BEGIN it ends the block before it has a body,
// and two of them together end a block anywhere: the key's lines below are
// published unless an END reads back to them. So is a piece between two marks,
// at neither end of its line. So, on a key written on one line, is the part
// across a second cut, and across a cut with no run of the key at a full
// line's width beyond it. With no END below, a mark on a line of its own after
// a head cut short is a line interrupting the key after its short last line,
// so the tail it kept and the key's lines after it are published. And read
// from inside, a key cut short on every line and broken by two lines another
// process wrote is published above them: a bridge lands on a line whose width
// a cut hid only from a line at an encoder's width, and no line of that key is
// at one ([backReading]).
func keyBlocks(text string) ([][2]int, int) {
	spans, open, _ := readKeys(text)
	return spans, open
}

// readKeys is [keyBlocks], with the claims the blocks were read beside, or nil
// where text holds no key's armour.
func readKeys(text string) ([][2]int, int, *claims) {
	if !strings.Contains(text, keyArmour) {
		return nil, -1, nil
	}
	var spans [][2]int
	// closed is where each END a BEGIN's block reached begins, in order.
	var closed []int
	open := -1
	c := &claims{text: text}
	begins := keyBegin.FindAllStringIndex(text, -1)
	for _, b := range begins {
		if len(spans) > 0 && b[0] < spans[len(spans)-1][1] {
			// Inside the block before it. Part of that key, not one of
			// its own.
			continue
		}
		block := readKey(text, b, c)
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
	c.take(spans)
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
			if start, ok := readBack(text, e, prev, c); ok {
				spans = append(spans, [2]int{start, e[1]})
			}
		}
		prev = e[1]
	}
	return mergeSpans(spans), open, c
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
// text), forward, c being what the rules claim in text. See [keyBlocks] for the
// structure it reads.
func readKey(text string, b []int, c *claims) keyBlock {
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
	// A redacted key's marker is an armour here too, and is looked for only
	// before the armour found, for the same reason.
	if m := strings.Index(text[b[1]:stop], keyMarker); m >= 0 {
		armour = []int{b[1] + m, b[1] + m + len(keyMarker)}
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
		return oneLineKey(text, b, armour, c)
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
	gutter := strings.Trim(text[segmentStart(text, b[0]):b[0]], " \t")
	r := keyReader{
		wrapping: wrapping{shape: prefixShape(gutter), claims: c},
		text:     text,
		from:     b[1],
	}
	h := skipHeaders(trimmed)
	at := b[1] + leadingSpace(rest)
	switch body, end := runChunk(trimmed[h:], at+h, c); {
	case body:
		// The body begins on the BEGIN line: a key on one line, or its
		// first line glued to the armour.
		r.body, r.end = 1, at+h+end
		if lastBase64(trimmed[h+end:]) >= 0 {
			// The enclosing text resumes on the BEGIN line itself.
			return keyBlock{end: r.end}
		}
	case marked(gutter), marked(trimmed):
		// A line a pass has already redacted around.
		return keyBlock{}
	case trimmed == "":
	case inBase64(trimmed[0]):
		// The armour line goes on past the armour in base64, with no key.
		return keyBlock{}
	default:
		if ts, te, _ := (wrapping{}).tailRun(trimmed); c.piece(at+ts, at+te) {
			// A cut written between the armour and its line break, the
			// tail of a line of the body kept after its mark: the body
			// begins on the BEGIN line. Taken for the wrapping's suffix,
			// it left the block with no body when its END came, and the
			// END reads back no further than an armour line, so the tail
			// was published: 58 texts of 83,419 cut keys.
			r.body, r.end = 1, at+te
			break
		}
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
func oneLineKey(text string, b, armour []int, c *claims) keyBlock {
	between := text[b[1]:armour[0]]
	h := skipHeaders(between)
	body, end := runChunk(between[h:], b[1]+h, c)
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
// the armour on that line, where that is the wrapping's rather than a body —
// together with what the rules claim in the text ([claims]).
type wrapping struct {
	shape, suffix string
	claims        *claims
}

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
//
// A RUN ANOTHER RULE CLAIMS ANY OF IS NO LINE OF THE BODY ([claims]),
// whatever it ends in. Read as the body's last line written up to the
// enclosing text, a password's key — `Password:`, `pwd :`, a `pwd=` whose
// value is on the next line — went into the key's block and left its
// separator and its value outside it, where the password's rule no longer
// finds a key, so a key whose END was never written published the value that
// the same lines with no key above them redact. Read as a line interrupting
// the key, the key and its value are redacted with the key when a line of the
// key follows them, the value read as the short last line, and left to the
// password's rule when none does. An `=` that ends a run was read as base64's
// padding rather than a separator, so `pwd=` alone on a line was the key's
// short last line: over passwords and tokens under a key whose END was never
// written, 630 texts of 3,312 published the value on the line after it, every
// one a password whose `=` ends its line. What that costs is a key's last line
// ending in a password's key and `=` — about one key in fifty thousand — when
// no END follows it: it is published.
func (w wrapping) bodyLine(at int, line string) (kind lineKind, start, end int, terminated bool) {
	start, end, terminated = w.tailRun(line)
	if start == end || !w.prefixOK(strings.TrimRight(line[:start], " \t")) || w.claims.names(at+start, at+end) {
		return otherLine, 0, 0, false
	}
	if end-start >= minKeyLine {
		return fullLine, at + start, at + end, terminated
	}
	return shortLine, at + start, at + end, terminated
}

// tailRun is where a trimmed line's OWN run of base64 begins and ends, the run
// [wrapping.bodyLine] reads the line by — on a line ending in the wrapping's
// suffix, the run before the suffix, and on any other its last run — and
// whether it is the enclosing text rather than the suffix that follows it.
func (w wrapping) tailRun(line string) (start, end int, terminated bool) {
	end = len(line)
	if w.suffix != "" && strings.HasSuffix(line, w.suffix) {
		end = len(strings.TrimRight(line[:end-len(w.suffix)], " \t"))
	} else if i := lastBase64(line); i < end-1 {
		end, terminated = i+1, true
	}
	return end - trailingRun(line[:end]), end, terminated
}

// headRun is where the run of base64 on a trimmed line that the wrapping's
// prefix ends at begins and ends — the last of its runs with nothing but the
// prefix before it ([wrapping.prefixOK]) — or 0 and 0.
//
// THE PREFIX'S SHAPE IS BUILT AS THE LINE IS READ, a run at a time, rather
// than asked of [wrapping.prefixOK] before each run: that shapes the whole
// line up to the run again, so on a line whose prefix goes on matching — a
// gutter of numbers, one per run — a line cost its runs times its length, and
// 256 KiB of such lines took 4.7 s in lines of 30 KiB against 0.17 s in lines
// of one. No run is merged into the shape of the text before it, since a run
// ends at what is no base64 — no digit and no space — so the shape of the line
// up to a run's end is the shape up to the run before it and the shape of
// what lies between, appended.
func (w wrapping) headRun(line string) (int, int) {
	start, end := nextRun(line, 0)
	if start == end {
		return 0, 0
	}
	// shaped is how much of the wrapping's prefix shape the line up to end
	// spells.
	for from, shaped := 0, 0; ; {
		seg := prefixShape(line[from:end])
		if !strings.HasPrefix(w.shape[shaped:], seg) {
			return start, end
		}
		next, nextEnd := nextRun(line, end)
		if next == nextEnd {
			return start, end
		}
		from, shaped, start, end = end, shaped+len(seg), next, nextEnd
	}
}

// cutInto reports whether a trimmed line that is no line of a key's body by
// its own run ([wrapping.bodyLine]) is one A CUT WROTE INTO: it holds a piece
// of a key ([fragment]) where the wrapping puts a key's line — at the run its
// prefix ends at ([wrapping.headRun]), which a cut that kept the start of a
// key's line leaves before its mark, or at the run its suffix begins after
// ([wrapping.tailRun]), which a cut that kept the end of one leaves after its
// mark. A cut that kept both leaves both, its mark between them.
//
// AT THE LINE'S TWO ENDS AND NOWHERE ELSE ON IT, because what reads like a
// piece of a key is a property of a run and prose has runs with it — a Go
// test's name with a digit in it, a digest in base64, a login — and what a cut
// writes is a line of a key with a mark at one end or in the middle of it,
// never a line of prose. Read anywhere on the line, each such line beside a key
// was read as one of its lines: over keys of every kind in every wrapping, with
// prose around them one line in three of which held such a run, 9,586 texts of
// 19,800 lost prose that this package had taken from 748. Read at the line's
// two ends, 3,988 did, and not one text of 83,419 cut keys was redacted less.
// What prose still loses is a line that opens with such a run after the
// wrapping's prefix or ends in one, `=== RUN   TestSHA256Sum`, which is what a
// line a cut kept looks like as well.
func (w wrapping) cutInto(at int, line string) bool {
	hs, he := w.headRun(line)
	ts, te, _ := w.tailRun(line)
	return w.claims.piece(at+hs, at+he) || w.claims.piece(at+ts, at+te)
}

// cutStart is where the key's text on a line a cut wrote into
// ([wrapping.cutInto]), which starts at at in the text, begins — the run its
// prefix ends at, when that reads like a key's ([claims.keyText]), and its
// first piece otherwise — and the encoder's width that run is at, if it is at
// one ([atWidth]). cutEnd is where that text ends: the run its suffix begins
// after, when that reads like a key's, and its last piece otherwise.
//
// THE PIECE SAYS THE LINE IS ONE A CUT WROTE INTO, AND THE KEY'S TEXT RUNS TO
// ITS ENDS. The digit a piece is asked for is what keeps a line of prose from
// being read as a key's ([fragment]); once a piece at one end has said the line
// is a key's, the run at its other end is a key's too when it reads like one —
// a head or a tail of fewer than forty characters lacks a digit often, an RSA
// key's first sixteen never hold one (`MIIEogIBAAKCAQEA`) — and taken only to
// the piece, it was published beside the key it was cut from: 58 texts of
// 83,419 cut keys. A mark's own words — `truncated`, `812`, `TRUNCATED` — are
// in one case or none.
func (w wrapping) cutStart(at int, line string) (int, readAs) {
	if hs, he := w.headRun(line); w.claims.keyText(at+hs, at+he) {
		return at + hs, atWidth(line[hs:he])
	}
	start, _ := w.claims.firstPiece(at, line)
	return start, 0
}

// cutEnd is where the key's text on a line a cut wrote into ends: see
// [wrapping.cutStart].
func (w wrapping) cutEnd(at int, line string) int {
	if ts, te, _ := w.tailRun(line); w.claims.keyText(at+ts, at+te) {
		return at + te
	}
	return w.claims.lastPiece(at, line)
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
	// rather than a line of it. shortPiece is whether it is a short line that
	// is a piece of a key ([claims.piece]), read as the body's last.
	sinceFull   int
	interrupted bool
	shortPiece  bool
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
				if !armoured(line[:e[0]]) && r.closes(pos, line[:e[0]]) {
					return keyBlock{end: pos + e[1], closed: true, endArmour: pos + e[0]}
				}
				return r.ended()
			}
		}
		if armoured(line) {
			return r.ended()
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
//
// A LINE A CUT WROTE INTO IS A FULL LINE OF THE BODY ([wrapping.cutInto]): a
// key's line with a tool's mark after the head it kept, before the tail it
// kept, or between the two. Its key material runs to the run its suffix begins
// after when that reads like a key's, and to its last piece otherwise
// ([wrapping.cutEnd]), and the block goes on from it as from any full line:
// below a mark that kept the key's tail are the key's lines, and below one that
// kept only its head is whatever the tool printed next — taken as the body's
// short last line when it is one word, as after any full line, and otherwise
// as a line interrupting the key, which ends the block unless a line of the key
// follows it. The line's own run is its last, so on
// `MHcCAQEE…49[...truncated]` it was `truncated`, the line no key's, and the
// head the cut kept was published.
//
// And what follows a FULL line on its line ends nothing: only the body's last
// line is short, so what a tool wrote after a full one — the mark of a viewer
// that cut every line at its width — does not say the key is over.
//
// A SHORT LINE THAT IS A PIECE OF A KEY ([claims.piece]) IS A FULL LINE where
// the body's short last line cannot stand. Straight after the BEGIN, it is the
// body's first line cut short: read as the last, the block ended with it, so a
// key cut on every line to under forty characters with no mark and no END was
// published below its first line — 9,600 texts of 9,900 against 1,158 now —
// and 128 texts of 83,419 cut keys with it; what that costs is the word under
// a BEGIN whose one line of body is such a piece, 45 texts of 19,800 with
// prose around a key (977 against 932). With text after it, it is the head a
// viewer kept before a mark in punctuation (`MIIEow…`), and the body's last
// line ends in text where the key was written into some — the quote closing
// its string — so the two read alike; read as the last, a key cut on every
// line before a bare ellipsis ended at its second line, and with no END below
// the rest of it was published: 152 texts of the cut keys. What that costs is
// prose after a key with no END whose lines each open in a run reading like a
// piece and end in punctuation, read as the key's lines cut so: over prose one
// line in three of which opens so, 606 lines more of 39,427 were lost (14,254
// against 13,648); over other prose and the trees, none.
//
// AND A SHORT PIECE BELOW ANOTHER makes the one above a line of the body: a
// body has one short last line, so two short lines in a row that are both
// pieces of a key are two of its lines a viewer cut short, not its last line
// and a word. Read as the last line and a line interrupting the key, a key cut
// on every line to under forty characters with no mark and no END was read
// over three of its lines: 6,775 texts of 9,900 published the rest, against
// 1,158. What it costs is prose after a key with no END whose lines are each a
// lone such run — a list of ids or of test names — taken as the key's lines as
// far as it goes: where every line of the prose is one, 14,667 lines more of
// 69,415 were lost (67,126 against 52,459); over prose with such a line one
// in three, 485 (14,912 against 14,427).
func (r *keyReader) take(at int, line string) bool {
	kind, start, end, terminated := r.bodyLine(at, line)
	piece := kind == shortLine && r.claims.piece(start, end)
	switch {
	case kind == otherLine && r.cutInto(at, line):
		kind, end, terminated = fullLine, r.cutEnd(at, line), false
	case piece && (r.body == 0 || terminated):
		kind, terminated = fullLine, false
	case piece && r.shortPiece:
		// The short line before it was no last line: the body goes on
		// after it, so it was a line of it a cut left short.
		r.body++
		r.sinceFull, r.interrupted = 0, false
	}
	r.shortPiece = false
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
		r.end, r.interrupted, r.shortPiece = end, false, piece
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
	return !terminated || kind == fullLine
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
func (w wrapping) blankUnder(line string) bool {
	if w.suffix != "" && strings.HasSuffix(line, w.suffix) {
		line = strings.TrimRight(line[:len(line)-len(w.suffix)], " \t")
	}
	return trailingRun(line) < minKeyLine && w.prefixOK(line)
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
// The structure [readKey] reads, from the other end — the body's short last
// line and its full lines, at most one line that is not the key's between two
// that are, blank and glue lines anywhere, in the wrapping of the END line's
// own prefix and suffix — and one thing more, which a read forward never does:
// it crosses TWO lines in a row that are not the key's to a full line at an
// encoder's width above them, a BRIDGE ([backReading]), since with the BEGIN
// not in the text nothing else reads the lines above them. It stops where
// neither the structure nor a bridge goes on, at another armour line, and
// [MaxKeyBlockBytes] before the END; the span begins at the base64 of the
// earliest full line — on a line a cut wrote into, at the key's text it holds
// ([wrapping.cutStart]) — so whatever wrapped it, its prefix, the lines before
// it, a tool's mark, is kept. And where no line above the END's is read — the
// END's line holds the BEGIN, another armour or a marker, or goes on past the
// armour in base64, or no line above it reads as a key's — the END's line is
// read alone, as a key written on one line ([lineBack]).
//
// THE END'S OWN LINE IS READ AS ITS PREFIX FIRST. What comes before the armour
// may instead be the body's last line written up to it — a diff's `+` is a
// line of base64 and a prefix both, a log's timestamp ends in a digit — and
// the line does not say which. Read as the prefix, the END's line spends none
// of the block's tail, and its prefix, all of what comes before the armour,
// begins with the one the other reading is left with, so it admits every line
// that one does ([wrapping.prefixOK]): whenever it finds a body it reaches at
// least as far back, and the other reading adds only what it alone can find, a
// body of one line written up to its armour. The read used to try the last
// line first and keep the first body it found, so a `+` taken for the short
// last line spent the tail on the END's own line, a line that reads like a
// key's written between the key's real last line and its END was taken for the
// block's one full line, and every line above it was published.
//
// One line is told apart differently read back: a blank line in the
// wrapping, a line number with nothing after it, is one interrupting the key
// rather than a blank line — because a live view has to hold back every line
// an END not written yet could read back over ([backHold]), and what that
// END's prefix will be is not known until it is written. So read back, a key
// printed double-spaced under such a wrapping and broken by one line as well
// is broken by three, which no bridge crosses ([backReading]), and with no
// BEGIN in the text the lines above the break are published ([keyBlocks]).
//
// READ BACK, A FULL LINE IS ALSO BASE64 THAT READS LIKE A KEY'S ([keyLike]):
// upper and lower case letters together, as the encoding of random bytes all
// but always is. That is what keeps reading back affordable for a live view
// ([Settled]): an END not written yet can read back over every line that could
// be its body, so each such line waits to be shown until enough lines that
// cannot be follow it — and a hash in hex, a path or a package name in lower
// case, the commonest long base64-alphabet runs an executor prints, are shown
// at once.
//
// A DIGIT IS NOT ASKED FOR. A line of random bytes' base64 lacks one about once
// in fifty thousand, and a line a key's encoding fixes in part far more often:
// a last line of forty-odd characters, an EC key's second line, whose first
// twelve characters are its encoding's and hold no digit. Asked for, a line
// without one read back as a line interrupting the key — the one a block may
// carry between two of its lines, which a key printed double-spaced under a
// reader's line numbers, a grep's context marks or a log's timestamps has
// already spent on every blank line in the wrapping, and a key broken by
// another process's lines spends beside them. So the read stopped at that line
// and published every line above it, and a body with no other line read back
// was published whole. Lacking a case, a sixty-four-character line of random
// base64 is rarer than one in a trillion.
//
// WHAT NOT ASKING COSTS is the runs of letters in both cases, as wide as a
// key's line, that an executor does print: a Go test's name — most of this
// repository's are forty characters or more, in both cases with no digit — a
// CamelCase identifier, a path in mixed case. Each reads like a key's line
// now. A live view holds it as it holds one ([backHold]), so a verbose test
// run's output waits, as far back as [MaxKeyBlockBytes], until lines holding no
// such run release it; and a few of them directly above a bare END are read as
// that END's body and redacted with it.
//
// A line that reads like a key's may still be one another process wrote, so
// it is read both ways at once — a full line, and a line interrupting the key
// — and the block reaches as far back as either reading does ([backReading]).
//
// prev is where the last key armour before this END ends, BEGIN or END, or 0:
// the start of the END's line is looked for no further back than that, since
// a line holding another key armour is answered without it. Every unclosed END
// reads back, so on a line of many of them a scan back to the line's start —
// or to the bound, which a long line reaches from every END on it — would cost
// the armour count times the line, or times the bound.
func readBack(text string, e []int, prev int, c *claims) (int, bool) {
	lineS := segmentStartAbove(text, e[0], max(prev, e[0]-MaxKeyBlockBytes, 0))
	if start, ok := linesBack(text, e, prev, lineS, c); ok {
		return start, true
	}
	// Nothing above the END's line is read as a key's: the block is the
	// END's line alone, a key's body written on it when that is what comes
	// before the armour.
	if start := lineBack(text[lineS:e[0]], lineS, c); start >= 0 {
		return lineS + start, true
	}
	return 0, false
}

// linesBack is where the block the END armour at e closes begins, read back
// across the lines above the END's line, which starts at lineS ([readBack]),
// and whether there is one. The END's line is read first: where it holds the
// key armour before it (prev) or another armour, a marker, or goes on past
// the armour in base64, no line above it is.
func linesBack(text string, e []int, prev, lineS int, c *claims) (int, bool) {
	pre := text[lineS:e[0]]
	switch {
	case prev > 0 && lineS == prev, armoured(pre):
		// No break since the last key armour, or another armour on the
		// line: a key on one line is read from its BEGIN.
		return 0, false
	case marked(pre):
		// A line a pass has already redacted around.
		return 0, false
	}
	// The suffix is looked for only now, past the line's armours: on a line
	// of many of them, a look from each to the line's end would cost their
	// count times the line.
	restEnd, _ := lineAt(text, e[1])
	suffix := trimLine(text[e[1]:restEnd])
	if marked(suffix) || suffix != "" && inBase64(suffix[0]) {
		// Redacted around, or going on past the armour in base64, as
		// [readKey] reads a BEGIN line that does: no wrapping's suffix.
		return 0, false
	}
	gutter := strings.Trim(pre, " \t")
	return wrapping{shape: prefixShape(gutter), suffix: suffix, claims: c}.readBack(text, lineS, e[0])
}

// lineBack is where a key's body written on one line ends at the end of s, the
// text before an END armour on that line, which starts at at in the text,
// begins — read back from the armour as [runChunk] reads one forward:
// OpenPGP's checksum, the body's short last run, its runs at the encoder's
// width, and before those one more run that is a piece of a key
// ([claims.piece]), what a cut kept of a run whose start it left out — or -1.
// With no run at the encoder's width, the piece the END follows, or the one
// before the body's short last run, is a body a cut kept: what a window that
// opened on the body's last run leaves.
//
// A KEY ON ONE LINE WAS READ ONLY FROM ITS BEGIN, and back from an END only
// when the whole of its body was one run written up to the armour. A key on one
// line is the line a tool cuts — an environment dump, a JSON value with its
// breaks turned to spaces, both longer than any width a viewer keeps — so a
// window that opened inside it, or a mark a tool wrote between its head and its
// tail, left every run between the cut and the END published. It reads across
// one stretch of text that is neither — a word, a cut's own mark — to the
// key's runs before it, as [pastCut] reads forward ([beforeCut]) — and where
// nothing written up to the armour reads as the key's, only to a run as wide
// as an encoder writes — and no further, so a key two cuts left on one line is
// read only up to the second;
// and never across an armour or a marker, so a marker a rule writes to the
// left of where it stopped changes nothing it reads: a second pass reads what
// the first did.
func lineBack(s string, at int, c *claims) int {
	start := uncutRunsBack(s, at, c)
	from := start
	if from < 0 {
		// Nothing written up to the armour reads as the key's: the cut
		// may be there.
		from = len(s)
	}
	if earlier := beforeCut(s, from, at, c, start < 0); earlier >= 0 {
		return earlier
	}
	return start
}

// uncutRunsBack is [lineBack]'s body as far back as the first text in it that
// is no key's, or -1.
func uncutRunsBack(s string, at int, c *claims) int {
	i := len(s)
	// prev is the run of base64 that ends nearest before i.
	prev := func() (int, int, bool) {
		start, end := runBefore(s, i, 0)
		i = start
		return start, end, start < end
	}
	start, end, ok := prev()
	if ok && armourChecksum(s[start:end]) {
		start, end, ok = prev()
	}
	if !ok {
		return -1
	}
	lastStart, lastEnd := start, end
	short := end-start < minKeyLine
	if short {
		start, end, ok = prev()
	}
	body := -1
	for ok && c.full(at+start, at+end) {
		body = start
		start, end, ok = prev()
	}
	switch {
	case ok && end-start < minKeyLine && c.piece(at+start, at+end) && (body >= 0 || short):
		return start
	case body < 0 && short && c.piece(at+lastStart, at+lastEnd):
		return lastStart
	}
	return body
}

// readBack reads a block back from the line before the one starting at lineS,
// whose END armour begins at armour: where the base64 of the earliest line a
// reading takes as a full one begins, and whether one does.
func (w wrapping) readBack(text string, lineS, armour int) (int, bool) {
	r, from := tailNone|fromEnd0, -1
	for start := lineS; start > 0 && r != 0; {
		var end int
		start, end = lineBefore(text, start)
		if armour-start > MaxKeyBlockBytes {
			break
		}
		line := text[start:end]
		if armoured(line) {
			break
		}
		trimmed := trimLine(line)
		if trimmed == "" || glue(trimmed) || promptAt(trimmed) >= 0 {
			continue
		}
		as, runStart := w.readLine(text, start+leadingSpace(line), trimmed)
		var reached bool
		if r, reached = r.past(as); reached {
			from = runStart
		}
	}
	return from, from >= 0
}

// readLine is what a trimmed line starting at at can be read back as, and where
// the span begins when a reading reaches it: its run of base64, as
// [wrapping.bodyLine] and [readRun] read it — or, on a line a cut wrote into
// ([wrapping.cutInto]), a full line of the key from where the key's text on it
// begins ([wrapping.cutStart]), at the encoder's width its head is at when it
// is at one and at any width otherwise ([asAnyWidth]).
//
// A LINE A CUT WROTE INTO IS A FULL LINE WHICHEVER END OF IT THE CUT KEPT, and
// the readings go on above it: what is above it is the line before it in the
// key, whole or cut the same way, as what is below one read forward is the
// line after it ([keyReader.take]). A cut that kept a line's end leaves text
// before the key's that no wrapping writes — `[… 812 bytes truncated]q3O4Ou…`
// — and read as the first line such a cut kept, where the readings above it
// ended, a key cut so on every line, as a viewer that shows each line's end
// writes it, was read back over the line above it and no further: of 130,680
// texts of keys cut the same way on every line in every wrapping, 5,511 more
// published eight characters of the key, and of 40,695 cut every other way,
// 225. AT ANY WIDTH where its head is at none, since the width the line was
// written at is what the cut took: read at none — a line a cut wrote into, or
// a short piece, at the width of its run — a bridge across two lines another
// process wrote landed on nothing above them, and of 3,059 texts of a key
// broken by two such lines with a line beside them cut short, read from
// inside, 2,895 published the key above the two against 261.
//
// What it costs is prose straight above a key read from inside, or above a
// window a cut kept, whose lines each open or end in a run reading like a
// piece of a key (`=== RUN   TestSHA256Sum`): taken up to where such lines
// stop. Over prose one line in three of which is such a run alone, 511 lines
// more of 39,427 were lost (14,912 against 14,401), and 612 where the run is
// anywhere on the line (5,812 against 5,200); over other prose, and over this
// repository's tree and the other codebases it was read against, none.
func (w wrapping) readLine(text string, at int, line string) (readAs, int) {
	kind, start, end, _ := w.bodyLine(at, line)
	if w.blankUnder(line) {
		// Nothing but the wrapping's prefix and suffix: its runs are the
		// prefix's own, never a piece of a key.
		return readRun(kind, text[start:end], false), start
	}
	if kind != otherLine {
		return readRun(kind, text[start:end], w.claims.piece(start, end)), start
	}
	if w.cutInto(at, line) {
		start, width := w.cutStart(at, line)
		if width == 0 {
			width = asAnyWidth
		}
		return asFull | asOther | width, start
	}
	return asOther, 0
}

// readRun is what a line read back can be, given its kind as
// [wrapping.bodyLine] reads one, its run of base64 and whether that run is a
// piece of a key ([claims.piece]): a full line that reads like a key's
// ([keyLike]) is a full line — at an encoder's width when it is at one
// ([atWidth]) — or a line interrupting the key, and one that does not is a
// line interrupting it; a short line is the body's last or a line
// interrupting it, and one that may be OpenPGP's checksum is read as that too
// ([checksumRun]), and one that is a piece of a key as a full line as well, at
// the width a cut hid ([asAnyWidth]) — a line of a key cut short with no mark
// ([wrapping.readLine]).
func readRun(kind lineKind, run string, piece bool) readAs {
	switch {
	case kind == shortLine && checksumRun(run):
		return asShort | asChecksum
	case kind == shortLine && piece:
		return asShort | asFull | asAnyWidth
	case kind == shortLine:
		return asShort
	case kind == fullLine && keyLike(run):
		return asFull | asOther | atWidth(run)
	}
	return asOther
}

// atWidth is the encoder's width a full line's run is at, as a reading reads
// it ([asAt64], [asAt70], [asAt76]), or none. A key's body is wrapped at one
// width, all but its last line: 64 characters as OpenSSL, Go and GnuPG write
// it (PEM, RFC 7468; PKCS#8; OpenPGP's armour), 70 as OpenSSH does, and 76 as
// MIME's base64 does — what Java's MIME encoder, Python's
// base64.encodebytes and .NET's InsertLineBreaks write, and so what a key
// written by hand with one of them is wrapped at. A diff's `+` before a line
// is base64, so the run is one longer there, and starts with it.
func atWidth(run string) readAs {
	n := len(run)
	if n == 65 || n == 71 || n == 77 {
		if run[0] != '+' {
			return 0
		}
		n--
	}
	switch n {
	case 64:
		return asAt64
	case 70:
		return asAt70
	case 76:
		return asAt76
	}
	return 0
}

// checksumRun reports whether a short line's run is OpenPGP's armour checksum
// ([armourChecksum]), under a diff's `+` too.
func checksumRun(run string) bool {
	return armourChecksum(run) || len(run) == 6 && run[0] == '+' && armourChecksum(run[1:])
}

// backReading is the set of places a read back from an END can be in, a bit
// for each: a SET, because what a line is can turn on lines not read yet, and
// a reading that had to choose would stop where the other goes on. [readBack]
// and [backHold] run the same readings — the one over what each line is under
// the END's wrapping, the other over everything it could be under any — which
// is what makes a live view's hold reach as far back as the read it waits for.
//
// BEFORE ANY FULL LINE a reading is the TAIL read so far, nearest the END
// first: the body's short last line, and at most one line that is not the
// key's on either side of it. Which short line is the last is not decided by
// where it is: of two, either may be, and the other interrupts. So no more than
// three, never two interruptions in a row, and of three, the middle one the
// short last line — seven tails, and a line none of them can take ends the
// reading.
//
// AFTER A FULL LINE a reading is whether the line read last since a full line
// was not one: the one line interrupting the key a block may carry between two
// of its own, which a second ends.
//
// ACROSS TWO LINES a reading is a BRIDGE. Two lines in a row that are not the
// key's end every reading above them, so the lines of a key above two another
// process wrote were read from its BEGIN or not at all: read from inside — a
// stream's end, a window of a log — they were published, and so was a short
// last line the two cut off from the body, wherever the BEGIN was. A bridge
// goes on across exactly two lines, from where a key's lines stand below them,
// to a FULL LINE AT AN ENCODER'S WIDTH above them ([atWidth]), and lands there:
// a full line the span reaches back to, which the readings go on from as from
// any other, so the two are redacted with the key, as one line another process
// wrote into it is. From a full line it lands on one at that line's width,
// since all of a key's lines are one width but its last; from the END itself,
// or from a line that may be the body's last, short or full, on one at any of
// them. A bridge from the END carries the tail a reading does above it: the
// body's last line, and one line interrupting the key between that and the
// line it lands on. It crosses no three lines in a row and lands on no line at
// another width: a CamelCase name or a path in mixed case as wide as a key's
// line is all but never exactly as wide as one, so ordinary text is not
// bridged to, and the live view, which has to hold every line a bridge could
// land on, holds almost nothing it did not ([backHold]).
//
// OPENPGP'S CHECKSUM LINE IS READ AS THE END IS. It is `=` and a CRC-24, the
// block's line after the body's last and before the END, and no line of the
// body. Read only as a short line, it was the body's last or a line
// interrupting the key, and spent the one a block may carry: one line another
// process wrote between it and the body's last line ended every reading, and
// two below it cut a bridge off from the body. So a reading that reaches it
// with nothing of the key read since the END — a line interrupting it, or the
// two a bridge crossed — also starts again above it, as from the END
// ([asChecksum]).
type backReading uint32

const (
	// tailNone has read nothing since the END.
	tailNone backReading = 1 << iota
	// tailS has read the short last line.
	tailS
	// tailO has read a line interrupting the key.
	tailO
	// tailSS has read two short lines, either of them the last.
	tailSS
	// tailSO has read the short last line, then a line interrupting the
	// key.
	tailSO
	// tailOS has read a line interrupting the key, then the short last line.
	tailOS
	// tail3 has read three lines, the middle one the short last line.
	tail3
	// body0 has read a full line, and nothing since; body1 one line since.
	body0
	body1

	// fromEnd0 is a bridge from the END itself, fromEnd1 and fromEnd2 one and
	// two lines past it: on any width.
	fromEnd0
	fromEnd1
	fromEnd2
	// aboveLast0 is a bridge from the END that has crossed its two and read
	// the body's last line above them, and aboveLast1 one line past that: on
	// any width.
	aboveLast0
	aboveLast1
	// afterLast0 is a bridge from a line that may be the body's last, short
	// or full, and afterLast1 and afterLast2 one and two lines past it: on
	// any width.
	afterLast0
	afterLast1
	afterLast2
	// across64x0 is a bridge from a full line at 64 characters, and
	// across64x1 and across64x2 one and two lines past it: on a line at 64.
	across64x0
	across64x1
	across64x2
	// across70x0, across70x1 and across70x2 are the same at 70, and the
	// across76 ones at 76.
	across70x0
	across70x1
	across70x2
	across76x0
	across76x1
	across76x2

	// readings are the places a reading proper can be in, tail or body.
	readings = tailNone | tailS | tailO | tailSS | tailSO | tailOS | tail3 | body0 | body1
	// bridges are the places a bridge can be in.
	bridges = fromEnd0 | fromEnd1 | fromEnd2 | aboveLast0 | aboveLast1 | afterLast0 | afterLast1 | afterLast2 |
		across64x0 | across64x1 | across64x2 | across70x0 | across70x1 | across70x2 |
		across76x0 | across76x1 | across76x2

	// everyReading is every place a read back can be in.
	everyReading = readings | bridges
)

// readAs is what one line can be read as, read back: a set, since a line that
// reads like a key's may be a full line or a line interrupting the key, and
// since a live view does not know the wrapping of the END that will read it
// ([backHold]).
type readAs uint8

const (
	// asShort is the body's short last line, or a line interrupting the key.
	asShort readAs = 1 << iota
	// asOther is a line interrupting the key.
	asOther
	// asFull is a full line of the body.
	asFull
	// asAt64 is a full line at 64 characters, asAt70 one at 70, asAt76 one
	// at 76 ([atWidth]).
	asAt64
	asAt70
	asAt76
	// asChecksum is OpenPGP's checksum line ([checksumRun]), which a
	// reading starts again above.
	asChecksum
	// asAnyWidth is a full line whose width a CUT hid: a line a cut wrote
	// into ([wrapping.cutInto]) whose head is at no encoder's width, or a
	// short line that is a piece of a key ([claims.piece]). A bridge lands
	// on it as on a line at every encoder's width, and one starts from it
	// to a line at any of them ([backReading.past]).
	asAnyWidth

	// asAtWidth is a full line at any encoder's width.
	asAtWidth = asAt64 | asAt70 | asAt76
)

// widths are, for each encoder's width, a full line at it and the bridge that
// starts from one: the bridge's places one and two lines past it are the next
// two bits.
var widths = [...]struct {
	at     readAs
	across backReading
}{{asAt64, across64x0}, {asAt70, across70x0}, {asAt76, across76x0}}

// past is the readings r go on to past one more line, read as any of as, and
// whether the block's span reaches back to it: one of them takes it as a full
// line.
func (r backReading) past(as readAs) (backReading, bool) {
	var next backReading
	if as&asChecksum != 0 && r&(tailNone|tailO|tailS|fromEnd2) != 0 {
		// OpenPGP's checksum, with nothing of the key read since the END
		// but a line interrupting it, or the two a bridge crossed: the
		// reading starts again above it, as from the END.
		next |= tailNone | fromEnd0
	}
	if as&asShort != 0 {
		next |= r.interrupted(true)
	}
	if as&asOther != 0 {
		next |= r.interrupted(false)
	}
	bridged, lands := r.across(as)
	next |= bridged
	full := lands || as&asFull != 0 && r&readings != 0
	if as&(asShort|asFull) != 0 && r&(tailNone|tailO|tailS) != 0 {
		// The line may be the body's last: no short line before it, or
		// one read as interrupting the key.
		next |= afterLast0
	}
	if !full {
		return next, false
	}
	next |= body0
	for _, w := range widths {
		if as&w.at != 0 {
			next |= w.across
		}
	}
	if as&asAnyWidth != 0 {
		next |= afterLast0
	}
	return next, true
}

// across is where the bridges in r go past one more line, read as any of as,
// and whether one of them lands on it ([backReading]).
func (r backReading) across(as readAs) (backReading, bool) {
	var next backReading
	// Whatever the line is, it is one of the two a bridge goes across, or
	// the one line interrupting the key above the body's last.
	for _, step := range [...][2]backReading{
		{fromEnd0, fromEnd1}, {fromEnd1, fromEnd2}, {aboveLast0, aboveLast1},
		{afterLast0, afterLast1}, {afterLast1, afterLast2},
	} {
		if r&step[0] != 0 {
			next |= step[1]
		}
	}
	lands := r&(fromEnd2|aboveLast0|aboveLast1|afterLast2) != 0 && as&asAtWidth != 0
	for _, w := range widths {
		next |= (r & (w.across | w.across<<1)) << 1
		lands = lands || r&(w.across<<2) != 0 && as&(w.at|asAnyWidth) != 0
	}
	if r&fromEnd2 != 0 && as&(asShort|asFull) != 0 {
		// The body's last line above the two, the line at a width above
		// it still to come.
		next |= aboveLast0
	}
	return next, lands
}

// interrupted is the readings r go on to past one more line that is not a full
// one: the short last line or a line interrupting the key when short is true, a
// line interrupting it when it is not.
func (r backReading) interrupted(short bool) backReading {
	var next backReading
	switch {
	case short:
		// Only as the short last line. Read as a line interrupting the key
		// instead, it would take tailNone to tailO and tailS to tailSO, and
		// tailS and tailSS go on past every line those two do: the set
		// loses no reading without them.
		if r&tailNone != 0 {
			next |= tailS
		}
		if r&tailS != 0 {
			next |= tailSS
		}
		if r&tailO != 0 {
			next |= tailOS
		}
	default:
		if r&tailNone != 0 {
			next |= tailO
		}
		if r&tailS != 0 {
			next |= tailSO
		}
		// tailO takes no second interruption in a row.
	}
	if r&(tailSS|tailOS) != 0 {
		// Either way, the middle of the three is the short last line.
		next |= tail3
	}
	// tailSO and tail3 take no further line: a third after an interruption
	// would make the middle one no short last line, a fourth is one too
	// many. body1 takes no second interruption.
	if r&body0 != 0 {
		next |= body1
	}
	return next
}

// keyLike reports whether a run of base64 reads like the encoding of random
// bytes, as a key's body does: upper and lower case letters together. A
// sixty-four-character line of uniformly random base64 lacks either case with
// odds below one in a trillion. A digit is not asked for: such a line lacks one
// about once in fifty thousand ([readBack]).
func keyLike(run string) bool {
	var upper, lower bool
	for i := 0; i < len(run); i++ {
		switch c := run[i]; {
		case 'A' <= c && c <= 'Z':
			upper = true
		case 'a' <= c && c <= 'z':
			lower = true
		}
		if upper && lower {
			return true
		}
	}
	return false
}

// minFragment is the shortest run of base64 read as a piece of a key on a line
// it shares with text that is no key's ([fragment]): eight characters, six
// bytes of a key. A shorter run says too little to tell from a word.
const minFragment = 8

// fragment reports whether run, a run of base64 on a line it shares with other
// text, reads like a PIECE OF A KEY — what a cut leaves of a key's line beside
// its mark: at least [minFragment] characters, upper and lower case letters
// together ([keyLike]), and a digit, a diff's `+` before it aside.
//
// THE PIECE IS WHAT IS READ, NEVER THE MARK. A tool's mark is prose — no two
// tools spell one alike, and a list of them is a list of the cuts somebody
// remembered — and prose is also what a line holds that is no key's at all, so
// the line's structure cannot say which it is. Read by structure alone — every
// run where the line's prefix ends or its suffix begins taken as the key's,
// whatever the rest of the line is — every line of prose beside a key was read
// as one of its lines: over keys of every kind in every wrapping with prose
// around them, 19,200 texts of 19,800 lost prose that this package had taken
// from 932, and above a bare END, or between two armours with nothing but
// prose between them, every one did.
//
// A DIGIT, because what else reads like base64 on such a line has none: a word,
// a CamelCase identifier, a Go test's name, a path in mixed case. Both cases
// alone, as a full line read back asks ([keyLike]), took a capitalised word:
// 8,568 of the 19,800 texts lost prose, against 977 with the digit. A capital
// past the first letter instead of a digit took every Go test's name: 3,045
// lost prose, and since a line holding a piece is held as a full line
// ([backHold]), the live view held a verbose test run's output for as long as
// its names went on — fed `go test -v` output of this repository's packages, it
// held more in 162 packages of 165, the median package's mean hold 1,388 bytes
// against 220 and the largest the whole 64 KiB a hold may reach
// ([MaxKeyBlockBytes]) against 4,026. `+` and `/` are base64's and no word's,
// but code writes `name+"` and a path writes `/`: 1,971. What the digit costs
// is a piece that lacks one — of eight characters of random bytes' base64 about
// one in four, of sixteen one in fifteen, of forty one in a thousand — which
// leaves 1,327 texts of 83,419 cut keys publishing what both cases alone would
// have redacted. Not even straight after a BEGIN, where a key's first line
// stands: asked for no digit there, a BEGIN with prose under it — a grep's
// match, a placeholder — read a capitalised word as a key's first line and the
// prose after it as the rest, and 1,920 texts lost prose against 977, for 369
// texts of cut keys most of whose published characters the encoding fixes, an
// RSA key's first sixteen (`MIIEowIBAAKCAQEA`) among them.
func fragment(run string) bool {
	run = strings.TrimPrefix(run, "+")
	return len(run) >= minFragment && keyLike(run) && strings.ContainsAny(run, "0123456789")
}

// claims is what the rules REPLACE in a text a key is read in, and what NAMES
// what they replace: every token a rule matches, and a password's key and the
// separator after it with the value after them — as ranges of the text in
// order, those that overlap made one — read the first time a run asks, since a
// text holding a key's armour seldom holds either. A line of a key's body
// holds nothing that names ([wrapping.bodyLine]), and a piece of a key nothing
// they replace ([claims.piece]).
//
// A RUN ANOTHER RULE CLAIMS IS NO PIECE OF A KEY. Where a cut leaves a key's
// line a piece reads like one ([fragment]), so does a run of a token or a
// password around it — `s3Password`, `Password=hunter2`, the `Ab12…` after a
// token's `_` — and taken as a key's, a span ended or began inside it: the
// key's marker took the password's key or the token's head, and what was left
// was no match of its rule. `s3Password: hunter2-Secret!` under a key whose
// END was never written became `[REDACTED:private-key]: hunter2-Secret!`, and
// `OPENAI_API_KEY=sk-abcdefghij_Ab12…` above an END kept the token's first ten
// characters: over passwords and tokens beside keys in every place one is
// read, 4,175 texts of 39,744 published what the rules redact, and as many in
// a string with its breaks escaped. Any of the run claimed, not only part of
// it: a password whose value a pass has already replaced is its key and
// separator alone, `s3Password=` before the marker, all of it inside the run,
// and read as a piece the second pass redacted the key the first had left
// beside its value. What it costs is a key's line that holds a token's shape
// whole — `AKIA` and sixteen capitals and digits, about once in four thousand
// million lines of a key — read as a line interrupting it wherever it is read.
type claims struct {
	text string
	// whole is what each rule replaces: a token, a password's key and
	// separator with its value. named is what names it, which is all a
	// token is and a password's key and separator alone ([claims.names]).
	whole, named [][2]int
	// tokens are the tokens' ranges, and passwords where each password's key
	// begins, its separator ends and its value ends ([claimed]).
	tokens    [][2]int
	passwords [][3]int
	// taken are the blocks read forward from a BEGIN ([claims.take]).
	taken [][2]int
	read  bool
}

// any reports whether the rules replace any of text[start:end].
func (c *claims) any(start, end int) bool {
	c.load()
	return overlaps(c.whole, start, end)
}

// names reports whether a token or a password's key and separator takes any
// of text[start:end]: what a line of a key's body may not hold
// ([wrapping.bodyLine]), where a password's value may still be the body's
// short last line, redacted with it.
func (c *claims) names(start, end int) bool {
	c.load()
	return overlaps(c.named, start, end)
}

// load reads the claims the first time a run asks.
func (c *claims) load() {
	if !c.read {
		c.tokens, c.passwords = claimed(c.text)
		c.read = true
		c.build()
	}
}

// take says which blocks the keys read forward from their BEGINs took, before
// any is read back from an END: A KEY A BLOCK TAKES NAMES NO VALUE PAST IT.
// The rules run over the text the keys leave, so a password whose key and
// separator a block takes is no password to them, and its value on a line the
// block did not reach is no value, but text: claimed as one, it kept an END
// from reading that line back as the key's when it read like one — a value is
// read past the wrapping's blank lines ([readValue]), which a block read under
// another wrapping may take as its own lines and stop at — so the line was
// published, and a second pass, the key now inside the block's marker, read it
// back and redacted it.
func (c *claims) take(blocks [][2]int) {
	c.taken = blocks
	if c.read {
		c.build()
	}
}

// build makes whole and named from the tokens and the passwords, the value of a
// password whose key a block took left out where it runs past the block.
func (c *claims) build() {
	whole := slices.Clone(c.tokens)
	named := slices.Clone(c.tokens)
	for _, p := range c.passwords {
		named = append(named, [2]int{p[0], p[1]})
		end := p[2]
		i, _ := slices.BinarySearchFunc(c.taken, p[0], func(b [2]int, at int) int {
			if b[1] <= at {
				return -1
			}
			return 1
		})
		if i < len(c.taken) && c.taken[i][0] <= p[0] && end > c.taken[i][1] {
			end = p[1]
		}
		whole = append(whole, [2]int{p[0], end})
	}
	c.whole, c.named = mergeSpans(whole), mergeSpans(named)
}

// overlaps reports whether any of ranges, in order and apart, takes any of
// [start, end).
func overlaps(ranges [][2]int, start, end int) bool {
	i, _ := slices.BinarySearchFunc(ranges, start, func(r [2]int, at int) int {
		if r[1] <= at {
			return -1
		}
		return 1
	})
	return i < len(ranges) && ranges[i][0] < end
}

// piece reports whether text[start:end], a run of base64, is a piece of a key
// ([fragment]) that no rule replaces any of.
func (c *claims) piece(start, end int) bool {
	return fragment(c.text[start:end]) && !c.any(start, end)
}

// full reports whether text[start:end] is a run at a full line's width that
// reads like a key's ([keyLike]) and that no rule replaces any of.
func (c *claims) full(start, end int) bool {
	return end-start >= minKeyLine && keyLike(c.text[start:end]) && !c.any(start, end)
}

// keyText reports whether text[start:end] reads like a key's text — at least
// as long as a piece of one ([minFragment]), in both cases ([keyLike]), digit
// or none — and no rule replaces any of it: the run at the other end of a line
// a piece has said a cut wrote into ([wrapping.cutStart]).
func (c *claims) keyText(start, end int) bool {
	return end-start >= minFragment && keyLike(c.text[start:end]) && !c.any(start, end)
}

// keyRun reports whether text[start:end] is one of a key's runs on a line that
// holds its body ([keyRun]) that no rule replaces any of.
func (c *claims) keyRun(start, end int) bool {
	return keyRun(c.text[start:end]) && !c.any(start, end)
}

// promptAt is where a password's key begins on line, a trimmed line, when the
// line is a password's PROMPT — it ends in that key and its separator:
// `Password:`, `Enter password:`, `hunter2 Retype password:`, `    12→pwd=`
// — or -1. An END reads back across a prompt as across a blank
// line ([wrapping.readBack]), and a live view counts one no more than it does
// a blank line ([backHold]).
//
// A prompt is a line a key's block can take whole. What it names is on a line
// after it, and a block an END closes ends at that END, so whatever the
// prompt's value is, the block takes it with the prompt — it is a line
// holding a password's key with its value BESIDE it that a block must not
// split ([wrapping.bodyLine]). Read as a line interrupting the key, each
// prompt spent one of the two a read back crosses, and a key's tail broken by
// three of them, another process asking for a password three times, was
// published: of 896,000 texts of real keys with one to three lines of a
// password exchange written into them, 42,075 published at least 24
// characters of the body. An earlier reading redacted them only by taking
// each second prompt for the value of the one before it, which is what
// published `hunter2` in `password:` over `  password: hunter2`.
//
// ASKED OF THE KEY ALONE, and not of what comes before it, because a pass
// replaces what comes before it and never a key a prompt ends in (below): a line holding a value a key
// above it named (`hunter2x9  # rotate the password:` under `Password:`) is a
// marker and a prompt on the second pass, and asked whether that value could
// be a key's line, the first pass had read it as none and the second read
// back further. Another password's key and its value before the prompt's are
// no exception: read as interrupting the key, three lines of `pwd=x
// password:` published the key above them. And a prompt read back is inside
// the block whenever a line above it is, so whatever it holds before its key
// is redacted with the key — and a line no line above it reads, the read
// never starts from.
//
// A KEY THAT IS ANOTHER'S VALUE IS NO PROMPT. In `pwd= password:` and
// `passwd:Pwd:` the rule takes the second key as the first one's value, as
// it takes them one after another ([rule.next]), so the line holds a
// password and its value and a pass replaces the second key. Read as a
// prompt, a line a read never starts from, `ffAt0gun passwd:Pwd:` above an
// END was published, and a second pass, the prompt gone, read the line as a
// key's and redacted it.
//
// NOR IS A KEY A TOKEN TAKES. The rules before the password's replace a
// token run into the key with it (`sk-…Pwd:`), so the line ends in the
// token's marker and the colon, and a pass leaves no prompt there. Asked of
// the line as written, `ffAt0gun sk-…Pwd:` above an END was a prompt and the
// piece before it was published, and a second pass, the token's marker in its
// place, read the line as a key's and redacted it. So the line is asked as
// the rules before the password's leave it ([passwordInput]), as [claimed]
// reads the passwords.
func promptAt(line string) int {
	at := passwordPrompt.indexAtEnd(line)
	if at < 0 {
		return -1
	}
	w := passwordInput(line, nil)
	if len(w.edits) > 0 {
		at = passwordPrompt.indexAtEnd(w.text)
	}
	var prior [][2]int
	for from := 0; at > 0; {
		start, end, ok := passwordRule.next(w.text, from)
		if !ok || start >= at {
			break
		}
		valueStart, valueEnd := passwordValue(w.text, start, end, prior)
		if valueEnd <= valueStart {
			from = beyond(w.text, start)
			continue
		}
		if valueStart <= at && at < valueEnd {
			// The prompt's key is a value the rule replaces.
			return -1
		}
		prior, from = append(prior, [2]int{valueStart, valueEnd}), valueEnd
	}
	if at < 0 {
		return -1
	}
	return w.at(at)
}

// claimed is every token the rules replace, in order, those that overlap made
// one, and every password: where its key begins, where its separator ends and
// where the value it names ends, the separator's end where it names none
// ([claims]).
//
// IN THE RULES' ORDER, and as they take them, which is what they replace and
// not what each would on its own: the passwords are read in the text as the
// rules before them leave it, every token replaced by its marker
// ([passwordInput]). So a password's key a token took is no key, and a value
// ends where a token in it begins, at the token's marker ([valueEnd]): read on
// its own, a value written in a string ran on across an escaped break into the
// next line's piece of a key (`pwd=\nsk-…\n…u1SU1Lf9x= -----END`), which was
// then no piece, and the rule replaced the value only up to the token, so the
// piece was published and a second pass, the token's marker ending the value,
// redacted it. And a value is read where a line's wrapping puts it, by lines
// that read differently with a token's marker on them than with the token
// ([gutterLen]): read in the text as written, a claim named a value the rule
// did not replace. A password's matches are taken one after another as
// [rule.next] takes them, so a key inside a value it replaced is none: claimed
// with its own value, a second pass, the value gone, read a key's piece there
// that the first had not. A password's key and separator are claimed whether
// or not a value follows yet, so what a run is does not change when the line
// after it is written.
func claimed(text string) (tokens [][2]int, passwords [][3]int) {
	w := passwordInput(text, nil)
	for _, e := range w.edits {
		tokens = append(tokens, [2]int{e[2], e[3]})
	}
	tokens = mergeSpans(tokens)
	var prior [][2]int
	for from := 0; ; {
		start, end, ok := passwordRule.next(w.text, from)
		if !ok {
			break
		}
		if valueStart, valueEnd := passwordValue(w.text, start, end, prior); valueEnd > valueStart {
			passwords = append(passwords, [3]int{w.at(start), w.at(end), w.at(valueEnd)})
			prior, from = append(prior, [2]int{valueStart, valueEnd}), valueEnd
			continue
		}
		passwords, from = append(passwords, [3]int{w.at(start), w.at(end), w.at(end)}), beyond(w.text, start)
	}
	return tokens, passwords
}

// lastPiece is where the last run of base64 on line, which starts at at in the
// text, that is a piece of a key ([claims.piece]) ends, or -1.
func (c *claims) lastPiece(at int, line string) int {
	last := -1
	for i := 0; i < len(line); {
		start, end := nextRun(line, i)
		if start == end {
			break
		}
		if c.piece(at+start, at+end) {
			last = at + end
		}
		i = end
	}
	return last
}

// firstPiece is where the first run of base64 on line, which starts at at in
// the text, that is a piece of a key ([claims.piece]) begins and ends, or -1
// and -1.
func (c *claims) firstPiece(at int, line string) (int, int) {
	for i := 0; i < len(line); {
		start, end := nextRun(line, i)
		if start == end {
			break
		}
		if c.piece(at+start, at+end) {
			return at + start, at + end
		}
		i = end
	}
	return -1, -1
}

// backHold is where the earliest line of text — a text ending on a line break —
// begins that an END not written yet could still read back over ([readBack]),
// or -1.
//
// Every line such an END could take, under ANY wrapping, since the END's prefix
// and suffix are not written yet either, nor the lines that may come before it.
// So this runs the readings the read back runs ([backReading]), from every one,
// over everything each line could be under some wrapping: a line interrupting
// the key or the body's short last line, always; a full line when it holds a
// run of base64 at a body's width that reads like a key's ([keyLike]); and a
// full line at the width a cut hid ([asAnyWidth]) when it holds a piece of a
// key ([fragment]) wherever on it, since which runs are at its two ends
// depends on the wrapping and a line a cut wrote into is read as one
// ([wrapping.readLine]) — or no more than a full line at its run's width, when
// its run is the only one on it ([keyRuns]). It holds from the earliest line a
// reading takes as a full one. The same readings over a superset of what every
// line can be are never a shorter reach than the real read's, whatever is
// written next.
//
// A line it holds is released once five lines in a row holding no such run
// come after it; a blank or glue line counts for none, and a line that may be
// OpenPGP's checksum with no more than two of the five after it starts the
// count again above it, since a reading starts again there as from the END.
// Every reading has taken two of them, and from there none takes more than one
// further line that is not a full one — a body carries one interruption, and a
// tail its third line; a bridge from the END or the body's last line has
// crossed its two by the fourth, and lands on nothing but a full line at an
// encoder's width, the END's with at most the body's last line and one line
// interrupting the key above its two — so the five end every reading and every
// bridge short of the held line. Fewer do where neither the held line nor one
// after it, above the two, is at an encoder's width: there no bridge has a
// line to land on, and two lines in a row holding none with two more lines of
// any kind after them release it, as they did before a read crossed two
// lines. Nothing less does: a line holding such a run may be read as one
// interrupting a key, so it keeps a reading going through it rather than
// ending one, which is why four lines in a row holding none release a held
// line where three do not, and five one at an encoder's width, which a bridge
// lands on across the third and fourth. A line held for a piece of a key alone
// is released by the same four: no reading proper is left four lines above an
// END with nothing at a key's width between, a bridge from the END or from a
// line that may be the body's last lands on nothing but a line at an encoder's
// width, and one across two lines lands on a piece only from a line at a
// width two lines below it, which those four lines do not hold.
func backHold(text string) int {
	r, hold := everyReading, -1
	for start := len(text); start > 0 && r != 0; {
		var end int
		start, end = lineBefore(text, start)
		if len(text)-start > MaxKeyBlockBytes {
			break
		}
		line := text[start:end]
		if armoured(line) {
			break
		}
		trimmed := trimLine(line)
		if trimmed == "" || glue(trimmed) || promptAt(trimmed) >= 0 {
			continue
		}
		var full bool
		if r, full = r.past(asShort | asOther | keyRuns(trimmed)); full {
			hold = start
		}
	}
	return hold
}

// keyRuns is what a line can be read as beside the body's short last line and
// a line interrupting the key, under some wrapping: a full line, at each
// encoder's width it holds one at ([atWidth]), when it holds a run of base64
// at a body's width ([minKeyLine]) that reads like a key's ([keyLike]);
// OpenPGP's checksum line when it holds a run that is one ([checksumRun]);
// and when it holds a piece of a key ([fragment]), a full line at the width a
// cut hid ([asAnyWidth]), since under some wrapping the piece is a run at one
// of its ends and the line one a cut wrote into ([wrapping.cutInto]), or its
// own run, a short line cut so ([readRun]).
func keyRuns(line string) readAs {
	var as readAs
	runs, piece, full := 0, false, false
	for i := 0; i < len(line); {
		if !inBase64(line[i]) {
			i++
			continue
		}
		j := i
		for j < len(line) && inBase64(line[j]) {
			j++
		}
		switch run := line[i:j]; {
		case len(run) >= minKeyLine && keyLike(run):
			as |= asFull | atWidth(run)
			full = true
		case checksumRun(run):
			as |= asChecksum
		}
		piece = piece || fragment(line[i:j])
		runs++
		i = j
	}
	if piece && (runs > 1 || !full) {
		// A line no wrapping can read as anything but a full line at
		// its run's own width — one run, at a full line's width — is
		// never one a cut wrote into.
		as |= asFull | asAnyWidth
	}
	return as
}

// prefixShape is a prefix with what a wrapping moves on from line to line made
// alike: every run of digits one 0, every `-` a `:`, every run of spaces one
// space ([shapeAt]).
func prefixShape(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); {
		var c byte
		c, i = shapeAt(p, i)
		b.WriteByte(c)
	}
	return b.String()
}

// shapeAt is what [prefixShape] writes for the run of p that begins at i, and
// where the next run begins: one 0 for a run of digits, one space for a run of
// spaces, a `:` for a `-`, and any other byte as it is. A password's value is
// read past a wrapping's gutter by the same shapes, run for run ([gutterLen]).
func shapeAt(p string, i int) (byte, int) {
	switch c := p[i]; {
	case '0' <= c && c <= '9':
		for i++; i < len(p) && '0' <= p[i] && p[i] <= '9'; i++ {
		}
		return '0', i
	case c == ' ' || c == '\t':
		for i++; i < len(p) && (p[i] == ' ' || p[i] == '\t'); i++ {
		}
		return ' ', i
	case c == '-':
		return ':', i + 1
	default:
		return c, i + 1
	}
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
// full-width one is a word, and no key's — unless it is a piece of a key
// ([fragment]), the body's first run a cut left short. And a body is read on
// across one cut in it ([pastCut]).
func runChunk(s string, at int, c *claims) (bool, int) {
	body, end := uncutRuns(s, at, c)
	if body {
		end = pastCut(s, end, at, c)
	}
	return body, end
}

// keyRun reports whether a run of base64 on a line that holds a key's body
// is one of the key's: a run at the encoder's width that reads like a key's
// ([keyLike]), or a piece of one ([fragment]).
func keyRun(run string) bool {
	return len(run) >= minKeyLine && keyLike(run) || fragment(run)
}

// pastCut is where a key's body written on one line, read in s up to end,
// ends once it is read across ONE CUT: past one stretch of text holding no
// run of the key ([keyRun]) — a cut's mark — to the key's runs after it, and
// the body's short last run and OpenPGP's checksum after a run at the
// encoder's width. It is end itself where no run of the key follows the
// stretch, or where the runs that do hold none at the encoder's width.
//
// A KEY ON ONE LINE IS READ ACROSS A CUT as a key on its lines is read across a
// line that is not the key's between two that are — once — because a tool
// that cuts long text cuts the key on one line first, an environment dump or
// a JSON value with its breaks turned to spaces, and keeps its head and its
// tail with its mark between them. Read only up to the mark, the part of the
// key across it from the one armour the line held was published: of 83,419
// texts of keys of every kind in every wrapping cut at every place, 3,170
// published it, read across no cut either way, that are now redacted. ONLY TO
// A RUN AT THE ENCODER'S WIDTH, because a piece alone is what a word with a
// digit in it reads like as well: read across to any piece, forward and back,
// prose written on the line of a key beside it lost a word in 4,282 texts of
// 8,400 against 2,197 read up to the mark; read across only to a run at the
// width, in 2,373 — a Go test's name as wide as a key's line, in both cases,
// reads like one — at the price of 462 texts of the cut keys that a read to
// any piece would have redacted. And ONCE, so a key two cuts left on one line
// is read up to the second and no further.
func pastCut(s string, end, at int, c *claims) int {
	i := end
	for {
		k, l := nextRun(s, i)
		switch {
		case k == l:
			return end
		case !c.keyRun(at+k, at+l):
			i = l
			continue
		}
		break
	}
	last, full, atWidth := end, false, false
	for {
		k, l := nextRun(s, i)
		switch {
		case k == l:
		case c.keyRun(at+k, at+l):
			last, full, i = l, l-k >= minKeyLine, l
			atWidth = atWidth || full
			continue
		case full:
			// The body's short last run after one at the width, and
			// OpenPGP's checksum after that.
			last = l
			if m, n := nextRun(s, l); armourChecksum(s[m:n]) {
				last = n
			}
		}
		break
	}
	if !atWidth {
		return end
	}
	return last
}

// beforeCut is where a key's body written on one line, read back in s from
// start ([lineBack]), begins once it is read back across one cut, as
// [pastCut] reads one forward — or -1 where no run of the key at a full
// line's width lies across one, or none as wide as an encoder writes one when
// wide is true: when nothing between the cut and the END reads as the key's.
//
// WIDE, THEN, because a cut written straight before the END leaves nothing of
// the key on the END's side to say what is across it, and a run at a full
// line's width that reads like a key's is what a path in mixed case is too: a
// grep's match for the END line, `…/certs/ServerPrivateKey.pem:28:-----END
// PRIVATE KEY-----`, was a key's head across the cut `.pem:28:` and redacted
// whole. A run exactly as wide as an encoder's line, or wider than any — the
// body of a key whose line breaks were removed — is one a path all but never
// is: of 2,382 lines of `rg -n 'PRIVATE KEY'` over a module cache, the 9 a run
// at a full line's width took are left, and 111 texts of 8,400 with prose on a
// key's line lose none of it, for 28 texts of 83,419 cut keys whose head
// across the cut holds no such run.
//
// It reads nothing before the last five dashes in s, which open every armour,
// since another block's armour ends a key's block; nor at or before the last
// marker, where a rule replaced what was there, since the armour that stopped
// the first pass may be what the marker replaced: a key written on one line
// between a run as wide as an encoder's line and an END is redacted up to its
// BEGIN's armour, and to the second pass its marker is words, which a read back
// from the END went on across to that run — redacting twice was not redacting
// once ([Secrets]). Read forward there is no marker to stop at: a BEGIN a
// second pass still reads a body after is one the first pass read the same body
// after, and redacted.
func beforeCut(s string, start, at int, c *claims, wide bool) int {
	floor := max(strings.LastIndex(s[:start], Marker), 0)
	if i := strings.LastIndex(s[:start], "-----"); i >= 0 {
		floor = max(floor, i+len("-----"))
	}
	i := start
	// prev is the run of base64 that ends nearest before i and after floor.
	prev := func() (int, int, bool) {
		start, end := runBefore(s, i, floor)
		i = start
		return start, end, start < end
	}
	b, e, ok := prev()
	for ok && !c.keyRun(at+b, at+e) {
		b, e, ok = prev()
	}
	first, across := -1, false
	for ok && c.keyRun(at+b, at+e) {
		first = b
		across = across || e-b >= minKeyLine && (!wide || encoderWide(s[b:e]))
		b, e, ok = prev()
	}
	if !across {
		return -1
	}
	return first
}

// runBefore is where the run of base64 in s that ends nearest before i, and no
// nearer s's start than floor, begins and ends: [nextRun] read backwards, and
// start == end where there is none.
func runBefore(s string, i, floor int) (int, int) {
	end := i
	for end > floor && !inBase64(s[end-1]) {
		end--
	}
	start := end
	for start > floor && inBase64(s[start-1]) {
		start--
	}
	return start, end
}

// encoderWide reports whether a run is as wide as an encoder writes a key's
// line ([atWidth]), or wider than any line one writes: the body of a key whose
// line breaks were removed.
func encoderWide(run string) bool { return atWidth(run) != 0 || len(run) > maxKeyLine }

// uncutRuns is [runChunk]'s body as far as the first text in it that is no
// key's.
func uncutRuns(s string, at int, c *claims) (bool, int) {
	long, end := false, 0
	for i := 0; i < len(s); {
		start, j := nextRun(s, i)
		if start == j {
			break
		}
		if j-start < minKeyLine {
			if !long && c.piece(at+start, at+j) {
				// A body of its own where no run at the encoder's
				// width follows — the cut's own word after it is no
				// body's last run — and the head of one where one does.
				if k, l := nextRun(s, j); l-k < minKeyLine {
					return true, j
				}
				long, end, i = true, j, j
				continue
			}
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
// that a later line would have redacted. Five things hold a line back, and
// each is a match that can cross a line break:
//
//   - a private key's block still OPEN — its BEGIN line, and every line
//     after it that a key's block could hold, while the next line written
//     could still be its body or its END ([keyBlocks]). Prose straight after
//     a BEGIN ends its block, so a mere mention of the armour holds back no
//     more than itself; after a body, two lines that are not a key's do;
//   - lines an END NOT WRITTEN YET could read back over ([backHold]): a run
//     of base64 that reads like a key's, and the lines after it a key's
//     block could hold before its END — two in a row that are not a key's
//     release it once two more lines follow them, four in a row being the
//     commonest case, and five where a run is exactly as wide as a key's
//     line, which an END can read back across two lines to ([backReading]).
//     A hash in hex or a path in lower case is shown at once; a Go test's
//     name, a CamelCase identifier or a path in mixed case reads like a key's
//     line and waits as one ([readBack]), and so, for four lines, does a line
//     holding a piece of one — eight or more characters in both cases with a
//     digit — which an END can read back across as a line a cut wrote into,
//     a full line at the width the cut hid ([asAnyWidth]);
//   - a key whose block, as written so far, runs across the boundary;
//   - a password key whose value a line at or after the boundary decides,
//     or a line not written yet: the rule reads a value past its wrapping's
//     blank lines and punctuation, lines below its key ([readValue]), and a
//     text cut between the two reads another, or none ([valueHolds]). Not
//     where the cut reads the same — the value the key's line alone gives,
//     which the next line holding a key of its own leaves it — since held
//     there, each line of a run of `password:"` held the one above it and a
//     live view showed nothing of the run while it went on; unless the
//     line that decided it is held for a key, by the three above, since
//     what is written next may read it as a key's, or as no longer one;
//   - a password key whose value, as the keys are read beside it — in the
//     text with no key's block replaced ([claims]) — a line at or after the
//     boundary decides, as the fourth holds the value the rule reads: the
//     text after a cut there is read with no key naming that value, and an
//     END reads the value back as a line of its key. The two readings differ
//     where a password's key follows an END on its line
//     (`-----END RSA PRIVATE KEY-----|passwd:`): there the rule reads a
//     marker, which can join the value's line to the END's or take the
//     gutter the END's line opened with, so the claim keeps out of the block
//     a value the rule does not take. Held for the rule's reading alone, the
//     live view redacted a piece of a key's line that the whole text
//     publishes.
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
	spans, open, c := readKeys(whole)
	back := backHold(whole)
	keys := func(settled int) (int, bool) {
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
		return settled, moved
	}
	settled := end
	for moved := true; moved; {
		settled, moved = keys(settled)
	}
	// rewritable is where the lines the keys hold begin: what is written
	// next may still read any of them as a key's, or as no longer one — a
	// one-line key an END below reads back across into a block, a block an
	// END closes — and a password's value decided on such a line may be
	// decided again once it reads otherwise ([passwordInput]).
	rewritable := settled
	input := passwordInput(whole, spans)
	holds := valueHolds(input.text)
	for k, h := range holds {
		holds[k].start, holds[k].decided = input.at(h.start), input.at(h.decided)
	}
	// The keys were read beside the passwords' values as the text reads
	// with no block replaced ([claims]), and a value read so is decided
	// by its own lines too: a key whose claim a cut would decide otherwise
	// is held with it, since the text after the cut is read with no claim
	// on that value, and an END there reads it back as the key's.
	if c != nil && c.read {
		raw := passwordInput(whole, nil)
		for _, h := range valueHolds(raw.text) {
			h.start, h.decided = raw.at(h.start), raw.at(h.decided)
			holds = append(holds, h)
		}
	}
	for {
		var moved bool
		settled, moved = keys(settled)
		for _, h := range holds {
			if h.start < settled && h.decided >= settled && (!h.unread || h.decided >= rewritable) {
				settled, moved = lineStart(whole, h.start), true
			}
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
