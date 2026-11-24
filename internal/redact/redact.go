// Package redact is THE secret-redaction pass for free text the engine ships
// to a person or a model, which can carry a credential it never meant to: a
// cloned repo's URL with its token, a harness echoing its key, a command whose
// ${VAR} was resolved before it ran.
//
// There is deliberately only one of these: a second copy is how a surface ends
// up redacting a token shape its neighbour already knows about. It imports
// nothing of the engine but internal/plainjson, a leaf over the standard
// library, which is what lets internal/logging pass every record through it and
// internal/secrets and internal/config depend on it.
//
// The patterns are a DENYLIST OF KNOWN CREDENTIAL SHAPES, which bounds what
// this can promise: it catches the vendor prefixes, the engine's own per-run
// tokens, a JSON Web Token and the key formats below, not an arbitrary opaque
// secret. A session's credential is a shape too where its vendor gives it one
// — an AWS session's access key id (`ASIA`), a Google access token (`ya29.`)
// — and a run obtains one by itself as readily as it reads the keys it was
// handed. An AWS session's secret access key and session token have no shape
// of their own, so each is read as a password is, by the key AWS writes it
// under ([awsSecretKeys]): printed under one, it is taken out on every
// surface; printed bare, under none, it is published. It is the last line, not
// the first — the first is not putting a credential in the text.
//
// # Text that is still being written
//
// Every rule here reads whole text; a live view shows a growing one. [Settled]
// is how much of it has a redaction nothing written after it can change, so a
// reader never sees a key's body that its END line, arriving later, redacts; a
// rule that can reach across a line break is one Settled has to learn, and its
// tests feed it every shape here a line at a time. A stream from
// [NewLiveStream] hands its stages only what Settled settles, and
// [Stream.Flush] decides each stage there as at the text's end. Unflushed, it
// answers as a [NewStream] does; flushed, it writes Secrets where Close answers
// nil, and refuses only in a layout named below.
//
// # How far a rule reads
//
// Every rule decides what it replaces by text no further than [MaxReach] from
// where it decides, and the few bytes its format spells around that, 85 at
// most, so whatever holds that much of a text around a place redacts that
// place as the whole text does. At the bound a read decides as though the
// text ended there, or began there for a read back, and what that publishes
// is text no credential's format writes:
//
//   - an armour line whose label runs past 64 bytes, which is no armour, so a
//     token or a password's value written up to it runs on through it;
//   - a run token whose subject runs past MaxReach, or whose expiry has
//     twenty digits or more, neither of which runtoken mints;
//   - a JSON Web Token whose header and claims run past MaxReach together,
//     matched, if at all, from an `eyJ` inside its header from which they
//     fit, the head before it published;
//   - an SSH certificate with more than MaxReach of whitespace before its
//     body, and an AWS secret's value further than that from its key;
//   - a password's value further than MaxReach from where its key begins, on
//     its line or below it, or on a line that ends further than that under a
//     wrapping's suffix;
//   - a password's value under a key whose label runs back past MaxReach,
//     the markup its line opened before the label then read as the value;
//   - and a password's value whose key is further than MaxReach along the
//     line after an escaped break in another value, which that value then
//     runs on over.
//
// Five reads take another answer at the bound, so a second pass reads what the
// first did. An END line that goes on past it has no suffix a line above it
// ends in, and one holding a marker only past the bound is read back as one
// holding none. A line under a wrapping's suffix that the bound cuts names no
// value where a marker stands where its value would begin, and a table's row
// holding a marker of a value closes no key's row. And a password key's line
// that begins further back than the bound, as written or as the rule leaves
// it, has no gutter, so the value is read from the start of its line, the
// wrapping's gutter taken with it. And two redact more: the punctuation after
// an escaped break in a password's value, read that far with no letter, digit
// or space in it, runs on as the value; and a quoted key's name running back
// that far is the key's own string, its value redacted.
//
// # Settled in one pass
//
// Settled is the largest line start no hold's bar stands over ([bar]), an
// unread key's counted at the rewritable point there ([heldLines.sweep]),
// which only falls as the start does. One sweep down the bars finds it: a move
// skips only barred starts, a passed unread bar wakes once the point falls to
// it, and the sweep stops where none stands. A claim beside a block lowers the
// point and needs no bar: the block's and its own cover what it would skip.
//
// # A text that arrives in pieces
//
// [Stream] redacts a text written to it in pieces in one pass and BOUNDED
// MEMORY, whatever the text, and writes, put together, exactly what [Secrets]
// returns for the whole however it is cut, or refuses it (below): a whole
// PDF's base64 on one line of a transcript is redacted in what a few hundred
// KiB a pass hold. It runs the passes Secrets makes, the keys' blocks and then
// each rule in order, as STAGES, each reading what the one before it wrote and
// deciding with the code Secrets decides with ([rule.decide], [keyReading]),
// so the two answer alike. Each stage is streamed over its own input in a
// window of MaxReach either side of a step, deciding a window once it holds a
// step past where it resumes or the text has ended, and trusting a decision
// only where the window holds all it read. The stages are handed the text a
// piece at a time at its own offsets, so what each holds is the text's alone,
// and writes are cut at any byte.
//
// What no window holds is carried from one to the next: the SEQUENCE, where
// the rule looks next and the values it replaced within reach, which the
// password's rule reads its next value beside; and an open RUN, a match or a
// value decided where it begins whose rest runs on (a token's body, a JSON Web
// Token's signature, a password's value), read on with the run's own test
// rather than held, and written out a piece at a time. A marker stands for a
// run of any length, so a stage that claims tokens or AWS secrets in the text
// as written, holding markers that stand for more of it than a step, or one
// still being read, decides up to the last marker it holds, since no token's
// or AWS secret's read crosses a marker's `[` ([ruleStage.markerToStepAt]);
// the password's reader, whose reads do, excepted.
//
// The keys' blocks read what the rules claim in the text as written
// ([claims]), so the rules run twice: ahead of the keys' stage over that text,
// keeping where each place they write stands in it, and over what the keys'
// stage writes. That stage reads an armour's block once the claims through its
// window are decided, or once the text has run the claims' lag ([holdBytes])
// past the window, whatever they say.
//
// A STREAM'S MEMORY IS ITS STAGES' WINDOWS, WHATEVER THE TEXT. A rule's stage
// holds MaxReach behind where it resumes and a step past it, about 290 KiB, in
// one buffer it reads in place; the keys' stage four blocks' reach and a step,
// and, waiting on its claims, at most the claims' lag, about 5.3 MiB. That is
// at most about 23 MiB of text, and about 33 MiB of the edits, values and
// claims beside it, where a text is markers end to end. So a stream allocates
// its buffers, about 16 MiB, and past them under a quarter of a byte a byte of
// a session or a PDF, a few where passwords are dense. Tests hold each. A live
// stream also holds its text past the settled point, any length ([settling]).
//
// A WINDOW DECIDED EARLY IS CHECKED, AND A STREAM THAT FINDS IT READ A CLAIM
// OTHERWISE REFUSES ([ErrDecidedEarly]). Every token's and AWS secret's claim,
// and every run's end but one, is known within the lag, so a window decided
// early reads a claim otherwise only where it is decided further past it: by
// the password's reader, which decides a key before a marker only with what
// follows the marker, and by an AWS secret's, which claims a quoted key where
// the blanks after it end. The one late run's end, a Google token's before
// dots that run on past the lag, is checked only over the dots
// ([keyStage.ended]). The keys' stage keeps the footprint of what each window
// decided early asked of the claims, and a claim that then comes, or ends,
// otherwise over it stops the stream: what it wrote is not Secrets of the text
// and may hold what Secrets redacts, so its caller neither keeps nor shows it.
// A text gets that only where a key's block is read beside such a claim with a
// run of about the lag after it: a password's key on a line that ends in a
// key's END, the run straight after its value's line; one on a key's BEGIN
// line or the line after it, the run as its value's line or a few lines
// below; a password's key, its value's line and a key's END on three lines
// under a gutter; a password's key or more between an armour and the run; or
// armours among long tokens each with a password's key after it. None is a
// credential's format; below its own hold a stream refuses more.
// Whether a text is refused never turns on how it is cut, and an ordinary text
// decides no window early.
//
// # A key's block beside a password
//
// The keys' blocks are read beside what the rules claim in the text as written
// ([claims]), and the rules then read the text the blocks leave, so a
// password's value is read twice, by the claims and by the rule past the keys'
// markers, and the two readings must name one value, but under a key's line
// with no gutter (below).
//
// A PASSWORD IS CLAIMED AS ITS KEY AND ITS VALUE, never the stretch between
// them: between a key and a value read below it ([readValue]) lie the
// wrapping's suffix, blank lines and the value line's gutter, which the rule
// never replaces. Claimed as one stretch, a key's piece in it would end the two
// lines unalike, and the rule would read another value than the claims and
// publish the value. A stream's reader claims each value so too
// ([claimSource.valued]); a window decided early still checks a claim as the
// one stretch from its key to its value's end ([keyStage.handed]).
//
// A KEY'S BLOCK IS WRITTEN APART FROM THE GUTTER of a password key's line it
// holds ([claims.apart]). The rule reads a value below its key past the key
// line's gutter ([gutterLen]), in the text the blocks leave; a block that runs
// across the break before the key's line, or begins on it within the gutter,
// writes the gutter into its marker, and under a wrapping that writes a word on
// every line the rule then reads that word as the value. So the read says where
// on the key's line the gutter ends ([aValue]), and the block is written as two
// markers around the line break and the gutter, unless the line it begins on
// opens, before it, with text shaped as the gutter ([claims.opensAlike]), as
// under a log's timestamp. A key's line whose value's line spells none of it
// has no gutter, and its block is written whole ([valueRead.gutterOf]): the
// rule then reads the value past the opening of the block's first line where
// the value's line opens alike, and the word the claims, reading the key's line
// as written, take for the value is the wrapping's, and shown. So a gutter the
// value's line repeats letter for letter is the wrapping's, and shown, though
// it be a run of base64, but for A KEY'S LAST RUN, glued to its END on the
// key's line or a space before it: a wrapping writes its gutter on every line,
// the key's last run stands on its END's line alone, so where the line after
// the value's does not repeat it, the run is the key's ([valueRead.withRuns]).
// The claim carries the run on each line that spells it, the keys write a
// block over each ([claims.keyRuns]), and the gutter seam ends before it, so
// both readings read the value past the same marker. A key the claims read no
// value of is written whole
// too: where the line below opens with the gutter and then an armour, the
// claims read no value, the armour ending the read, while the rule, in the text
// the blocks leave, reads the armour and the gutter before it as one marker and
// redacts the word after it as the value; cut, the key's line would open
// otherwise, and the word would be shown. The password's marker on the line the
// read stops at is the value a pass redacted there ([claims.readsAValue]), so a
// second pass cuts the block as the first did. And the markup the key's line
// opened before the key's label ([pastMarkup]), a table row's `|`, is read as
// the gutter is: a block is written apart from it, and from the line break
// before it where only spaces stand before it ([claims.openingApart]).
//
// A KEY'S BLOCK IS WRITTEN APART FROM THE ENDING of the line below a key that
// its value was read on ([claims.endingApart]). A block that begins on that
// line past the value and runs on past its end writes the line's ending into
// its marker, and the rule then takes the key line's own punctuation for the
// value. So the read says where the ending begins ([aValue]), and the block is
// written as two markers around the ending and the line break after it, which
// are the wrapping's, and shown. A second pass reads the value's line as the
// first left it, a marker where the value was, and no armour on it.
//
// A KEY'S MARKER IS READ AS THE WORDS IT REPLACED where a key's label is read
// back to the markup its line opened ([labelStart]), as the claims read a
// table row holding a key's text before a password's key, so the row's closing
// closes its opening in both readings. Where the key's text holds what no
// label does (a `+`), the claims take the closing, punctuation, for the value,
// while the rule redacts the value below: nothing a claim names is shown.
//
// A STRING'S LINES ARE READ AS THE LINES THEY STAND FOR where markup closes
// the key's piece of a string: an escaped break after the markup ends the
// key's line as a real break does ([readValue]), and the string's first piece
// is read from the quote that opens it ([stringOpening]).
//
// A BLOCK NO END CLOSES TAKES A PASSWORD'S KEY ONLY WITH ITS VALUE
// ([claims.shortOfValues]): it ends before such a key, at the key's line's last
// run of base64 or before that line, or the key in its marker would name
// nothing ([claims.take]) and the value below be shown. A block an END closes
// still takes a prompt with its value ([promptAt]), and a block that takes the
// start of a value takes its key, the value's rest then text.
//
// A STREAM CUTS A BLOCK ONCE IT IS WHOLE, as it writes it ([keyStage.write]),
// since a block read back from an END can join one read forward in an earlier
// window. The keys' stage holds the text and the claims from as far back as
// the line of the first block it has yet to write can begin
// ([keyStage.opening]), MaxKeyBlockBytes; a step that writes a block reads the
// claims over its window as a step that reads an armour does; a value that
// begins past a window reads there as a value ([keyStage.claimsIn]); and the
// window that reads where a password's key begins writes its claim's runs
// ([keyStage.runsIn]), a claim handed after it a miss. The cut
// apart from an ending asks of the claims within MaxReach before the block, so
// a claim handed or ended late there is a miss. A claim is a key, its
// separator's end, and each of its value's two ranges with three seams
// ([aValue]), 96 bytes, and 24 for each line its key's runs lie on, held
// while a window may ask of it.
//
// # What a password's rule still publishes
//
// A password's value is read on its key's line, or on a later one where the
// wrapping every line shares puts it ([readValue]). Markup the line opens with
// that the key's line does not (`|`, `-`, `>`) is read past to the value and
// replaced with it ([ownMarkup]); a JSON string escaped in layers is read
// decoded too ([decodedReading]). THE RULE LEANS TO HIDE: replaced markup
// costs formatting, markup taken for a value shows it. What neither finds is
// published:
//
//   - a value under a quoted key whose name is more than a name — letters,
//     digits, `_`, `-` and `.` — such as `{"Admin Password": "x"}`
//     ([quotedElsewhere]);
//   - a value `printf '%s\n'` writes a line at a time, every line broken at
//     the escaped break in its own prefix;
//   - a value under a blank line of a wrapping that lost the tab or the space
//     after its gutter — nl's number, a log's timestamp — which is read as the
//     value;
//   - a value shaped like the word the key's line opens with, a space after
//     it on both (`pass2 next` under `pass1 Retype password:`), read as the
//     gutter;
//   - a value written over several lines — a YAML block scalar, a YAML
//     sequence under the key, a heredoc — of which the first word at most is
//     read, and a diff whose key is on a context line and whose value
//     changed, of which the removed line's value alone is read;
//   - a value under a gutter that holds an armour, or under a blank line of
//     the wrapping whose gutter holds a password's key;
//   - under a wrapping's suffix, a value on a line that holds a password's key
//     of its own (`echo "hunter2 password:" >> f` under
//     `echo "Password:" >> f`), the key's line taking its own punctuation for
//     its value instead, or the closing of the markup that alone ends it
//     (`| Password: |` over `| > Retype password: |`): redacted, the value's
//     line would no longer end as the wrapping's does once the second key's
//     value is replaced, and a second pass would read the first key's value
//     elsewhere;
//   - the part of a value after an escaped break inside it when that part is
//     punctuation alone (`pwd=abc\n!!`), which reads as the string's;
//   - and a key's piece below a key's line that spells none of the piece's
//     line, under a block whose first line opens with a run of base64 that
//     the piece's line repeats before a key's END: written whole, the block
//     leaves the key's line opening with that run, which the rule reads as the
//     wrapping's while the claims take it for the value; a second pass reads
//     it as a key's and redacts it, and there redacting twice is not
//     redacting once.
package redact

import (
	"bytes"
	"cmp"
	"io"
	"iter"
	"regexp"
	"slices"
	"strings"
	"sync"
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
// It lives in this leaf, which owns what redaction looks like on the wire,
// because two packages need it and neither can import the other's copy:
// config's masking writes it through [internal/secrets], and config's restore
// must recognise it, or a write would silently replace a working credential
// with these twelve characters. A constant spelled twice drifts, and that
// drift is that outage. A DISTINCTIVE LITERAL rather than an
// empty string: an operator who stored an EMPTY credential said something, and
// a round trip that erased the difference would turn "no credential" into "the
// credential I could not see".
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
	value *valueFunc

	// read is, for the password's rule, the read [value] is the bounds of:
	// with where the gutter of the key's line the value was read under ends
	// ([aValue]), which the keys' blocks are written apart from
	// ([claims.apart]). Nil for every other rule.
	read *readFunc

	// bound is, for a rule whose pattern reads further than [MaxReach]
	// allows, whether a match the pattern found reads within it, and where
	// the search goes on when it does not: past every place a match that
	// does could not begin. What it holds the pattern to is the pattern with
	// the bound written into it, which RE2 cannot count to (its repeats stop
	// at a thousand), so the matches taken are that pattern's own, leftmost
	// first. Nil for a rule whose pattern bounds what it reads by itself.
	bound *boundFunc

	// run is how a token's match runs on past where it is decided, and
	// valueIn the bytes a value is written in where a run of them is all it
	// is ([awsSecretByte]): what a text that arrives in pieces reads a match
	// or a value on with once its window has decided where it begins
	// ([Stream]). Nil for a token its pattern bounds, and for the password's
	// value, which is read on as [valueEnd] reads it.
	run     *tokenRun
	valueIn func(byte) bool
	// pending is, for an AWS secret's rule, its key and the spaces after it
	// up to the end of a text: where the pattern's match, or whether there is
	// one, waits on where the spaces end ([ruleStage.pendingSeparator]).
	pending *regexp.Regexp
}

// bounded is r held to bound ([rule.bound]).
func (r rule) bounded(bound *boundFunc) rule {
	r.bound = bound
	return r
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
// into a fresh buffer and then converts that buffer to a string. Every rule
// over a clean transcript would be two full copies of it — and a clean
// transcript is the overwhelming case, since this runs on every sandbox result
// and every executor's transcript whether or not a credential is in it.
func (r rule) apply(text string) string {
	if r.keyed == nil && !r.pattern.MatchString(text) {
		return text
	}
	// prior is every value replaced so far, which the next one is read
	// beside ([readValue]).
	var prior [][2]int
	start, end, also, found := r.nextAlso(text, 0, nil)
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
		if also[1] > also[0] {
			b.WriteString(text[done:also[0]])
			r.write(&b, text[also[0]:also[1]])
			done, end = also[1], also[1]
			prior = append(prior, also)
		}
		start, end, also, found = r.nextAlso(text, end, prior)
	}
	b.WriteString(text[done:])
	return b.String()
}

// write writes what the rule makes of replaced, one thing it replaces: its
// marker — or, for a rule with a value, a marker for each PIECE of the value
// between the escaped line breaks in it, the breaks written as they stand.
//
// A value runs on through an escaped break: `export PASSWORD=Xk9\nQ2#vL8`
// holds one password, `"pwd=hunter2\nnext"` a password and the next line, and
// the text cannot say which. Replaced whole, two lines would become one and a
// second pass read a key across them; ended at the break, a password's rest
// would be published. In pieces, nothing is published and every line stays,
// at the cost of redacting the word after a break that is a line's.
func (r rule) write(b *strings.Builder, replaced string) { writeRule(r, b, replaced) }

// writeRule is [rule.write] of a replaced stretch of either kind ([textual]).
func writeRule[T textual](r rule, b io.StringWriter, replaced T) {
	if r.value == nil {
		_, _ = b.WriteString(r.with)
		return
	}
	writeValue(b, r.with, replaced)
}

// writeValue writes replaced, a value, as a rule with a value writes it: with
// for each piece of it between its escaped line breaks, the breaks as they
// stand ([rule.write]).
func writeValue[T textual](b io.StringWriter, with string, replaced T) {
	w := valueWriter{with: with}
	writeStretch(&w, b, "", replaced)
	w.close(b)
}

// valueWriter writes a value as [writeValue] does, handed to it a stretch at a
// time: what a text that arrives in pieces ([Stream]) writes of a value whose
// end is not read yet, where the whole text writes it at once. writeValue is
// one handed the whole value, so the two are one implementation.
type valueWriter struct {
	with string
	// marked is whether the piece being written has its marker already, and
	// pending the end of the stretch handed last that the next can make an
	// escaped break of: a `\`, or the `\r` a break can open with, and the
	// `\` after it.
	marked  bool
	pending string
}

// write writes s, the next stretch of the value.
func (w *valueWriter) write(b io.StringWriter, s string) { writeStretch(w, b, "", s) }

// writeStretch is [valueWriter.write] of head and then s, a stretch of either
// kind ([textual]): head, a few backslashes a stream held, is read as the
// start of the stretch without joining the two, which would copy s.
func writeStretch[T textual](w *valueWriter, b io.StringWriter, head string, s T) {
	head = w.pending + head
	w.pending = ""
	// The stretch is head then s; from is where what is still to write
	// begins in it, an escaped break being two or four bytes that may
	// begin in head.
	n := len(head) + len(s)
	at := func(i int) byte {
		if i < len(head) {
			return head[i]
		}
		return s[i-len(head)]
	}
	from := 0
	for {
		i := -1
		for k := from; k < len(head) && k+1 < n; k++ {
			if at(k) == '\\' && at(k+1) == 'n' {
				i = k
				break
			}
		}
		if i < 0 {
			lo := max(from, len(head)) - len(head)
			if k := tIndex(s[lo:], `\n`); k >= 0 {
				i = len(head) + lo + k
			}
		}
		if i < 0 {
			break
		}
		next := i + 2
		if i-2 >= from && at(i-2) == '\\' && at(i-1) == 'r' {
			i -= 2
		}
		w.piece(b, i > from)
		if next-i == 4 {
			_, _ = b.WriteString(`\r\n`)
		} else {
			_, _ = b.WriteString(`\n`)
		}
		w.marked = false
		from = next
	}
	keep := 0
	switch {
	case n-from >= 3 && at(n-3) == '\\' && at(n-2) == 'r' && at(n-1) == '\\':
		keep = 3
	case n-from >= 2 && at(n-2) == '\\' && at(n-1) == 'r':
		keep = 2
	case n-from >= 1 && at(n-1) == '\\':
		keep = 1
	}
	w.piece(b, n-from > keep)
	w.pending = [...]string{"", `\`, `\r`, `\r\`}[keep]
}

// close writes what is left of the value.
func (w *valueWriter) close(b io.StringWriter) {
	w.piece(b, w.pending != "")
	w.pending = ""
}

// piece writes the marker of the piece being written where it holds anything
// and has none yet.
func (w *valueWriter) piece(b io.StringWriter, holds bool) {
	if holds && !w.marked {
		_, _ = b.WriteString(w.with)
		w.marked = true
	}
}

// next is what the rule replaces first at or after from: where it begins and
// ends, and whether there is any, prior being every value it replaced before
// from. Taken one after another, each from the end of the one before, these
// are the pattern's own leftmost, non-overlapping matches — or, for a rule
// with a value, the value after each.
//
// WHAT A RULE REPLACES NEVER TAKES WHAT A KEY IS READ ACROSS — a line break,
// an armour, a key's marker — or a second pass would read a key across lines
// the first read apart. So a password's rule replaces its value, never its key,
// separator or wrapping ([passwordValue]), around its escaped breaks
// ([rule.write]); and a match that runs into an armour is the match as if the
// text ended there.
func (r rule) next(text string, from int, prior [][2]int) (int, int, bool) {
	start, end, _, found := r.nextAlso(text, from, prior)
	return start, end, found
}

// nextAlso is [rule.next] with the second range a password's value names.
func (r rule) nextAlso(text string, from int, prior [][2]int) (int, int, [2]int, bool) {
	for {
		key, v, found := decideReadIn(r, text, from, prior)
		if !found {
			return 0, 0, v.also, false
		}
		if v.to > v.from {
			return v.from, v.to, v.also, true
		}
		from = beyond(text, key[0])
	}
}

// decide is what the rule decides first at or after from, prior being every
// value it replaced before from: where what decides it begins and ends (the
// match, which for a rule with a value is its key and the separator after it),
// what it replaces there, and whether it decides anything more. What it
// replaces is the match itself for a rule with no value, and for one with a
// value the value the key names, empty where it names none: [rule.next] goes
// on past that key, and [claimed] records it as named.
//
// ONE DECISION AT A TIME, so a reader of a text that arrives in pieces
// ([Stream]) decides with the same code as the whole text, and trusts each
// decision only where its window holds all it read.
func (r rule) decide(text string, from int, prior [][2]int) (key, replaced [2]int, found bool) {
	return decideIn(r, text, from, prior)
}

func decideIn[T textual](r rule, text T, from int, prior [][2]int) (key, replaced [2]int, found bool) {
	key, v, found := decideReadIn(r, text, from, prior)
	return key, [2]int{v.from, v.to}, found
}

// decideReadIn is [rule.decide] of a text of either kind ([textual]): what
// decides it, and what it replaces as a password's value's read
// ([aValue]), whether that value is its run read in the whole text and the
// stretches of a key's last run the read went past among it.
func decideReadIn[T textual](r rule, text T, from int, prior [][2]int) (key [2]int, v aValue, found bool) {
	for from <= len(text) {
		start, end, found := findIn(r, text, from)
		if !found {
			return key, v, false
		}
		switch {
		case r.read != nil:
			return [2]int{start, end}, callRead(r.read, text, start, end, prior), true
		case r.value != nil:
			v.from, v.to = callValue(r.value, text, start, end, prior)
			return [2]int{start, end}, v, true
		}
		at := armourIn(text, start, end)
		if at == end {
			return [2]int{start, end}, aValue{from: start, to: end}, true
		}
		// The match as if the text ended at the armour: the rule's first
		// match there, which no place before start has, since none had
		// one in the whole text. None, and the search goes on past it.
		if s, e, found := findIn(r, text[:at], start); found {
			return [2]int{s, e}, aValue{from: s, to: e}, true
		}
		from = at + reFindIndex(pemArmourAt(), text[at:])[1]
	}
	return key, v, false
}

// find is the rule's first match at or after from: its pattern's, held to its
// bound ([rule.bound]).
func (r rule) find(text string, from int) (int, int, bool) { return findIn(r, text, from) }

// findIn is [rule.find] in a text of either kind ([textual]).
func findIn[T textual](r rule, text T, from int) (int, int, bool) {
	for {
		start, end, found := matchIn(r, text, from)
		if !found || r.bound == nil {
			return start, end, found
		}
		within, resume := callBound(r.bound, text, start, end)
		if within {
			return start, end, true
		}
		// Past where the match began, whatever the bound answers, so the
		// search ends: a bound that named that place or one before it
		// would find the same match again for ever.
		from = max(resume, beyond(text, start))
	}
}

// matchIn is the pattern's first match at or after from.
func matchIn[T textual](r rule, text T, from int) (int, int, bool) {
	if r.keyed != nil {
		return keyedNext(r.keyed, text, from)
	}
	loc := reFindIndex(r.pattern, text[from:])
	if loc == nil {
		return 0, 0, false
	}
	return from + loc[0], from + loc[1], true
}

// armourIn is where the first armour line's armour ([pemArmour]) beginning in
// text[start:end] begins, or end. The armour is read from text past end, no
// further than its own 64 bytes of label: it holds spaces, which end every
// match a rule makes, so a match runs at most into its first word.
func armourIn[T textual](text T, start, end int) int {
	for i := start; i < end; {
		d := tIndex(text[i:end], "-----")
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
func armourAt[T textual](text T, i int) bool {
	return tHasPrefix(text[i:], "-----") && reMatch(pemArmourAt(), text[i:])
}

// rules are applied in order, which matters where one shape prefixes another
// (sk-proj- before sk-) or can HOLD another: the run token first, a Google
// access token next, a JSON Web Token after it ([runTokenPattern],
// [googleTokenPattern], [jwtPattern]). AN AWS SESSION'S SECRETS ARE VALUES read
// after a key ([awsSecretKeys]): AFTER EVERY TOKEN, so a vendor's key written
// straight after a value is taken whole first, and BEFORE THE PASSWORD'S RULE
// ([passwordInput]). RE2 keeps matching linear whatever the pattern; every rule
// opens with a literal it skips ahead on, or is [keyed]. BUILT AT FIRST USE,
// NOT AT INIT: Gitlet's push hook is this binary and pays every init for a
// process that redacts nothing, and a bounded repeat compiles large. A PRIVATE
// KEY IS NOT A RULE: it spans lines and may not have ended yet, so [keyBlocks]
// finds it before every rule, and a token in a key's body goes with the block.
var rules = sync.OnceValue(func() []rule {
	table := []rule{
		token(runTokenPattern, "run-token").bounded(&boundFunc{runTokenWithin[string], runTokenWithin[[]byte]}),
		token(googleTokenPattern, "google-token"),
		token(jwtPattern, "jwt").bounded(&boundFunc{jwtWithin[string], jwtWithin[[]byte]}),
		// A Google refresh token (`1//0` and some hundred base64url
		// characters), which a consent grant holds and a token endpoint's
		// error can echo, and a Google OAuth client's secret. Sixty at least,
		// so a run inside a base64 blob, where `+` and `/` end it within a
		// few dozen, is no token. Before the vendors' rules, for the Google
		// access token's reason: a long base64url body spells their prefixes.
		token(`1//0[0-9A-Za-z_-]{60,}`, "google-refresh-token"),
		token(`GOCSPX-[0-9A-Za-z_-]{20,}`, "google-client-secret"),
		token(`sk-proj-[A-Za-z0-9_-]{20,}`, "api-key"),
		token(`sk-ant-[A-Za-z0-9_-]{20,}`, "api-key"),
		token(`sk-[A-Za-z0-9_-]{20,}`, "api-key"),
		token(`xox[bpsare]-[A-Za-z0-9-]{20,}`, "slack-token"),
		// An AWS access key's id: a long-term key's, and a session's,
		// under which STS issues every temporary credential — `aws sts
		// assume-role`, an instance's role, the session a box is handed.
		// TWO RULES, NOT `A[KS]IA`: the class would leave RE2 the `A`
		// alone to skip ahead on.
		token(`AKIA[A-Z0-9]{16}`, "aws-key"),
		token(`ASIA[A-Z0-9]{16}`, "aws-key"),
		token(`gh[pousr]_[A-Za-z0-9]{36,}`, "github-token"),
		token(`github_pat_[A-Za-z0-9_]{50,}`, "github-token"),
		token(`glpat-[A-Za-z0-9_-]{20,}`, "gitlab-token"),
		token(`gl(?:rt|soat|ptt)-[A-Za-z0-9_-]{20,}`, "gitlab-token"),
		// DigitalOcean's: a personal access token — what a box on
		// DigitalOcean is handed as DIGITALOCEAN_ACCESS_TOKEN — and an
		// OAuth application's access and refresh tokens, each 64
		// hexadecimal digits after its prefix. THREE RULES, NOT
		// `do[opr]_v1_`, for the AWS ids' reason: the class would leave RE2
		// the `do` that `done`, `docs` and `do` itself open with.
		token(`dop_v1_[0-9a-f]{20,}`, "digitalocean-token"),
		token(`doo_v1_[0-9a-f]{20,}`, "digitalocean-token"),
		token(`dor_v1_[0-9a-f]{20,}`, "digitalocean-token"),
		token(sshCertificatePattern, "ssh-certificate").bounded(&boundFunc{sshCertificateWithin[string], sshCertificateWithin[[]byte]}),
	}
	for _, k := range awsSecretKeys {
		table = append(table, awsSecret(k.key, k.what))
	}
	password := passwordRule().valued(passwordMarker, &valueFunc{passwordValue[string], passwordValue[[]byte]})
	password.read = &readFunc{readValue[string], readValue[[]byte]}
	return append(table, password)
})

// runTokenPattern is the engine's OWN per-run token (internal/runtoken),
// `crewlet_rt1.<tag>.<subject>.<expiry>.<sig>`, matched BY ITS SHAPE: no node
// that collects an executor holds the tokens its launch minted, and a harness
// that prints its environment prints its run's (runtoken's
// TestAMintedTokenIsRedacted holds the two spellings to one). Each segment is
// as runtoken writes it; the subject holds no dot, whitespace, `[` (a marker's
// opening, so a second pass never reads a marker as a subject) or `\` (so no
// escaped break joins lines, and [Settled] holds nothing back). FIRST IN THE
// TABLE: its base64url can spell a vendor's prefix and body, and it can come
// after a vendor's key; taken after, a vendor's rule would leave its rest. It
// READS NO FURTHER THAN A TOKEN RUNS: its subject [MaxReach] at most
// ([runTokenWithin]), its expiry nineteen digits (an int64's Unix time).
const runTokenPattern = `crewlet_rt1\.[A-Za-z0-9_-]{8}\.[^.\s\[\\]*\.[0-9]{1,19}\.[A-Za-z0-9_-]{43}`

// runTokenWithin is [rule.bound] for a run token: its subject [MaxReach] at
// most. A match whose subject is longer is no token, and the search goes on
// from the byte after where it began, so a token beginning inside the refused
// match is still found there: a subject can end in the literal, the subject's
// own dot then following it, and the token read from there takes the refused
// one's expiry for its tag. That reads the refused subject once more, looking
// for the literal, and no more than once, since a subject holds no dot: a
// match beginning inside it reads its own subject only past the refused
// subject's end.
func runTokenWithin[T textual](text T, start, end int) (bool, int) {
	token := text[start:end]
	tag := tIndexByte(token, '.') + 1
	subject := tag + tIndexByte(token[tag:], '.') + 1
	return tIndexByte(token[subject:], '.') <= MaxReach, beyond(text, start)
}

// googleTokenPattern is a Google OAuth access token, as `gcloud auth
// print-access-token` prints it: `ya29.` and a base64url body, which a service
// account's writes in segments a dot apart (`ya29.c.…`), ending on a character
// of its own, since a full stop after a token is the sentence's; twenty
// characters at least, so a word after the prefix in prose is no token. SECOND
// IN THE TABLE, before the vendors' rules for the run token's reasons
// ([runTokenPattern]): it too can come after a vendor's key. And after the
// run token: read first, a Google token opening in a run token's segment
// ending in `ya29` would run across its dots and leave its head unmatched.
const googleTokenPattern = `ya29\.[A-Za-z0-9_.-]{19,}[A-Za-z0-9_-]`

// jwtPattern is a JSON Web Token in its compact form,
// `<header>.<claims>.<signature>`: an OpenID Connect ID token, a Kubernetes
// service account's, a workload identity federation's, a bearer. Header and
// claims each open `eyJ`, `{"` in base64url, which keeps a run of base64url
// that merely starts so from reading as a token; the signature may be empty.
// THIRD IN THE TABLE: before the vendors' rules for the Google token's reasons
// ([googleTokenPattern]), and after the Google token, whose dotted body can
// hold what reads as a JWT. It READS NO FURTHER THAN A TOKEN RUNS: header and
// claims [MaxReach] at most together ([jwtWithin]), or every `eyJ` in a long
// dotless run of base64url would hold the rest of it undecided.
const jwtPattern = `eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`

// jwtWithin is [rule.bound] for a JSON Web Token: its header, the dot after it
// and its claims [MaxReach] at most together. A match whose header and claims
// run longer is matched as the pattern with that bound would match it, from
// the leftmost `eyJ` from which they fit: one inside its header, its head
// before it published, or a later one. None begins more than MaxReach before
// these claims end, since every match beginning after this one's start has
// claims that end there or later, so the search goes on from there, and a
// header holding many an `eyJ` is not read again from each.
func jwtWithin[T textual](text T, start, end int) (bool, int) {
	header := start + tIndexByte(text[start:end], '.')
	claims := header + 1 + tIndexByte(text[header+1:end], '.')
	return claims-start <= MaxReach, claims - MaxReach
}

// sshCertificatePattern is an OpenSSH certificate as a line writes it, from its
// type's `-cert-v01@openssh.com` suffix through its base64 body: what a
// certificate file, `ssh-keygen -L`'s input and an agent's pasted line hold.
// Opened on the suffix, a literal RE2 skips ahead on; the key type before it
// names nothing and stays.
//
// ITS WHITESPACE IS [MaxReach] AT MOST ([sshCertificateWithin]). It is
// replaced with the body, so it is known to be a certificate's only once forty
// characters of base64 follow it, and a run of spaces read without bound held
// everything after the suffix undecided. OpenSSH writes one space there.
const sshCertificatePattern = `-cert-v01@openssh\.com[ \t]+[A-Za-z0-9+/]{40,}={0,3}`

// sshCertificateWithin is [rule.bound] for an SSH certificate: the whitespace
// between its type's suffix and its body [MaxReach] at most. A match with more
// is none, and the search goes on past where it began; the suffix is spelled
// in neither whitespace nor base64, so no other match begins before the body
// ends.
func sshCertificateWithin[T textual](text T, start, end int) (bool, int) {
	match := text[start:end]
	space := match[tIndexAny(match, " \t"):]
	return len(space)-len(tTrimLeft(space, " \t")) <= MaxReach, beyond(text, start)
}

// token is the rule that replaces each match of pattern with the marker for
// what it is.
func token(pattern, what string) rule {
	return rule{pattern: regexp.MustCompile(pattern), with: Marker + what + "]", run: tokenRunOf(pattern)}
}

// awsSecretKeys are the keys an AWS session's secret access key and session
// token are written under, and what the value after one becomes: STS's and a
// credential process's (`SessionToken`, `SecretAccessKey`), an SDK's, the
// credentials file's, the environment's (`AWS_SESSION_TOKEN`,
// `AWS_SECURITY_TOKEN`), a request's header and a presigned URL's parameter
// (`X-Amz-Security-Token`), a flag's (`--session-token`) ([awsSecret]). A
// SESSION'S SECRETS HAVE NO SHAPE, so each is read by its key, as a password
// is; a run obtains a session by itself (`aws sts assume-role`), so no value a
// launch holds is the one it prints. EACH KEY FROM ITS SECOND LETTER, in each
// case written: a literal RE2 skips ahead on, the first letter being a
// format's case and what precedes it (`aws_`, `X-Amz-`) a prefix the rule
// need not read; read in any case, a [keyed] scan would stop at every `s`.
var awsSecretKeys = []struct{ key, what string }{
	{`ession(?:Token|_token|-token)`, "session-token"},
	{`ESSION_TOKEN`, "session-token"},
	{`ecurity(?:Token|_token|-[Tt]oken)`, "session-token"},
	{`ECURITY_TOKEN`, "session-token"},
	{`ecret(?:AccessKey|_access_key|-access-key)`, "secret-access-key"},
	{`ECRET_ACCESS_KEY`, "secret-access-key"},
}

// awsSecretSeparator is what joins an AWS secret's key to its value, on the
// key's line: what closes a password's key and separates it from its value
// ([passwordClose], [passwordSeparator]), or the spaces alone a command's
// argument is written after its flag with (`aws configure set
// aws_session_token …`), either's spaces being spaces, tabs or escaped
// line breaks ([awsSpace]). NEVER A REAL LINE BREAK, so the rule reads
// nothing [Settled] has to hold back: an escaped one is a string's, on the
// same real line. And [MaxReach] AT MOST ([awsSecretValue]): the value's
// decision reads back across it to its key.
const awsSecretSeparator = `(?:` + passwordClose + awsSpace + `*` + passwordSeparator + awsSpace + `*|` + awsSpace + `+)`

// awsSpace is a space of an AWS secret's separator ([awsSecretSeparator]): a
// space, a tab, or an escaped line break, the `\n` a JSON string writes for
// one, with the backslashes each string it is written in adds before it, as
// a value reads them ([valueBack]).
const awsSpace = `(?:[ \t]|\\+[nr])`

// awsSecret is the rule that replaces, with the marker for what, the value
// after an AWS secret's key, key being a pattern of that key's spelling
// ([awsSecretKeys]): the value alone ([awsSecretValue]), the key and its
// separator left as they are, as a password's are.
func awsSecret(key, what string) rule {
	return rule{
		pattern: regexp.MustCompile(key + awsSecretSeparator),
		with:    Marker + what + "]",
		value:   &valueFunc{awsSecretValue[string], awsSecretValue[[]byte]},
		valueIn: awsSecretByte,
		pending: regexp.MustCompile(`(?:` + key + `)` + passwordClose + awsSpace + `*\\*\z`),
	}
}

// AN AWS SECRET'S VALUE is a run of what [awsSecretByte] admits at least
// awsSecretMin characters long, holding a digit, a `+` or a `/` unless it is
// awsSecretWhole long ([awsSecretValue]).
//
// FORTY, a secret access key's length — a session token's is hundreds — so
// either is taken whatever it holds. A shorter run is taken where it reads as
// random, since a program writes a name after such a key
// (`sessionToken = currentSessionToken`), which is no secret. And SIXTEEN,
// below which what such a key names is a placeholder (`test`) or a word.
const (
	awsSecretMin   = 16
	awsSecretWhole = 40
)

// awsSecretValue is where the value an AWS secret's key names begins and ends,
// the key and its separator ending at end: past the one quote that may open
// it, a run of what such a value is written in, where it reads as a value; or
// end twice, where none does ([rule.value]). None where the separator runs
// past [MaxReach], as though the text ended inside it.
func awsSecretValue[T textual](text T, start, end int, _ [][2]int) (int, int) {
	if awsSeparatorLen(text[start:end]) > MaxReach {
		return end, end
	}
	from := end
	q := from
	for q < len(text) && text[q] == '\\' {
		q++
	}
	if q < len(text) && tIndexByte("\"'`", text[q]) >= 0 {
		// The value's opening quote, escaped as its key's close may be.
		from = q + 1
	}
	to, random := from, false
	for ; to < len(text) && awsSecretByte(text[to]); to++ {
		c := text[to]
		random = random || '0' <= c && c <= '9' || c == '+' || c == '/'
	}
	if n := to - from; n < awsSecretMin || n < awsSecretWhole && !random {
		return end, end
	}
	return from, to
}

// awsSeparatorLen is how long the separator ending match, an AWS secret's key
// and its separator ([awsSecretSeparator]), is: what follows the key's last
// letter, since every key in [awsSecretKeys] ends in one and a separator holds
// no letter or digit but an escaped line break's.
func awsSeparatorLen[T textual](match T) int {
	key := len(match)
	for key > 0 {
		switch c := match[key-1]; {
		case !isAlnum(rune(c)):
			key--
		case (c == 'n' || c == 'r') && key >= 2 && match[key-2] == '\\':
			key--
			for key > 0 && match[key-1] == '\\' {
				key--
			}
		default:
			return len(match) - key
		}
	}
	return len(match) - key
}

// awsSecretByte reports whether c is a byte an AWS secret's value is written
// in: base64's, its padding included ([inBase64]), and `%`, with which a URL
// escapes base64's `+`, `/` and `=`. NEVER A `-`, so a value never runs on
// into an armour written straight after it; nor a bracket, so a marker where
// a value was is no value, and redacting twice is redacting once; nor a
// backslash, so a value never takes an escaped line break.
func awsSecretByte(c byte) bool {
	return inBase64(c) || c == '%'
}

// passwordMarker is what a password's value reads as once it is redacted.
const passwordMarker = Marker + "password]"

// passwordKeys are the words a password's assignment opens with, and THE ONE
// SPELLING of them: [passwordRule], [pendingKey] and the scan that finds where
// either can begin are all built from this list, so a word added here is
// redacted, held back by [Settled] and scanned for at once, with no second list
// to drift from it.
var passwordKeys = []string{"password", "passwd", "pwd"}

// NamesPassword reports whether name — the key a structured document names a
// value by, a JSON object's member — names a password: it holds one of
// [passwordKeys], whatever its case. What a reader that takes a document apart
// asks of a key, where the rules read the key in the text itself: one list, so
// a word added there is a key both readings take a value under.
func NamesPassword(name string) bool {
	lower := strings.ToLower(name)
	for _, key := range passwordKeys {
		if strings.Contains(lower, key) {
			return true
		}
	}
	return false
}

// PasswordKeyMaxBytes is the longest of [passwordKeys] in bytes, and so the
// most runes of a name one of them spans once [NamesPassword] lowers it, since
// every rune lowers to a byte or more. A reader that has a name a piece at a
// time, and cannot hold it whole, asks NamesPassword of stretches of the name
// that overlap by one rune fewer than this, and finds every key the whole
// name's reading finds, with no second copy of the list.
func PasswordKeyMaxBytes() int {
	n := 0
	for _, key := range passwordKeys {
		n = max(n, len(key))
	}
	return n
}

// passwordClose is what closes a password's key before its separator, and
// passwordSeparator the separator: the quote or the markup a key is written
// in, and the operator a language assigns or compares it with. Each byte of
// the close may be escaped, as a key written inside a string is, once for
// each string around it.
//
// THE SEPARATOR IS WHAT THE TEXT WRITES: a JSON object's key is quoted, a
// label bold, and `:=`, `==`, `=>` assign. Read as a bare `:` or `=` straight
// after the key, a quoted or bold key is none and its value published, and an
// operator's second character is taken for the value; so each operator is
// tried before the one character it opens with.
const (
	passwordClose     = `(?:\\*["'` + "`" + `*_]){0,2}`
	passwordSeparator = `(?::=|==|=>|[:=])`
)

// separated is k, a pattern of a password's key and its separator, refusing a
// key closed by a quote that does not close the key's own string
// ([quotedElsewhere]) and one whose separator begins [MaxReach] or more past
// where the key does ([separatorWithin]).
func separatedRefuses[T textual](text T, at int) bool {
	return !separatorWithin(text, at) || quotedElsewhere(text, at)
}

func separated(k *keyed) *keyed {
	k.refuses = &refusesFunc{separatedRefuses[string], separatedRefuses[[]byte]}
	return k
}

// awaiting is k, a pattern of a password's key at the text's end with its
// separator still to come, refusing a key closed by a quote that does not
// close the key's own string ([quotedElsewhere]) and one [MaxReach] or more
// before the text's end: a separator written next would begin too far from it
// to make it a key ([separatorWithin]), so it waits for none.
func awaitingRefuses[T textual](text T, at int) bool {
	return len(text)-at >= MaxReach || quotedElsewhere(text, at)
}

func awaiting(k *keyed) *keyed {
	k.refuses = &refusesFunc{awaitingRefuses[string], awaitingRefuses[[]byte]}
	return k
}

// separatorWithin reports whether the separator of the password's key at at,
// a match of a pattern that holds one, begins among the first [MaxReach]
// bytes from it: the first `:` or `=` after the key, which neither its word,
// what closes it nor whitespace holds.
//
// A WORD IS A KEY ONLY WHERE A SEPARATOR FOLLOWS IT, and whitespace may come
// between, across lines: `Enter password` and then `= swordfish`. Read without
// bound, whether a word was a key waited on the whitespace after it however
// far it ran. Past the bound it is no key, as though the text ended before the
// separator came; and the value of one whose separator is that far from it
// lies past what its key reads anyway ([readValue]).
func separatorWithin[T textual](text T, at int) bool {
	return tContainsAny(text[at:min(len(text), at+MaxReach)], ":=")
}

// quotedElsewhere reports whether the password's key at at is closed by a
// quote that is not the end of the key's own string: one whose opening quote
// does not stand before the key, or before a name run into it — letters,
// digits, `_`, `-` and `.`, as `"db_password"` and `'reset-password'` are.
//
// A QUOTED KEY IS A STRING THAT IS A NAME: read wherever a quote closed it,
// every string ending in the key's word was one (`"POST /auth/password": () =>`
// lost its `()`). Markup is not held to it: `**Admin password**:` is a label.
// It costs a name in a string that is no key where it stands, as in a choice
// between two strings. THE NAME IS READ BACK [MaxReach] AT MOST: one running
// past the bound would be refused and its value published, so it is taken as
// the key's own string, which costs redacting the value after a longer one.
func quotedElsewhere[T textual](text T, at int) bool {
	i := at
	for i < len(text) {
		r, size := tDecodeRune(text[i:])
		if !unicode.IsLetter(r) {
			break
		}
		i += size
	}
	q := byte(0)
	for j, n := i, 0; n < 2; n++ {
		// Each byte of the close may be escaped ([passwordClose]).
		for j < len(text) && text[j] == '\\' {
			j++
		}
		if j == len(text) || tIndexByte("\"'`*_", text[j]) < 0 {
			break
		}
		if text[j] == '"' || text[j] == '\'' {
			q = text[j]
			break
		}
		j++
	}
	if q == 0 {
		return false
	}
	j, floor := at, max(0, at-MaxReach)
	for j > floor && keyName(text[j-1]) {
		j--
	}
	if j > 0 && j == floor {
		// The name runs on past the bound: the key's own string.
		return false
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
// THE VALUE IS WHAT IS REPLACED, the key and separator left as they were, line
// breaks and all ([rule.next]). Its separator begins within [MaxReach] of the
// key ([separatorWithin]).
var passwordRule = sync.OnceValue(func() *keyed {
	return separated(keyedOn(passwordKeys, passwordClose+`\s*`+passwordSeparator+passwordTrail))
})

// pendingKey is a password key at the very END of a text with nothing but
// what closes it and whitespace after it: a [passwordRule] match whose
// separator may still be written, across the line break (`Enter password`
// and then `= swordfish`), within [MaxReach] of the key ([awaiting]).
var pendingKey = sync.OnceValue(func() *keyed {
	return awaiting(keyedOn(passwordKeys, passwordClose+`\s*\z`))
})

// passwordPrompt is a password key and its separator at the very end of a line
// ([promptAt]), a key exactly where [passwordRule] reads one.
var passwordPrompt = sync.OnceValue(func() *keyed {
	return separated(keyedOn(passwordKeys, passwordClose+`\s*`+passwordSeparator+`\s*\z`))
})

// passwordValue is where the value a [passwordRule] match names begins and
// ends, the match beginning at start and ending at end and prior being every
// value the rule replaced before it, in order — or end twice where it names
// none: [readValue], with nothing written after text.
func passwordValue[T textual](text T, start, end int, prior [][2]int) (int, int) {
	v := readValue(text, start, end, prior)
	return v.from, v.to
}

// readValue is the value a [passwordRule] match names, from start to end, prior
// being every value the rule replaced before it, in order ([aValue]): the
// text's own reading ([readValuePlain]) and, where the key stands in a logged
// JSON string escaped in layers, the string's decoded ([decodedReading]).
func readValue[T textual](whole T, start, end int, prior [][2]int) aValue {
	v := readValuePlain(whole, start, end, prior)
	if o, ok := decodedReading(whole, start, end, prior); ok && (o.to > o.from || o.also[1] > o.also[0]) {
		// A LOGGED STRING IS READ AS IT IS AND DECODED: every range either
		// reading names is hidden, so it only hides more than the text's own
		// reading. One that names nothing adds nothing: it decides inside
		// the key's raw line, where no hold stands ([valueHolds]).
		v = unionReading(v, o)
	}
	return v
}

// readValuePlain is [readValue] of the text as it is written: the reading of
// [readValueAs], and where the key's line opens a string after a quote, which
// the text does not say is the one the escaped breaks belong to, the readings
// from the string's quote and from the line's start as well. A reading on a
// later piece that the first overlaps joins it; one that does not is
// [aValue]'s also, the second range, held to [MaxReach] past the key and not
// to its line, since a reading can join two later pieces. Read one way alone,
// a value the other way names was published, and a second pass read the line
// the first wrote otherwise.
func readValuePlain[T textual](whole T, start, end int, prior [][2]int) aValue {
	v := readValueAs(whole, start, end, prior, 0)
	if !v.firstQuote {
		return v
	}
	for _, pick := range []int{1, 2} {
		o := readValueAs(whole, start, end, prior, pick)
		// EVERY READING HOLDS WHAT IT READS, whatever it finds: a text cut
		// before the line that decides a reading has no result from it yet,
		// so the value is decided only where each reading is ([valueHolds]).
		// Held by the result alone, a cut text read no second range, and the
		// value a later line gives was shown past the cut.
		v.decided, v.unread = max(v.decided, o.decided), v.unread && o.unread
		// Every reading's key's runs are the keys' ([aValue]).
		v.runs = append(v.runs, o.runs...)
		if o.to > o.from && (o.from < end || sameLine(whole, o.from, start)) {
			// A reading on the key's own line is no reading of a piece.
			continue
		}
		if o.to <= o.from || o.from < v.to && v.from < o.to && v.to > v.from {
			if o.to > o.from && v.to > v.from {
				v.from, v.to = min(v.from, o.from), max(v.to, o.to)
			}
			continue
		}
		o.to = min(o.to, len(whole), start+MaxReach)
		if o.to <= o.from {
			continue
		}
		switch {
		case v.to <= v.from:
			v.from, v.to, v.ran = o.from, o.to, false
		case v.also[1] <= v.also[0]:
			v.also = [2]int{o.from, o.to}
		case o.from < v.also[1] && v.also[0] < o.to:
			v.also = [2]int{min(v.also[0], o.from), max(v.also[1], o.to)}
		}
	}
	if v.also[1] > v.also[0] && v.also[0] < v.to && v.from < v.also[1] {
		v.from, v.to, v.also = min(v.from, v.also[0]), max(v.to, v.also[1]), [2]int{}
	}
	if v.also[1] > v.also[0] && v.also[0] < v.from {
		v.from, v.to, v.also = v.also[0], v.also[1], [2]int{v.from, v.to}
		v.ran = false
	}
	v.runs = mergeSpans(v.runs)
	return v
}

// quotedMarker is where a marker begins in text at i past a quote, escaped or
// not, that opens it, or i where none does: the quote a value was written in,
// left standing when a pass replaced the value inside it.
func quotedMarker[T textual](text T, i int) int {
	for _, q := range []string{`\"`, `"`, `'`} {
		if tHasPrefix(text[i:], q) && markerLen(text[i+len(q):]) > 0 {
			return i + len(q)
		}
	}
	return i
}

// sameLine reports whether a and b, a <= or > b, lie on one line of text.
func sameLine[T textual](text T, a, b int) bool {
	a, b = min(a, b), max(a, b)
	end, _ := lineAt(text, a)
	return b <= end
}

// readValueAs is the value a [passwordRule] match from start to end names, prior
// every value replaced before it ([aValue]), a first piece's gutter picked by
// pick: 0 by the line read, 1 from the string's quote, 2 from the line's start.
//
// A VALUE IS READ WHERE ITS WRAPPING PUTS IT, by the key reader's machinery
// ([lineAt], [shapeAt]): taken as the first word past the separator across
// lines, it was the wrapping's syntax (a line number, an `echo`'s quote) and
// the value below was published. On the key's line, past its markup
// ([pastMarkup]), a first word holding a letter or digit, or `""`, is the
// value; punctuation alone is the wrapping's SUFFIX where the next line not
// blank ends in it ([closingOf], [outermost]), else the value, as `***` is;
// nothing, or an escaped break, sends the read below ([valueRead.lines]).
func readValueAs[T textual](whole T, start, end int, prior [][2]int, pick int) (v aValue) {
	text := whole[:min(len(whole), start+MaxReach)]
	end = min(end, len(text))
	sep := start + tIndexAny(text[start:end], ":=") + 1
	r := valueRead[T]{text: text, whole: whole, start: start, end: end, from: end, prior: prior, pick: pick}
	defer func() { v.firstQuote = len(r.wrapper) != 0 && !r.later }()
	if !tContains(text[sep:end], "\n") && !tContains(text[sep:end], `\n`) {
		// The separator's line goes on: what follows it there, past the
		// markup that closes what the line opened, its first word read as
		// the rule's `\S+` read it.
		r.from, r.opening = pastMarkup(text, start, sep-1, end, prior)
		if q := quotedMarker(text, r.from); q > r.from {
			// A quote and then a marker: a pass took the value, as a
			// marker where the value begins says ([valueRead.lines]).
			r.from = q
		}
		word := wordEnd(text, r.from)
		switch {
		case word > r.from && isValue(text[r.from:word]):
			return aValue{from: r.from, to: keyLineValue(whole, text, r.from), decided: end, ran: true}
		case word > r.from:
			lineEnd, named := lineEndUnnamed(text, word)
			if named {
				// Another key on the line: a suffix holds none.
				return aValue{from: r.from, to: keyLineValue(whole, text, r.from), decided: end, ran: true}
			}
			r.setSuffix(trimLine(text[r.from:lineEnd]))
		case r.from > end:
			// The markup alone ends the key's line, or its piece of a
			// string: read with no suffix, a blank row below gave its
			// closing for the value and the value under it was shown.
			markup := tTrimRight(text[end:r.from], " \t")
			r.setSuffix(outermost(trimLine(markup)))
			r.markupOnly = true
			r.closer = [2]int{end + len(markup) - len(r.suffix), end + len(markup)}
		}
	}
	lineEnd, next := lineAt(text, sep)
	escaped := escapedOnly(lineEnd, next, len(whole))
	switch {
	case escaped && len(r.suffix) == 0:
		var named bool
		if next, escaped, named = r.restOfLine(next); named {
			// Another key on the rest of the line: a suffix holds none.
			if v, ok := restValue(whole, text, next); ok {
				v.decided, v.ran = end, true
				return v
			}
			return aValue{from: r.from, to: valueEnd(whole, r.from), decided: end, ran: true}
		}
	case escaped && r.markupOnly && r.keyOnRest(next):
		// The markup closes the key's piece of a string, and the rest of
		// the line names its own value.
		if v, ok := restValue(whole, text, next); ok {
			v.decided, v.ran = end, true
			return v
		}
		return aValue{from: r.from, to: valueEnd(whole, r.from), decided: end, ran: true}
	}
	return r.lines(next, escaped)
}

// keyLineValue is where a value read on a key's own line from from ends: past
// its first word, or, beside another key on the line, past markup to the first
// word after it that is a value ([ownMarkup]) and that key's label: the label's
// words are the key's (`*** Retype password:`), no value.
func keyLineValue[T textual](whole, text T, from int) int {
	return valueEnd(whole, ownMarkup(text, from, len(text), keyBefore(text, from)))
}

// restValue is the value on the rest of a line an escaped break left, from
// rest, before a key on that rest, where markup opens the rest and a value
// follows it there ([ownMarkup]), and whether one does: read as a line below
// the key is, the break being the string's line break. The key's label is the
// key's (`- Confirm password:`), and markup with no value before it is read
// as it was.
func restValue[T textual](whole, text T, rest int) (aValue, bool) {
	at := ownMarkup(text, rest, len(text), keyBefore(text, rest))
	return aValue{from: rest, to: valueEnd(whole, at)}, at > rest
}

// outermost is the last closing of markup, the run of its last byte: the
// table cell's `|` of `** |`, or `**` alone. It is what closes the first thing
// the key's line opened, and so the one closing a line of the same wrapping
// writes whatever its content: read as the whole of the markup, the suffix of
// `| **Password:** |` was `** |`, so a table's blank row `|  |` was no blank
// line of it, its pipe was taken for the value and the value on the row
// below published.
func outermost[T textual](markup T) T {
	last := markup[len(markup)-1]
	return markup[len(tTrimRight(markup, string(rune(last)))):]
}

// ownMarkup is where the value read on a line from from begins, the line
// ending at end at the latest, or at its first break: at from, where the first
// word there is a value ([isValue]); or, where that word and those after it are
// markup alone (a table's `|`, a bullet's `-`, a quote's `>`, a list's number,
// [listMarker]) and a word that is a value follows on the line, at that word,
// the markup read as the line's own and replaced with it. Where keyBefore says
// a key begins at or before that word, or the keys' reading holds it
// ([keysHold]), the markup is the value, as a
// masked `***` is: the key names its own value, and the run is the key's.
// It reads no further than the first value word, so a line of many keys is
// read once ([TestALineOfManyPasswordKeysIsLinear]).
func ownMarkup[T textual](text T, from, end int, keyBefore func(w int) bool) int {
	for i := from; i < end; {
		w := i + 1
		for w < end && !valueSpace(text[w]) && !tHasPrefix(text[w:], `\n`) {
			w++
		}
		if isValue(text[i:w]) && (i > from || !listMarker(text[i:w])) {
			if i > from && (keysHold(text, i, w) || keyBefore(w)) {
				return from
			}
			return i
		}
		for i = w; i < end && (text[i] == ' ' || text[i] == '\t'); {
			i++
		}
		if i < end && (valueSpace(text[i]) || tHasPrefix(text[i:], `\n`)) {
			// The line ends in markup alone, the value: read no further, so
			// a key's read costs its own line, whatever follows.
			return from
		}
	}
	return from
}

// keyBefore is [ownMarkup]'s keyBefore for a value read from from: whether
// the first password's key at or past from, read no further than [MaxReach]
// past the word ending at w, has a label beginning before w. Only a key
// beginning before [labelEnd] can, a key's word opening with a letter, so
// none past it is read.
func keyBefore[T textual](text T, from int) func(w int) bool {
	return func(w int) bool {
		k := keyFrom(text, from, labelEnd(text, w, w+MaxReach), w+MaxReach)
		return k >= 0 && labelStart(text, from, k) < w
	}
}

// keyTried, where an in-package test sets it, is called at each place keyFrom
// tries the pattern, so a test counts the tries a read costs, which a clock
// on a loaded runner cannot pin; nil otherwise.
var keyTried func()

// keyFrom is where the first password's key at or past from begins, read no
// further than bound, or -1, or -1 too where that key begins at by or past it.
func keyFrom[T textual](text T, from, by, bound int) int {
	k, text := passwordRule(), text[from:min(len(text), bound)]
	by = min(by-from, len(text))
	for i := keyedBeginsBefore(k, text, 0, by); i >= 0; i = keyedBeginsBefore(k, text, beyond(text, i), by) {
		if keyTried != nil {
			keyTried()
		}
		if keyedMatchesAt(k, text, i) {
			return from + i
		}
	}
	return -1
}

// keysHold reports whether the keys' reading holds the word text[i:w]: a key's
// marker, an armour, or a run of a key's body ([keysRun]), by the key reader's
// own predicates. ASKED BY SHAPE, NOT OF THE HOLDS: the claims read every value
// before the keys read their blocks (keySpans excludes what is claimed), so no
// block exists yet when a value is read; where it does, it is a marker.
func keysHold[T textual](text T, i, w int) bool {
	return markerLen(text[i:]) > 0 || armourAt(text, i) || keysRun(text[i:w])
}

// keysRun reports whether word is a run of a private key's body ([keyRun]) and
// nothing else: base64 alone. A value glued to a key's last run
// (`q8Zr4TfL2w==-placeholder9x`) is no key's run, and is read as a value.
func keysRun[T textual](word T) bool {
	for i := 0; i < len(word); i++ {
		if c := word[i]; !isAlnum(rune(c)) && c != '+' && c != '/' && c != '=' {
			return false
		}
	}
	return keyRun(tString(word))
}

// holdsKeysWord reports whether the keys' reading would take a word of s, apart
// at its spaces: an armour or a key's run ([keysHold]). Not a marker: one there
// is the value a pass took, and the line is the wrapping's as it was.
func holdsKeysWord[T textual](s T) bool {
	for i := 0; i < len(s); {
		for i < len(s) && valueSpace(s[i]) {
			i++
		}
		w := i
		for w < len(s) && !valueSpace(s[w]) {
			w++
		}
		if w > i && (armourAt(s, i) || keysRun(s[i:w])) {
			return true
		}
		i = w
	}
	return false
}

// listMarker reports whether word is a numbered list's marker: digits, then a
// `.` or a `)`. Read as markup only where it opens the line's content and a
// value follows it there ([ownMarkup]); alone, it is the value, as any word
// is.
func listMarker[T textual](word T) bool {
	n := 0
	for n < len(word) && '0' <= word[n] && word[n] <= '9' {
		n++
	}
	return n > 0 && n == len(word)-1 && (word[n] == '.' || word[n] == ')')
}

// wordEnd is where the word that begins at from ends, as the rule's `\S+` read
// it: at the next space, or at an escaped break.
func wordEnd[T textual](text T, from int) int {
	for from < len(text) && !valueSpace(text[from]) && !tHasPrefix(text[from:], `\n`) {
		from++
	}
	return from
}

// pastMarkup is where what follows a password's separator, which begins at
// sep, is read from, the match beginning at start and ending at end: past the
// markup that closes what the key's line opened before the key's label
// ([labelStart]) — `**`, a backtick, a table cell's `|` — and the spaces after
// it, or end where none does; `**Root password**: **x**` leaves the value
// its own. THE MARKUP IS THE KEY'S: read as the value, or as the suffix under
// `- **Password:**`, it was replaced and the value published. No quote is
// markup here (`msg="Enter password:" level=info`), nor a pipe the line did
// not open (`echo Password: | tee -a log`). The opening is read back over the
// label alone, not from the line's start, which made many keys quadratic; as
// the rule leaves it, prior's last value a marker; and no further than
// [MaxReach].
func pastMarkup[T textual](text T, start, sep, end int, prior [][2]int) (int, [2]int) {
	floor := max(0, start-MaxReach)
	if len(prior) > 0 {
		floor = max(floor, prior[len(prior)-1][1])
	}
	j := labelStart(text, floor, start)
	closed := tTrimRight(text[start:sep], " \t\r\n")
	k := len(closed)
	for k > 0 && tIndexByte("\"'`*_", closed[k-1]) >= 0 {
		k--
	}
	// inner is where the opening read ends, nearest the label.
	inner := -1
	for ; k < len(closed); k++ {
		// The key's own close, innermost first, takes the opening nearest
		// its label.
		for j > floor && isSpace(text[j-1]) {
			j--
		}
		if j == floor || text[j-1] != closed[k] {
			break
		}
		inner = max(inner, j)
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
		if i == len(text) || j == floor || text[i] != text[j-1] || tIndexByte("*_`|", text[i]) < 0 {
			break
		}
		inner = max(inner, j)
		i, j = i+1, j-1
		from = i
	}
	if from == end {
		return end, [2]int{}
	}
	for from < len(text) && isSpace(text[from]) {
		from++
	}
	return from, [2]int{j, inner}
}

// labelStart is where the label a password's key at start ends begins: back
// over the words of it — letters and digits in any script, the spaces between
// them, and the `-`, `'`, `.`, `(`, `)` and `/` a name is written with, and
// `_` inside a word — to floor at most. `**Root password:**` is one label, and
// `db_password` one word, whose `_` opens nothing.
//
// A KEY'S MARKER is read back across as the words it replaced (the package
// doc's "A key's block beside a password"). Every key reads back no further
// than the punctuation before its label, and a key's separator is
// punctuation, so a line of many keys is read back once.
func labelStart[T textual](text T, floor, start int) int {
	j := start
	for j > floor {
		if text[j-1] == ']' && tHasSuffix(text[floor:j], keyMarker) {
			j -= len(keyMarker)
			continue
		}
		r, size := tDecodeLastRune(text[floor:j])
		switch {
		case labelRune(r):
		case r == '_' && wordRune(tDecodeLastRune(text[floor:j-size])) && wordRune(tDecodeRune(text[j:])):
		default:
			return j
		}
		j -= size
	}
	return j
}

// labelRune reports whether r is one a label is written in ([labelStart]).
func labelRune(r rune) bool {
	return r == ' ' || r == '\t' || unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-'.()/", r)
}

// labelEnd is the first place at or past w, where a word ends (at a space, an
// escaped break or the text's end, so never inside a rune or a key's marker),
// short of bound, where a label read back to before w would stop
// ([labelStart]): a rune no label is written in, outside a key's marker. A key
// further on has a label beginning at w or past it, so a search for a key
// whose label begins before w ends there ([keyBefore]) and costs the label's
// run, not [MaxReach] of the text.
func labelEnd[T textual](text T, w, bound int) int {
	bound = min(bound, len(text))
	for w < bound {
		if tHasPrefix(text[w:], keyMarker) {
			w += len(keyMarker)
			continue
		}
		r, size := tDecodeRune(text[w:])
		if r != '_' && !labelRune(r) {
			return w
		}
		w += size
	}
	return min(w, bound)
}

// aValue is where the value a [passwordRule] match names begins and ends — or
// where the match ends, twice, where it names none — and where the line that
// DECIDED it begins: a text cut at a line start after the key and at or before
// that line reads the value as though nothing were written after the cut
// ([valueHolds]). unread is whether it reads the same there, the value being
// the one the read gives with no line after the key's to decide it
// ([valueRead.unread]). A read no line written yet decides is PENDING, a line
// written next still possibly the value, and decided is len(text).
//
// ran is whether to is where [valueEnd] ends the value, read on to its end,
// rather than where a wrapping's suffix cuts it: what a text that arrives in
// pieces reads on with where the value runs past its window ([Stream]).
type aValue struct {
	from, to, decided int
	unread, ran       bool
	// The value's SEAMS are where the stretches the read depends on and
	// the rule never replaces lie, which the keys' blocks are written apart
	// from ([claims.apart]), each 0 where there is none. gutter is where, on
	// the key's line, the gutter the line that decided the value opens with
	// ends ([gutterLen]); ending where, on a line below the key the value
	// was read on, what it ends in as the key's did begins
	// ([valueRead.ending]); and opening where the markup the key's line
	// opened and the read closed ends ([pastMarkup]).
	gutter, ending, opening int
	// also is a second range a reading of an ambiguous first piece names,
	// after [from, to), or zeros, and alsoSeams its own seams where the
	// reading that named it read them ([unionReading]); firstQuote whether
	// the read's key sat on a string's first piece after a quote.
	also       [2]int
	alsoSeams  [3]int
	firstQuote bool
	// runs are the stretches of a key's last run the read went past to a
	// value below ([valueRead.withRuns]), in order and apart, or none: the
	// keys write a block over each ([claims.keyRuns]).
	runs [][2]int
}

// seams is a password's value's seams, where the gutter of the key's line
// ends, where what the line the value was read on ends in begins, and where
// the markup the key's line opened ends, each or 0, then its second range's.
func (v aValue) seams() [6]int {
	return [6]int{v.gutter, v.ending, v.opening, v.alsoSeams[0], v.alsoSeams[1], v.alsoSeams[2]}
}

// valueRead is a [readValue] in progress.
type valueRead[T textual] struct {
	// text is what the read decides by: the text it was handed, ended
	// [MaxReach] past the key's start. whole is all of it, in which a value's
	// own run is read ([valueEnd]).
	text, whole T
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
	suffix, closing, appending T
	// markupOnly is whether the suffix is the markup that closes the key's
	// line ([outermost]) and nothing else: the suffix of a line that ends in
	// it, and of no other, which is read as though nothing had followed the
	// key. closer is where that closing stands on the key's line.
	markupOnly bool
	closer     [2]int
	// gutters are what the key's line has before the key ([valueRead.gutter]),
	// read once each: from its piece's start and from its real line's; and
	// none whether that line has none, beginning past the bound
	// ([gutterFrom]); los where each was read from.
	gutters [3]T
	none    [3]bool
	read    [3]bool
	los     [3]int
	wrapper T
	pick    int
	// later is whether the wrapper is the string's first piece's, the key
	// being on a later piece of it ([valueRead.gutter]).
	later bool
	// gutterAt is where on the key's line the gutter of the line the read
	// is at ends, and endingAt where on the line the value was read on what
	// it ends in begins ([aValue]), each or 0. opening is where the markup
	// the key's line opened and the read closed begins and ends
	// ([pastMarkup]), or zeros.
	gutterAt, endingAt int
	opening            [2]int
	// cuts are where in each gutter a key's last run begins and ends
	// ([bodyRunAt]), -1 twice where it holds none, read once each; runs
	// the stretches of it the read has gone past ([valueRead.runOn]),
	// through what the first line that spelled it spells up to its end, and
	// runNext where the line after the last begins.
	cuts    [3][2]int
	cutRead [3]bool
	runs    [][2]int
	through T
	runNext int
}

// setSuffix makes s the read's suffix.
func (r *valueRead[T]) setSuffix(s T) {
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
func appendingOf[T textual](suffix T) T {
	if tIndexByte(suffix, '>') < 0 {
		// The common case, and a suffix can be as long as its line.
		return suffix[:0]
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
		return suffix[:0]
	}
	return T(b.String())
}

// closingOf is what the last line of a list or of a concatenation ends in where
// every line before it ended in suffix: suffix without the `,` or `+` that
// carries it on to the next line, or "" where it ends in neither.
//
// The last element of a list and the last string of a concatenation close
// with the quote alone: read by the suffix alone, `"Password:",` over
// `"hunter2x9"` took the key's quote for the value and published the value.
// And not any line closing a string after an escaped break: `text :=
// "Enter password:\n"` over `fmt.Fprintf(w, "done\n")` lost `fmt.Fprintf(w,`.
func closingOf[T textual](suffix T) T {
	s := tTrimRight(suffix, " \t")
	if len(s) == 0 || s[len(s)-1] != ',' && s[len(s)-1] != '+' {
		return s[:0]
	}
	return tTrimRight(s[:len(s)-1], " \t")
}

// restOfLine reads the rest of a real line an escaped break ended the key's
// line on, the line starting at pos: the wrapping's suffix when it opens with
// punctuation and holds no key of its own, and the read goes on past it; and
// otherwise a line like any other, the read going on from it. It returns where
// the read goes on, whether the break before that is an escaped one, and
// whether the rest opens with punctuation before a key of its own — which
// makes the punctuation the value, as it is before another key on the key's
// own line ([lineEndUnnamed]).
//
// A KEY ON THE REST NAMES IT WHETHER OR NOT THE STRING GOES ON past it to
// another escaped line, so whether the bound cuts it changes nothing: else a
// second pass, the value now its marker, read the rest otherwise than the first.
func (r *valueRead[T]) restOfLine(pos int) (int, bool, bool) {
	lineEnd, next := lineAt(r.text, pos)
	rest := trimLine(r.text[pos:lineEnd])
	word := rest
	if i := tIndexByte(rest, ' '); i >= 0 {
		word = rest[:i]
	}
	if len(rest) == 0 || isValue(word) && !redirected(rest) {
		return pos, true, false
	}
	if r.keyOnRest(pos) {
		return pos, true, true
	}
	if next >= 0 && r.text[lineEnd] != '\n' {
		// The string goes on to another escaped line: one of its lines.
		return pos, true, false
	}
	if _, at := r.gutterOf(rest, false); at > 0 && !wordless(rest) {
		// The rest spells the key's line's own gutter and holds a word: the
		// string's next line, written in the same wrapping. A rest that
		// matches only the string's quote, as a shell's append after its
		// closing quote does, spells none of it and is the suffix.
		return pos, true, false
	}
	r.setSuffix(rest)
	return next, false, false
}

// redirected reports whether rest, what a real line holds after an escaped
// break, is the string's closing quote and a shell's redirect or pipe glued to
// it (`">>k`, `"|tee -a k`): the wrapping's suffix, whose target is no value.
func redirected[T textual](rest T) bool {
	i := 0
	for i < len(rest) && (rest[i] == '"' || rest[i] == '\'') {
		i++
	}
	for i > 0 && i < len(rest) && rest[i] == ' ' {
		i++
	}
	return i > 0 && i < len(rest) && (rest[i] == '>' || rest[i] == '|')
}

// keyOnRest reports whether the rest of a real line an escaped break ended the
// key's line on, starting at pos, opens with punctuation before a key of its
// own ([valueRead.restOfLine]).
func (r *valueRead[T]) keyOnRest(pos int) bool {
	lineEnd, _ := lineAt(r.text, pos)
	rest := trimLine(r.text[pos:lineEnd])
	word := rest
	if i := tIndexByte(rest, ' '); i >= 0 {
		word = rest[:i]
	}
	if i := tIndex(rest, keyMarker); i >= 0 {
		// A key's block can hold line breaks: a key past its marker may
		// stand on another line of the text as written.
		rest = rest[:i]
	}
	return len(rest) != 0 && !isValue(word) && keyedIndex(passwordRule(), rest) >= 0
}

// escapedOnly reports whether the line break ending a line at lineEnd, the
// line after it starting at next ([lineAt]) in a text total bytes long, is an
// escaped one with no real one after it: the string's next line, on the same
// line of the text. An escaped break written at the end of a real line
// (`password:\n` and then the line ends) is the real one: read as the
// string's, the next real line was the rest of the key's, and its punctuation
// the suffix every line after had to end in, so the value two lines down under
// a diff's `+` was published. SO IS ONE THAT ENDS THE TEXT, which no line of
// the string follows: read as the string's, the text read otherwise than it
// does with a break after it ([Secrets]).
func escapedOnly(lineEnd, next, total int) bool { return next == lineEnd+2 && next < total }

// lines reads on from the line starting at pos, escaped being whether the break
// before it is an escaped one with no real one after it ([readValueAs]). It
// passes over blank lines, the wrapping's blank lines (gutter and suffix
// alone) and a concatenation's closing `" +`; the line it stops at opens with
// the wrapping's GUTTER ([gutterLen]), and the value is the first word past it
// and the line's own markup ([ownMarkup]). A line holding a password's key
// where the value would be ENDS THE READ with no value, the key naming what is
// there (`password:` over `  password: hunter2`); under a suffix a key anywhere
// on the line does, and so does a marker, an armour or a key's marker where
// the value would begin. Where the value is, is DECIDED [MaxReach] PAST THE
// KEY'S START AT MOST ([valueRead.unread]); a row under a markup-only key
// closes as the key's did only where it holds no marker of a value.
func (r *valueRead[T]) lines(pos int, escaped bool) aValue {
	// crossed is whether the read has crossed a real line break: whether the
	// line it reads is another line of the wrapping, rather than the next
	// piece of the key's own string ([valueRead.gutter]).
	crossed := !escaped
	for pos >= 0 && pos < len(r.text) {
		lineEnd, next := lineAt(r.text, pos)
		line := r.text[pos:lineEnd]
		trimmed := trimLine(line)
		key := keyedIndex(passwordRule(), line)
		suffixed := len(r.suffix) != 0
		if suffixed && r.markupOnly && len(trimmed) != 0 {
			// A row a pass has replaced a value on closes as the key's
			// did only where nothing the marker stands for could have
			// carried its closing past the bound ([readValue]).
			_, suffixed = r.ending(trimmed, lineEnd, next)
			suffixed = suffixed && !tContains(line, passwordMarker)
		}
		switch {
		case len(trimmed) == 0:
		case escaped && wordless(trimmed) && (next < 0 || !escapedOnly(lineEnd, next, len(r.whole))):
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
				ended, ok := r.ending(trimmed, lineEnd, next)
				if !ok && r.reached(lineEnd, next) && r.taken(content, crossed) {
					// READ TO THE BOUND, with a marker where the value
					// would begin: the key's value was taken by a pass
					// before, whose marker, longer than the value,
					// carried the line's suffix past the bound. Read as
					// though the text ended at the bound, the line is no
					// wrapping's and the key's own punctuation its value,
					// so a second pass took that too.
					return r.decided(r.end, r.end, pos, false)
				}
				if !ok {
					return r.decided(r.end, r.end, pos, true)
				}
				content = ended
			}
			gutterEnd, g, k := r.gutterIn(content, crossed)
			rest := tTrimLeft(content[gutterEnd:], " \t")
			blank := len(tTrimSpace(rest)) == 0
			// The longest the read has spelled: a blank line of the
			// wrapping is one only by its whole gutter.
			if g > 0 {
				r.gutterAt = max(r.gutterAt, r.keyLineAt(k, g))
			}
			r.runOn(content, at, gutterEnd, g, k, rest, next)
			from := at + len(content) - len(rest)
			to, ran := from, false
			if !blank {
				keyAt := func(w int) bool { return key >= 0 && pos+key < w }
				to, ran = valueEnd(r.whole, ownMarkup(r.text, from, at+len(content), keyAt)), true
			}
			if suffixed && to >= at+len(content) {
				to, ran = at+len(content), false
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
			if suffixed {
				r.endingAt = at + len(content)
			}
			// A marker or an armour where the value would begin ends it
			// there ([valueEnd]), and the value is none.
			v := r.decided(from, to, pos, false)
			v.ran = ran
			if len(r.runs) > 0 {
				v = r.withRuns(v, next)
			}
			return v
		}
		escaped = escapedOnly(lineEnd, next, len(r.whole))
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
// UP TO THE END OF THE REAL LINE: punctuation a further escaped break follows
// is the string's next line, and may be the value, which a second pass would
// otherwise read elsewhere.
func (r *valueRead[T]) ending(trimmed T, lineEnd, next int) (T, bool) {
	ends, cut, endAt := trimmed, true, lineRest(r.text, lineEnd, next, len(r.whole))
	if endAt != lineEnd {
		ends, cut = trimLine(r.text[next:endAt]), false
	}
	if r.cutAt(endAt) {
		// What the line ends in runs past the bound, so it ends in no
		// suffix ([readValue]), even where its bytes up to the bound
		// spell one. Read as ending in the `" >> f` the bound cut it
		// after, a line took its value a line long for the key's, and
		// a second pass, that value a marker and the line whole within
		// the bound, took the key's own quote.
		return trimmed[:0], false
	}
	for i, s := range [...]T{r.suffix, r.closing, r.appending, unquoted(r.suffix), unquoted(r.closing), unquoted(r.appending)} {
		if len(s) == 0 || !tHasSuffixOf(ends, s) {
			continue
		}
		content := trimmed
		if cut {
			content = trimmed[:len(trimmed)-len(s)]
		}
		if i >= 3 && holdsKeysWord(content) {
			// Spelled without its quote, the suffix is a key's row's
			// too: a run of a private key's body there is the key's,
			// never this key's value, which a block reads on past.
			continue
		}
		return tTrimRight(content, " \t"), true
	}
	return trimmed[:0], false
}

// unquoted is suffix without the quote it opens with, where a redirect or a
// pipe follows the quote (`" >> k`, `\" |`, `" | tee -a k`), or "": the quote
// closes the key's own string, and a line of the same wrapping need not quote
// its content (`echo hunter2x9 >> k` under `echo "Password:\n" >> k`). Read
// by the whole suffix alone, such a line was none of the wrapping's, the quote
// was taken for the value and the value on it shown. Words after the quote
// are no wrapping (a key's run and its END), and are matched whole.
func unquoted[T textual](suffix T) T {
	for _, q := range [...]string{`\"`, `\'`, `"`, `'`} {
		if rest := tTrimLeft(suffix[min(len(q), len(suffix)):], " "); tHasPrefix(suffix, q) && len(rest) > 0 && (rest[0] == '>' || rest[0] == '|') {
			return suffix[len(q):]
		}
	}
	return suffix[:0]
}

// lineRest is where what the line ending at lineEnd ends in ends, the next
// line starting at next ([lineAt]) in a text total bytes long: the end of the
// real line an escaped break ends it before, where what follows the break up
// to there is punctuation alone that no further escaped break follows
// ([valueRead.ending]), and lineEnd otherwise.
func lineRest[T textual](s T, lineEnd, next, total int) int {
	if escapedOnly(lineEnd, next, total) {
		end, after := lineAt(s, next)
		if rest := trimLine(s[next:end]); len(rest) != 0 && wordless(rest) && (after < 0 || !escapedOnly(end, after, total)) {
			return end
		}
	}
	return lineEnd
}

// reached reports whether reading where the line ending at lineEnd ends, the
// next starting at next ([valueRead.ending]), ran to [MaxReach] past the key
// with the text going on past it, and what lies past the bound could have
// changed where [valueRead.ending] says the line ends: the line goes on past
// the bound, or the line after an escaped break that ends it does and what was
// read of that holds no letter and no digit, so it could still turn out to be
// the punctuation the line ends in.
//
// NOT WHERE WHAT WAS READ OF THE LINE AFTER THE BREAK HOLDS A WORD: such a line
// is none the line ends in however it goes on, and the bound decided nothing;
// read as cut, it named no value, where a second pass, the value's marker now
// ending the line inside the bound, took the key's quote.
func (r *valueRead[T]) reached(lineEnd, next int) bool {
	switch {
	case r.cutAt(lineEnd):
		return true
	case !escapedOnly(lineEnd, next, len(r.whole)):
		return false
	}
	end, _ := lineAt(r.text, next)
	return r.cutAt(end) && wordless(r.text[next:end])
}

// cutAt reports whether the bound cut a line read to end: it runs to the end of
// what the read decides by, the text goes on past that, and no line feed there
// ends the line whole at the bound. That byte is read past the bound, as a
// BEGIN's and an END's lines are read ([readKey], [linesBack]), so a line whose
// last byte is the last within it is one that ends within it.
func (r *valueRead[T]) cutAt(end int) bool {
	return end == len(r.text) && end < len(r.whole) && r.whole[end] != '\n'
}

// taken reports whether a marker stands where the value of a line read below
// the key would begin, content being the line, trimmed: past the wrapping's
// gutter ([gutterLen]) and the spaces after it.
func (r *valueRead[T]) taken(content T, crossed bool) bool {
	return tHasPrefix(tTrimLeft(content[r.gutterEnd(content, crossed):], " \t"), Marker)
}

// gutterEnd is where the gutter a line read below the key opens with ends, the
// line's content being content: past the key's line's gutter ([gutterLen]),
// or, where that line has none ([gutterFrom]), past the markers the line
// opens with but a password's ([leadingMarkers]).
func (r *valueRead[T]) gutterEnd(content T, crossed bool) int {
	n, _ := r.gutterOf(content, crossed)
	return n
}

// gutterOf is [valueRead.gutterEnd], and where on the key's line the gutter
// the line spells ends, in the text, or 0 where it spells none of it: a key's
// line with no gutter has none to write a key's block apart from
// ([claims.apart]), and its block is written whole.
//
// AS WRITTEN, the end [keyPrefix] reads is where it stands in the text: the
// line as the rule leaves it is the text byte for byte up to its first
// password's marker, since [writeValue] writes a value's escaped breaks
// before its first marker as they stand, and a gutter ends at a password's
// marker ([gutterLen]), so no gutter runs into a value written as its marker.
func (r *valueRead[T]) gutterOf(content T, crossed bool) (int, int) {
	n, at, k := r.gutterIn(content, crossed)
	if at <= 0 {
		return n, 0
	}
	return n, r.keyLineAt(k, at)
}

// gutterIn is [valueRead.gutterOf], the end on the key's line being where in
// the read's gutter k ([valueRead.gutter]) it stands, -1 where that line has
// none.
func (r *valueRead[T]) gutterIn(content T, crossed bool) (n, at, k int) {
	g := r.gutter(crossed)
	k = gutterIndex(crossed)
	if !crossed && len(r.wrapper) != 0 && (r.pick == 2 || r.pick == 0 && tHasPrefixOf(tTrimLeft(content, " \t"), r.wrapper)) {
		g, k = r.gutters[2], 2
	}
	if r.none[k] {
		return leadingMarkers(content), -1, k
	}
	n, at = gutterLen(content, g)
	return n, at, k
}

// keyLineAt is where i, a place in the read's gutter k, stands in the text.
func (r *valueRead[T]) keyLineAt(k, i int) int {
	return r.los[k] + leadingSpace(r.text[r.los[k]:r.start]) + i
}

// runOn notes the stretch of the key's last run ([bodyRunAt]) that content, a
// line below the key beginning at at in the text, the next at next, spells in
// its gutter, which ends at n and at g in the read's gutter k, rest following
// it: from where the run begins to where the gutter ends, the spaces before
// that left out, and the run on the key's line. A line that goes on as the
// key's line does past the run, its armour, spells none.
func (r *valueRead[T]) runOn(content T, at, n, g, k int, rest T, next int) {
	if g <= 0 {
		return
	}
	prefix := r.gutters[k]
	if !r.cutRead[k] {
		s, e := bodyRunAt(prefix)
		r.cuts[k], r.cutRead[k] = [2]int{s, e}, true
	}
	cut := r.cuts[k]
	if cut[0] < 0 || g <= cut[0] || g > cut[1]+1 || armourAt(rest, 0) {
		return
	}
	from, _ := gutterLen(content, prefix[:cut[0]])
	i, p := from, cut[0]
	for p < g && i < n {
		_, i = gutterShapeAt(content, i)
		_, p = gutterShapeAt(prefix, p)
	}
	if len(r.runs) == 0 {
		r.through = content[:i]
	}
	for i > from && isSpace(content[i-1]) {
		i--
	}
	r.runs = append(r.runs, [2]int{r.keyLineAt(k, cut[0]), r.keyLineAt(k, cut[1])}, [2]int{at + from, at + i})
	r.runNext = next
}

// withRuns is v, a value read below its key or none read there, with the key's
// last run the read went past ([valueRead.runOn]): A KEY'S LAST RUN IS NO
// GUTTER, but the keys' to read ([claims.keyRuns]), its gutter seam moved to
// where the run begins on the key's line, so that line's block takes it. A
// wrapping writes its gutter on every line, so where the line after the
// value's, or the last that spelled the run, beginning at next (-1 where none
// does), opens with what the first spelled up to the run's end as a word of
// its own ([wraps]), it is the wrapping's. v is decided no sooner than where
// the line after that one begins, or len(text) while it is open: asked of the
// line after, never the one above the key's, which [Settled] cuts off.
func (r *valueRead[T]) withRuns(v aValue, next int) aValue {
	line, after := r.text[:0], len(r.text)
	if next >= 0 {
		e, n := lineAt(r.text, next)
		line = r.text[next:e]
		if n >= 0 {
			after = n
		}
	}
	v.decided = max(v.decided, after)
	if wraps(line, r.through) {
		return v
	}
	v.runs, v.gutter = mergeSpans(r.runs), r.runs[0][0]
	return v
}

// bodyRunAt is where in prefix, a key's line before its key, a key's last body
// run begins and ends, or -1 twice: a word that is a key's run ([keysRun])
// glued to the first armour on the line, a key's END, or one space before it,
// beginning at a letter or a digit, a diff's `+` before it being the
// wrapping's ([valueRead.withRuns]).
func bodyRunAt[T textual](prefix T) (int, int) {
	a := armourIndex(prefix)
	if a <= 0 || !reMatch(keyEndAt(), prefix[a:]) {
		return -1, -1
	}
	end := a
	if prefix[end-1] == ' ' {
		end--
	}
	start := end
	for start > 0 && inBase64(prefix[start-1]) {
		start--
	}
	for start < end && !isAlnum(rune(prefix[start])) {
		start++
	}
	if start == end || start > 0 && tIndexByte(" \t\"'`|", prefix[start-1]) < 0 || !keysRun(prefix[start:end]) {
		// No word of its own, as a timestamp's `123Z` is, or no key's run.
		return -1, -1
	}
	return start, end
}

// wraps reports whether line opens with gutter, as a word of its own where
// gutter ends in a letter or a digit: a wrapping writing it on every line
// ([valueRead.withRuns]).
func wraps[T textual](line, gutter T) bool {
	line = tTrimLeft(line, " \t")
	return len(gutter) != 0 && tHasPrefixOf(line, gutter) &&
		(len(line) == len(gutter) || !isAlnum(rune(gutter[len(gutter)-1])) || !inBase64(line[len(gutter)]))
}

// armourIndex is where s holds its first armour, or -1.
func armourIndex[T textual](s T) int {
	for i := tIndex(s, "-----"); i >= 0; {
		if armourAt(s, i) {
			return i
		}
		j := tIndex(s[i+1:], "-----")
		if j < 0 {
			return -1
		}
		i += 1 + j
	}
	return -1
}

// gutterIndex is which of a read's gutters a line takes ([valueRead.gutter]).
func gutterIndex(crossed bool) int {
	if crossed {
		return 1
	}
	return 0
}

// leadingMarkers is where the markers content opens with end, and the spaces
// between them, a password's marker excepted, which ends them: 0 where it
// opens with none.
func leadingMarkers[T textual](content T) int {
	n := 0
	for i := 0; ; {
		for i < len(content) && isSpace(content[i]) {
			i++
		}
		m := markerLen(content[i:])
		if m == 0 || tHasPrefix(content[i:], passwordMarker) {
			return n
		}
		i += m
		n = i
	}
}

// gutter is what the key's line has before the key ([keyPrefix]) that a line
// the read stops at opens with: from the start of the key's real line where
// the read has crossed a real line break to it, whose gutter is the
// wrapping's, and from the start of the key's piece of its string, after the
// last escaped break, or the opening quote where markup closes the key's
// piece ([stringOpening]), where the read goes on in the same string. Read from the piece alone, the key in
// `"user: bob\npassword:\n"` had nothing before it, so on the next real line
// under a reader's line numbers the number was the value, and under a
// concatenation's lines the `"` that opens the string. A line or a piece that
// begins past [MaxReach] has none ([gutterFrom]).
func (r *valueRead[T]) gutter(crossed bool) T {
	k := gutterIndex(crossed)
	if !r.read[k] {
		lo := segmentStart(r.text, r.start)
		found := lo == 0 || r.text[lo-1] == '\n' || lo >= 2 && tEqual(r.text[lo-2:lo], `\n`)
		if !crossed && found && (lo == 0 || r.text[lo-1] == '\n') {
			if open := stringOpening(r.text, lo, r.start, r.prior); open > lo {
				r.gutters[2], r.none[2] = gutterFrom(r.text, lo, found, r.start, r.prior)
				r.los[2], r.read[2] = lo, true
				r.wrapper = tTrimLeft(r.text[lo:open], " \t")
				lo = open
			}
		} else if !crossed && found {
			// A later piece of a string: a line below that opens with what
			// the string's first piece has up to its quote is a line the
			// same wrapping wrote, read past it.
			rl := lineStartAbove(r.text, r.start)
			first := lo
			if i := tIndex(r.text[rl:lo], `\n`); i >= 0 {
				first = rl + i
			}
			if open := stringOpening(r.text, rl, first, r.prior); open > rl {
				r.wrapper = tTrimLeft(r.text[rl:open], " \t")
				r.gutters[2], r.none[2] = r.wrapper, false
				r.los[2], r.read[2], r.later = rl, true, true
			}
		}
		if crossed {
			lo = lineStartAbove(r.text, r.start)
			found = lo == 0 || r.text[lo-1] == '\n'
		}
		r.gutters[k], r.none[k] = gutterFrom(r.text, lo, found, r.start, r.prior)
		r.los[k] = lo
		r.read[k] = true
	}
	return r.gutters[k]
}

// stringOpening is where the string a key at start is written in begins on the
// real line that starts at lo: past the last quote before start that no
// backslash escapes and no value prior holds, or lo where there is none
// ([valueRead.gutter]). Read from the line's start alone, the rows below a key
// in a string spelled none of its gutter, and a row's opening was taken for the
// value. A later piece begins inside its string, so its wrapping is what the
// string's first piece has up to this opening, start being that piece's end.
func stringOpening[T textual](text T, lo, start int, prior [][2]int) int {
	for i := start - 1; i >= lo; i-- {
		if text[i] == '"' && (i == lo || text[i-1] != '\\') && !inSpan(prior, i) {
			return i + 1
		}
	}
	return lo
}

// gutterFrom is what a key's line has before the key at start: [keyPrefix] from
// lo, the line's start the read back found (else the bound, [MaxReach]), where
// that is within the bound as written and as the rule leaves it; else nothing.
//
// A LINE THAT BEGINS PAST THE BOUND HAS NO GUTTER, since a second pass reads
// the line as the rule leaves it: read from the bound, the gutter moved with
// each marker longer or shorter than its value (`echo "hunter2x9" >> f`), and
// no read within the bound says what precedes a longer value. The value is the
// first word below past the line's leading markers but a password's
// ([leadingMarkers]); made a password's marker, which ends every gutter
// ([gutterLen]), it gives a second pass no value there. A word gutter is taken
// for it, the value published; punctuation is redacted with it.
func gutterFrom[T textual](text T, lo int, found bool, start int, prior [][2]int) (T, bool) {
	if !found {
		return text[:0], true
	}
	if g, n := keyPrefix(text, lo, start, prior); n <= MaxReach {
		return g, false
	}
	return text[:0], true
}

// keyPrefix is text from lo to start — what a key's line has before the key at
// start — AS THE RULE LEAVES IT, every value it replaced there before the key
// (prior) written as the marker it becomes, and with its leading space
// trimmed; and how long it is so written, its leading space included
// ([gutterFrom]). Its trailing space stays: whether the key's line has a space
// between its gutter and its key is what says where a word of it ends
// ([gutterLen]).
//
// As the rule leaves it, because a second pass reads it so: read as written,
// `old_password="hunter2", new_password=` over `"hunter3x9"` read the quote as
// the gutter, and a second pass, a marker there, took the quote for the value.
func keyPrefix[T textual](text T, lo, start int, prior [][2]int) (T, int) {
	i, _ := slices.BinarySearchFunc(prior, lo, func(v [2]int, at int) int {
		if v[1] <= at {
			return -1
		}
		return 1
	})
	if i == len(prior) || prior[i][0] >= start {
		return tTrimLeft(text[lo:start], " \t"), start - lo
	}
	var b strings.Builder
	at := lo
	for ; i < len(prior) && prior[i][0] < start; i++ {
		from, to := max(prior[i][0], lo), min(prior[i][1], start)
		b.WriteString(tString(text[at:from]))
		writeValue(&b, passwordMarker, text[from:to])
		at = to
	}
	b.WriteString(tString(text[at:start]))
	return T(tTrimLeft(b.String(), " \t")), b.Len()
}

// lineStartAbove is where the real line holding i starts, looking back no
// further than [MaxKeyBlockBytes], as [segmentStart] does.
func lineStartAbove[T textual](s T, i int) int {
	floor := max(0, i-MaxKeyBlockBytes)
	return floor + tLastIndexByte(s[floor:i], '\n') + 1
}

// decided is a read's value — from and to, or the unread one
// ([valueRead.unread]) where unread is true — decided by the line beginning at
// pos ([aValue]).
func (r *valueRead[T]) decided(from, to, pos int, unread bool) aValue {
	unreadFrom, unreadTo := r.unread()
	if unread {
		from, to = unreadFrom, unreadTo
	}
	v := aValue{from: from, to: to, decided: pos,
		unread: from == to && unreadFrom == unreadTo || from == unreadFrom && to == unreadTo,
		ran:    unread && unreadTo > unreadFrom, gutter: r.gutterAt, ending: r.endingAt, opening: r.opening[1]}
	if unread && len(r.runs) > 0 {
		// No line below names a value, and the lines that spelled the run
		// are blank lines of the wrapping.
		v = r.withRuns(v, r.runNext)
	}
	return v
}

// unread is the value where no line after the key's holds one: the suffix's
// first word, which is a value where nothing repeats it, or none.
func (r *valueRead[T]) unread() (int, int) {
	if len(r.suffix) == 0 {
		return r.end, r.end
	}
	return r.from, valueEnd(r.whole, r.from)
}

// isValue reports whether word, the first after a password's separator, is a
// value rather than a wrapping's punctuation: it holds a letter or a digit, in
// any script, or opens with an empty quoted string. In any script, because a
// password's characters are a person's: read in ASCII alone, `Пароль` was
// punctuation, a line below that ended as its own did was its wrapping's,
// and the value was published.
func isValue[T textual](word T) bool {
	return tHasPrefix(word, `""`) || tHasPrefix(word, "''") || !wordless(word)
}

// wordless reports whether s holds no letter and no digit, in any script: the
// punctuation a wrapping writes around a line's content. ASCII is read a byte
// at a time, because what is asked of is often a line's whole rest, and every
// key on such a line asks.
func wordless[T textual](s T) bool {
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if isAlnum(rune(c)) {
				return false
			}
			i++
			continue
		}
		r, size := tDecodeRune(s[i:])
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
func lineEndUnnamed[T textual](text T, from int) (int, bool) {
	for i := from; ; {
		k := keyedBegins(passwordRule(), text, i)
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
		case keyedMatchesAt(passwordRule(), text, k):
			return k, true
		}
		i = beyond(text, k)
	}
}

// gutterLen is how much of content, a trimmed line that may hold a password's
// value, is the wrapping's gutter, and at where in prefix, the key's line
// before its key ([valueRead.gutter]), the stretch it spells ends: the longest
// start of the line SHAPED LIKE a start of prefix ([gutterShapeAt]), since a
// wrapping moves its numbers on and the key's line may have words the gutter
// lacks (`Enter the password:`), that ENDS WHERE A WORD OF BOTH DOES, since a
// value's start can be shaped like the key's line (`10:00:02Xk9…` under
// `10:00:01 password:`); a line the gutter would be all of, ending in a letter
// or digit, is a value, no blank line. A MARKER IS MATCHED WHOLE by the same
// marker or ends it, a `[REDACTED:password]` always, and so does an armour.
// THE QUOTE AFTER IT is the gutter's, a concatenation's opening on every line,
// but no other punctuation (`(9)Xk9…` under `(1) password:`).
func gutterLen[T textual](content, prefix T) (n, at int) {
	// n is the gutter's end so far, and at where prefix's own reading of it
	// ends; word is where it ended at the last space both lines have, before
	// the word of prefix the reading is in. An armour ends the gutter, since
	// none takes one: one ending in a dash took `-----END`'s first dash, and
	// the rest was read as the value and broke the armour with a marker.
	n, at, word := 0, 0, [2]int{0, 0}
	i, g := 0, 0
	for i < len(content) && g < len(prefix) && !armourAt(content, i) {
		if m := markerLen(content[i:]); m > 0 || markerLen(prefix[g:]) > 0 {
			// A marker is matched whole by the same marker, or ends it: a token
			// a wrapping writes on every line (a log's prefix holding one) is a
			// marker on each by then, so stopped at, every such line's value
			// was read as none and published, and matched by its shape, the key
			// line's `[REDACTED:api-key]` matched the start of the
			// `[REDACTED:password]` below and a second pass replaced its rest.
			// A password's marker always ends it: it is a value the key's line
			// held, never a wrapping's, and matched, a second pass read the
			// value's line as a blank one of the wrapping and read on past it.
			// A marker matched is a WORD OF BOTH lines: a key's run in a block
			// leaves the value glued to the marker under a key's line that
			// spaces it from its key ([valueRead.withRuns]).
			if m == 0 || tHasPrefix(content[i:], passwordMarker) || !tHasPrefixOf(prefix[g:], content[i:i+m]) {
				break
			}
			i, g = i+m, g+m
			n, at, word = i, g, [2]int{i, g}
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
	for k < len(content) && tIndexByte("\"'`\\", content[k]) >= 0 {
		k++
	}
	// The quote after it, never one inside a marker, which a key's line holds
	// only where a pass wrote one.
	if k > q && tContainsOf(unmarked(prefix[at:]), content[q:k]) {
		n = k
	}
	return n, at
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
func gutterShapeAt[T textual](p T, i int) (byte, int) {
	c, next := shapeAt(p, i)
	if c == '0' && next+1 < len(p) && p[next] == '.' && '0' <= p[next+1] && p[next+1] <= '9' {
		_, next = shapeAt(p, next+1)
	}
	return c, next
}

// unmarked is s with every marker in it removed.
func unmarked[T textual](s T) T {
	if !tContains(s, Marker) {
		return s
	}
	var b strings.Builder
	for {
		i := tIndex(s, Marker)
		if i < 0 {
			b.WriteString(tString(s))
			return T(b.String())
		}
		b.WriteString(tString(s[:i]))
		m := markerLen(s[i:])
		if m == 0 {
			return T(b.String())
		}
		s = s[i+m:]
	}
}

// markerLen is how long the marker s opens with is ([Marker]), or 0.
func markerLen[T textual](s T) int {
	if !tHasPrefix(s, Marker) {
		return 0
	}
	return tIndexByte(s, ']') + 1
}

// isSpace reports whether c is a space or a tab.
func isSpace(c byte) bool { return c == ' ' || c == '\t' }

// valueEnd is where a password's value that begins at at ends: at the next
// space, as the rule's `\S+` read it, ENDED EARLY at what no password holds and
// a key is read across ([rule.next]) — an armour, a marker — or a pass would
// replace its own marker again forever ([Contains]). An escaped line break ends
// it only where what follows, to the next space, holds no letter or digit (a
// closing quote, `" +`, `",`), the backslashes before it the string's; or where
// the word after it holds a password's key ([keyAfterBreak]). Elsewhere the
// value runs on across it and is replaced around it ([rule.write]), the text
// not saying which a break is. ONE READ OF THE VALUE, HOWEVER MANY BREAKS IT
// HOLDS: whether a letter or digit follows a break is read only as far as the
// first one ([wordAfter]).
func valueEnd[T textual](text T, at int) int {
	for i := at; i < len(text); i++ {
		if valueStop(text, i) {
			return valueBack(text, at, i)
		}
	}
	return len(text)
}

// valueStop reports whether a password's value read up to i ends at i
// ([valueEnd]): a space, an armour, a marker, or an escaped break the value
// does not run on across.
func valueStop[T textual](text T, i int) bool {
	switch c := text[i]; {
	case valueSpace(c):
		return true
	case c == '-' && armourAt(text, i), c == '[' && tHasPrefix(text[i:], Marker):
		return true
	case c == '\\':
		n := escapedBreakAt(text, i)
		return n > 0 && (!wordAfter(text, i+n) || keyAfterBreak(text, i+n))
	}
	return false
}

// valueBack is where a value that begins at at and stops at i ([valueStop])
// ends: at i, or before the backslashes written before an escaped break
// there, which are the string's ([valueEnd]).
func valueBack[T textual](text T, at, i int) int {
	if text[i] == '\\' {
		for i > at && text[i-1] == '\\' {
			i--
		}
	}
	return i
}

// escapedBreakAt is how long the escaped line break beginning at i is — `\n`,
// or `\r` and `\n` — or 0.
func escapedBreakAt[T textual](text T, i int) int {
	switch {
	case tHasPrefix(text[i:], `\n`):
		return 2
	case tHasPrefix(text[i:], `\r\n`):
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
//
// The line is read [MaxReach] past the break at most, as though the text
// ended there: a key further along it, its separator included, is none the
// value would run into, and the value runs on over it.
func keyAfterBreak[T textual](text T, i int) bool {
	limit := min(len(text), i+MaxReach)
	end, _ := breakWithin(text, i, limit)
	if end < 0 {
		end = limit
	}
	line := text[i:end]
	key := keyedIndex(passwordRule(), line)
	if key < 0 {
		return false
	}
	word := tIndexFunc(line, func(r rune) bool { return r < utf8.RuneSelf && valueSpace(byte(r)) })
	return word < 0 || key < word
}

// wordAfter reports whether text from i to the next space holds a letter or a
// digit, in any script, as [wordless] reads one ([isValue]).
//
// READ UP TO THE FIRST ONE, AND NO FURTHER: read to the space, a value in
// pieces with no space in it (`pwd=x\nx\nx…`) was read again at every break,
// quadratic; stopped there, a read never passes the next break's `n`, so the
// value is read once. AND [MaxReach] AT MOST where what was read holds neither
// a letter, a digit nor a space: read as a letter following, so the value runs
// on; read as the text's end, a letter past the bound would leave the piece
// after the break published. That costs redacting such punctuation.
func wordAfter[T textual](text T, i int) bool {
	limit := min(len(text), i+MaxReach)
	for i < limit && !valueSpace(text[i]) {
		if c := text[i]; c < utf8.RuneSelf {
			if isAlnum(rune(c)) {
				return true
			}
			i++
			continue
		}
		r, size := tDecodeRune(text[i:limit])
		if wordRune(r, size) {
			return true
		}
		i += size
	}
	return i == limit && limit < len(text)
}

// trimDebris is s without the debris of its line break — a real CR, an escaped
// one, the backslashes a string escaped twice leaves — as [trimLine] has it, and
// with its trailing spaces: a blank line of a wrapping that writes one after
// its gutter is that gutter alone only with it (`    32\t` under `nl`, a log's
// timestamp and a space).
func trimDebris[T textual](s T) T {
	for {
		t := tTrimSuffix(tTrimRight(s, "\r\\"), `\r`)
		if len(t) == len(s) {
			return s
		}
		s = t
	}
}

// valueHolds are the password keys in text whose value is decided on a line
// after the key's, or not yet: where each begins, and its value as
// [readValue] reads it. text is what the rule reads, the keys' blocks and the
// tokens already replaced ([passwordInput]); a value decided on the key's own
// line is no hold. The keys are read as the rule takes them ([rule.next]),
// each beside the values read before it.
//
// ONE READ OF THE WHOLE TEXT, AND NOT ONE OF EACH PREFIX A HOLD CUTS IT AT:
// asked per prefix, a chain of keys each decided by the next line held back
// the whole text and read it again for each line. read is added the bytes of
// text, for [settle] to report what [Settled] read: a count its caller keeps,
// not the package's, since tests call Settled side by side.
func valueHolds(text string, read *int) []valueHold {
	*read += len(text)
	var holds []valueHold
	var prior [][2]int
	for from := 0; ; {
		start, end, ok := passwordRule().next(text, from)
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
			if v.also[1] > v.also[0] {
				prior = append(prior, v.also)
				from = v.also[1]
			}
		}
	}
	if k := pendingKey().indexAtEnd(text); k >= 0 {
		holds = append(holds, valueHold{start: k, decided: len(text)})
	}
	return holds
}

// valueHold is a password key [valueHolds] reports: where it begins, where the
// line that decided its value begins, and whether a text cut before that line
// reads the same value ([aValue]).
type valueHold struct {
	start, decided int
	unread         bool
}

// passwordInput is text as the password's rule reads it in [Secrets]: its keys'
// blocks — spans, [keyBlocks] — and everything the rules before the
// password's replace by their markers, in their order: every token, and every
// AWS secret's value ([awsSecretKeys]), which holds no escaped line break and
// so is one marker, as a token is ([rule.write]).
//
// READ AS THE RULE READS IT, because a marker decides a read: a line a key's
// block was replaced on ends in the wrapping's suffix where the raw line did
// not (`[REDACTED:private-key]" >> key.pem`), and read off the raw text a live
// view replaced a `"` the whole text left as it was.
func passwordInput(text string, spans [][2]int) written {
	w := written{text: text}.replace(spans, keyMarker)
	for _, r := range rules() {
		if r.keyed == passwordRule() {
			break
		}
		if r.keyed == nil && !r.pattern.MatchString(w.text) {
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
// THE SHAPE RE2 CANNOT SKIP AHEAD ON: a case-insensitive alternation has no
// literal, so the machine stepped through every byte of every text
// ([BenchmarkSecrets]). So the words are found by a byte scan for the runes
// they open with, each matching its [unicode.SimpleFold] orbit as (?i) does
// (password's s matches ſ), and the pattern, whose words open a group, runs
// held to begin only there: the matches are its own, leftmost and
// non-overlapping, and a text with no word runs no pattern.
type keyed struct {
	words    []string
	firsts   string         // every rune a word opens with, folds included
	widest   int            // the most bytes a word takes, written in any case
	pattern  *regexp.Regexp // the pattern, as it reads anywhere in a text
	anchored *regexp.Regexp // the same pattern, held to begin where it is tried
	// refuses, where it is set, is whether a match the pattern finds at a
	// place is no match all the same — what a regular expression cannot say,
	// since it is about what stands before the match ([quotedElsewhere]).
	refuses *refusesFunc
}

// keyedOn is the pattern that opens with one of words, in any case, followed by
// what after matches: `(?i)(?:words…)(?:after)`.
//
// after is compiled on its own first, so one that is not a whole expression —
// an unbalanced `)|other(` that would close the words' group and let a match
// begin outside it — panics here rather than compiling into a pattern the
// scan does not describe. Each pattern is built at its first use, as the
// rules are ([rules]), and this package's tests build every one, so a bad word
// or a bad after fails them.
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
func (k *keyed) valued(with string, value *valueFunc) rule {
	r := k.rule(with)
	r.value = value
	return r
}

// begins is the first place at or after from where one of k's words opens, as
// (?i) reads it, or -1. from is where a rune starts: the start of text, the
// end of a match, or [beyond] a place already tried.
func (k *keyed) begins(text string, from int) int { return keyedBegins(k, text, from) }

func keyedBegins[T textual](k *keyed, text T, from int) int {
	return keyedBeginsBefore(k, text, from, len(text))
}

// keyedBeginsBefore is [keyedBegins] for a word opening before by alone: it
// scans no further.
func keyedBeginsBefore[T textual](k *keyed, text T, from, by int) int {
	for from < by {
		i := tIndexAny(text[from:by], k.firsts)
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
	return keyedNext(k, text, from)
}

func keyedNext[T textual](k *keyed, text T, from int) (start, end int, found bool) {
	for i := keyedBegins(k, text, from); i >= 0; i = keyedBegins(k, text, beyond(text, i)) {
		if loc := reFindIndex(k.anchored, text[i:]); loc != nil && !keyedRefused(k, text, i) {
			return i, i + loc[1], true
		}
	}
	return 0, 0, false
}

// matchesAt reports whether k's match begins at i in text, allocating nothing.
func (k *keyed) matchesAt(text string, i int) bool { return keyedMatchesAt(k, text, i) }

func keyedMatchesAt[T textual](k *keyed, text T, i int) bool {
	return reMatch(k.anchored, text[i:]) && !keyedRefused(k, text, i)
}

// keyedRefused reports whether k refuses a match the pattern finds at i.
func keyedRefused[T textual](k *keyed, text T, i int) bool {
	return k.refuses != nil && callRefuses(k.refuses, text, i)
}

// index is where k's leftmost match in text begins, or -1, allocating nothing.
func (k *keyed) index(text string) int { return keyedIndex(k, text) }

func keyedIndex[T textual](k *keyed, text T) int {
	for i := keyedBegins(k, text, 0); i >= 0; i = keyedBegins(k, text, beyond(text, i)) {
		if keyedMatchesAt(k, text, i) {
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
func beyond[T textual](text T, i int) int {
	_, size := tDecodeRune(text[i:])
	return i + size
}

// foldedPrefix reports whether text opens with word as (?i) reads it: rune for
// rune, each of text's in the [unicode.SimpleFold] orbit of word's.
func foldedPrefix[T textual](text T, word string) bool {
	for _, w := range word {
		if len(text) == 0 {
			return false
		}
		r, size := tDecodeRune(text)
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
	keyBegin = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`-----BEGIN (?:(?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY|PGP PRIVATE KEY BLOCK)-----`)
	})
	keyEnd = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`-----END (?:(?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY|PGP PRIVATE KEY BLOCK)-----`)
	})
)

// pemArmour is any PEM armour line — a private key's, or a certificate's, a
// public key's, any block's (RFC 7468's labels). A key's block never runs
// across one, in either direction: what follows it, or comes before it, is that
// block's, and a certificate's body is base64 at the same width as a key's. ITS
// LABEL IS 64 BYTES AT MOST: RFC 7468 sets no length, but the longest written
// run to 23 (OpenSSL's `NEW CERTIFICATE REQUEST`; `ENCRYPTED PRIVATE KEY` and
// `PGP PRIVATE KEY BLOCK` are 21), and an armour is read wherever a token, a
// value or a gutter passes dashes ([armourIn], [valueEnd], [gutterLen]), so an
// unbounded run of capitals held each read; a longer run is no armour. BUILT AT
// FIRST USE, as [rules] are.
var pemArmour = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`-----(?:BEGIN|END) [A-Z0-9][A-Z0-9 ]{0,63}-----`)
})

// Unarmoured is line, trimmed, cut at every PEM armour it holds ([pemArmour]):
// what of it is left, in order and each trimmed, empty pieces dropped — the
// whole line where it holds no armour.
//
// FOR A READER THAT TAKES TEXT OUT BY VALUE (a kept session's scrub of the
// secrets the engine handed a box): a key's armour is every key's of its kind,
// no secret of any one, and the lines this package reads a key's block by
// ([Secrets]). Taken out by value, every other key of that kind in the text
// lost the armour the redaction looks for, and its body went unredacted.
func Unarmoured(line string) []string {
	var out []string
	for _, piece := range pemArmour().Split(line, -1) {
		if piece = strings.TrimSpace(piece); piece != "" {
			out = append(out, piece)
		}
	}
	return out
}

// pemArmourAt is [pemArmour] held to begin where it is tried, built at first
// use for its reason.
var pemArmourAt = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^` + pemArmour().String())
})

// keyEndAt is [keyEnd] where a text begins ([bodyRunAt]).
var keyEndAt = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^` + keyEnd().String())
})

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
	return strings.Contains(s, keyMarker) || strings.Contains(s, "-----") && pemArmour().MatchString(s)
}

// marked reports whether s, beside an armour on its line, holds a marker: an
// ARMOUR LINE HOLDING ONE HOLDS NO KEY read across its lines, only one on it,
// read back from an END to the marker, whose words are no key's ([lineBack]).
//
// AND NO KEY'S BLOCK IS READ ACROSS A LINE HOLDING ONE ([keyReader.read],
// [wrapping.readBack], [backHold]): a marker can stand for far more text than
// its own, so a second pass would reach armours the first could not, and one
// beside an armour rewrites the prefix a second pass reads lines under (cut at
// the marker, a shorter prefix leaves fewer of the wrapping's lines blank, more
// a body's): redacting twice would not be redacting once. It costs a key with a
// line quoting a marker between its lines and one armour in the text, its far
// side published; an unredacted text holds a marker only to quote.
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
// A key's block is BOUNDED BY ITS STRUCTURE, not by this ([keyBlocks]): this
// only says how long a run of key-shaped lines can go on. RSA-16384's body is
// about 12.5 KiB, so 64 KiB holds any key in every wrapping more than twice
// over; past a key it bounds a base64 dump beside an armour, read as a key and
// held back from a live view ([Settled]) no further than this.
const MaxKeyBlockBytes = 64 << 10

// MaxReach is the furthest a rule reads from where it decides: forward from a
// match's or value's start, or back from a password's key. It bounds each read
// its format does not ([runTokenWithin], [jwtWithin], [sshCertificateWithin],
// [awsSecretValue], [separatorWithin], [awaiting], [readValue], [wordAfter],
// [keyAfterBreak], [pastMarkup], [quotedElsewhere], [linesBack]); around it a
// read takes the fixed bytes its format spells, 85 at most (a run token's 21
// before its subject; an expiry of nineteen digits and the rest, 64 after). At
// its bound a read decides as though the text ended or began there, but as the
// package doc's "How far a rule reads" lists; what a rule replaces runs as far
// as it goes. [MaxKeyBlockBytes] BY DEFINITION, no second literal to drift: the
// key blocks read that far either side of every armour, so a reader holding the
// key blocks' reach holds every rule's, and every format is far inside it.
const MaxReach = MaxKeyBlockBytes

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

// Secrets replaces known credential patterns with markers. A BREAK AFTER THE
// TEXT'S LAST LINE CHANGES NO REDACTION: Secrets(t+"\n") is Secrets(t)+"\n",
// t not ending in "\r", which the "\n" after it would join into one break.
// REDACTING TWICE IS REDACTING ONCE, since a transcript can pass more than one
// layer: what the rules write leaves a second pass nothing new to read
// ([rule.next], [keyMarker], [marked], [lineBack], [claims.shortOfNames],
// [claims.glued]), but the last case the package doc lists as published.
// THE TEXT IS READ WHOLE at any length: its caller holds it, and clean text
// costs a scan per rule and no copy. [Stream]'s stages run this code over
// windows, which here would cost 4.2 to 4.4 times the time and 17 MiB of
// buffers; its tests hold the two alike over every shape a stage carries,
// each window's edge at each byte around the shapes whose reads run furthest.
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
	for _, r := range rules() {
		text = r.apply(text)
	}
	return text
}

// OwnWords is text's stretches outside every marker this pass writes, each
// with the byte offset it begins at, in order: what a redacted text shows of
// its own words. A marker is [Marker], the name of what it replaced — a kind's
// name, in lowercase letters and dashes, as every rule spells one — and the
// bracket closing it; a "[REDACTED:" that does not go on so is the text's own
// words, and stays in its stretch.
//
// WHAT A SEARCH OF A REDACTED TEXT READS, never the text whole: a marker's
// words — `redacted`, `password`, `key`, `token` — are this pass's, so a word
// only a marker spells is never a hit that confirms, to whoever guessed a
// password that was that word (`passwd: password`), what the pass took out.
func OwnWords(text string) iter.Seq2[int, string] {
	return func(yield func(int, string) bool) {
		from, at := 0, 0
		for {
			i := strings.Index(text[at:], Marker)
			if i < 0 {
				break
			}
			start := at + i
			n := writtenMarkerLen(text[start:])
			if n == 0 {
				at = start + len(Marker)
				continue
			}
			if start > from && !yield(from, text[from:start]) {
				return
			}
			from, at = start+n, start+n
		}
		if from < len(text) {
			yield(from, text[from:])
		}
	}
}

// MarkersAround is the run of markers this pass wrote that the byte offset at
// of text falls strictly inside — where the run begins and where it ends — and
// whether there is one: an offset at a run's edge cuts no marker, and one in
// text's own words ([OwnWords]) is in none.
//
// WHERE A CUT MOVES TO, so a reader is never handed a marker in part: cut to
// "ACTED:github-token]", or to "[REDACTED:git", it reads as words of the
// text's own. A cut that keeps what comes before it moves back to where the
// run begins, and one that keeps what comes after on to where it ends.
func MarkersAround(text string, at int) (begin, end int, inside bool) {
	gap := 0
	for from, stretch := range OwnWords(text) {
		if at < from {
			return gap, from, at > gap
		}
		if at <= from+len(stretch) {
			return 0, 0, false
		}
		gap = from + len(stretch)
	}
	return gap, len(text), at > gap && at < len(text)
}

// writtenMarkerLen is how long the marker this pass writes that s opens with
// is — [Marker], a kind's name in lowercase letters and dashes, and the bracket
// closing it — or 0 where s opens with none.
func writtenMarkerLen(s string) int {
	if !strings.HasPrefix(s, Marker) {
		return 0
	}
	for i := len(Marker); i < len(s); i++ {
		switch c := s[i]; {
		case c == ']' && i > len(Marker):
			return i + 1
		case c >= 'a' && c <= 'z', c == '-':
		default:
			return 0
		}
	}
	return 0
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
	for _, r := range rules() {
		if r.in(text) {
			return true
		}
	}
	return false
}

// keySpans is every private key in text, as [start, end) byte ranges in order.
func keySpans(text string) [][2]int {
	spans, _, c := readKeys(text)
	if c == nil {
		return spans
	}
	return c.apart(spans)
}

// keyBlocks reads every private key's block in text: the spans to redact, in
// order, and where the last BEGIN starts if its block is still OPEN at the
// text's end (the next line could extend or close it), or -1. A KEY IS READ BY
// ITS STRUCTURE (RFC 1421, RFC 7468), FORWARD from its BEGIN ([readKey]) and
// BACK from an END no BEGIN's block reached ([readBack]). PROSE BEFORE ITS BODY
// ENDS A BLOCK, the whole defence against an armour with no key under it; in a
// body blank lines and ONE INTERRUPTING LINE are forgiven ([keyReader.take]).
// It allows for every [wrapping] and reads a CUT by a line's piece of the key,
// never the mark ([claims.piece] says what a cut publishes). Where three lines,
// two under a full line at no encoder's width, or two and two more round its
// short last line part a key ([backReading]), it publishes the part above, its
// BEGIN not in the text, and the part below with no full line, BEGIN or none.
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
	c := &claims{text: text}
	k := keyReading{open: -1}
	begins := keyBegin().FindAllStringIndex(text, -1)
	for _, b := range begins {
		k.begin(text, b, c)
	}
	c.take(k.forward)
	spans := append(slices.Clone(k.forward), k.ends(text, c, begins, keyEnd().FindAllStringIndex(text, -1), 0, len(text))...)
	return mergeSpans(append(c.shortOfNames(spans), c.keyRuns()...)), k.open, c
}

// keyReading is a reading of the keys' blocks in a text: every BEGIN's block
// read forward in order ([keyReading.begin]), and then every END's that no
// BEGIN's block reached read back ([keyReading.ends]), the claims taken
// between the two ([claims.take]).
//
// ONE ARMOUR AT A TIME, so a reader of a text that arrives in pieces
// ([Stream]) reads each armour's block with the same code as the whole text,
// in a window that holds all the block reads, and carries from one window to
// the next what a later armour's reading asks of the earlier ones: the blocks
// read forward, and the ENDs they reached.
type keyReading struct {
	// forward is every block read forward from a BEGIN, in order, and
	// closed where each END such a block reached begins, in order.
	forward [][2]int
	closed  []int
	// open is where the last BEGIN read begins when its block is still
	// open at the end of the text, or -1.
	open int
}

// begin reads the block of the BEGIN armour at b, forward, c being what the
// rules claim in text ([readKey]): none where it lies inside the block read
// before it, which it is part of.
func (k *keyReading) begin(text string, b []int, c *claims) {
	if len(k.forward) > 0 && b[0] < k.forward[len(k.forward)-1][1] {
		// Inside the block before it. Part of that key, not one of
		// its own.
		return
	}
	block := readKey(text, b, c)
	if !block.closed && block.end > 0 {
		if block.end = c.shortOfValues(b[1], block.end); block.end <= b[1] {
			// Nothing of a body before the key: no block.
			block.end = 0
		}
	}
	if block.end > 0 {
		k.forward = append(k.forward, [2]int{b[0], block.end})
	}
	if block.closed {
		k.closed = append(k.closed, block.endArmour)
	}
	k.open = -1
	if block.open {
		k.open = b[0]
	}
}

// ends reads back the blocks of the END armours in ends that begin in
// [from, to) and that no BEGIN's block reached ([readBack]), begins and ends
// being every key armour of each kind in text, in order. What it asks of the
// armours before each END is where the last of them ends, prev, which is as
// far back as readBack has to look for the start of the END's line.
func (k *keyReading) ends(text string, c *claims, begins, ends [][]int, from, to int) [][2]int {
	var spans [][2]int
	prev := 0
	for _, e := range ends {
		for len(begins) > 0 && begins[0][1] <= e[0] {
			prev, begins = max(prev, begins[0][1]), begins[1:]
		}
		if from <= e[0] && e[0] < to {
			for len(k.closed) > 0 && k.closed[0] < e[0] {
				k.closed = k.closed[1:]
			}
			if len(k.closed) == 0 || k.closed[0] != e[0] {
				if start, ok := readBack(text, e, prev, c); ok {
					spans = append(spans, [2]int{start, e[1]})
				}
			}
		}
		prev = e[1]
	}
	return spans
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
	// BEGIN, so each stops where the next BEGIN's starts: scanned to the
	// line's end or the bound, a line of many armours costs the armour count
	// times the line. So the armour is found first, and the break looked for
	// only before it.
	hi := min(len(text), b[1]+MaxKeyBlockBytes)
	stop, armour := hi, []int(nil)
	if a := pemArmour().FindStringIndex(text[b[1]:hi]); a != nil {
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
// THE FIRST ARMOUR DECIDES, so two keys flattened onto one line (`K1END K2`)
// each close at their own END: this key's END, only separators before it,
// CLOSES the block; any other armour, or this END after the enclosing text
// resumed, ENDS it at its body, as on a later line ([keyReader.read]). Giving
// up on the block instead published the whole body.
func oneLineKey(text string, b, armour []int, c *claims) keyBlock {
	between := text[b[1]:armour[0]]
	h := skipHeaders(between)
	body, end := runChunk(between[h:], b[1]+h, c)
	switch {
	case !body:
		return keyBlock{}
	case lastBase64(between[h+end:]) < 0 && keyEnd().MatchString(text[armour[0]:armour[1]]):
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
// whatever it ends in: read as the body's last line, a password's key (a
// `pwd=` whose value is on the next line) went into the block and left its
// value where the password's rule finds no key. Read as a line interrupting
// the key, both are redacted with the key when a line of it follows, and left
// to the password's rule when none does. It costs a key's last line ending in
// a password's key and `=` (one key in fifty thousand) with no END after it.
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
// than asked of [wrapping.prefixOK] before each run, which shaped the line
// again per run (quadratic): a run ends at what is no base64, so the shape up
// to a run's end is the shape before it and of what lies between, appended.
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
// of a key ([fragment]) where the wrapping puts a key's line: at the run its
// prefix ends at ([wrapping.headRun]), which a cut that kept a line's start
// leaves before its mark, or the run its suffix begins after
// ([wrapping.tailRun]), which one that kept its end leaves after it.
//
// AT THE LINE'S TWO ENDS AND NOWHERE ELSE ON IT: prose has runs that read like
// a piece (a test's name with a digit, a digest), and read anywhere, each such
// line beside a key was taken as one of its lines; a cut writes a key's line
// with a mark at one end or inside it. Prose still loses a line that opens or
// ends in such a run, `=== RUN   TestSHA256Sum`, which is what a cut leaves.
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
// ITS ENDS: the digit a piece is asked for keeps prose out ([fragment]), but a
// short head or tail often lacks one (an RSA key's `MIIEogIBAAKCAQEA`), and
// taken only to the piece it was published. A mark's own words (`truncated`,
// `812`) are in one case or none.
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
// LINE'S OWN PREFIX with such punctuation after it: a wrapping moves its
// numbers on (a grep's `file:N:` against `file-N-`), and the armour line's
// prefix may go on past the gutter every line shares, so a gutter need only
// begin it.
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
			if e := keyEnd().FindStringIndex(line); e != nil {
				// An END line holding a marker, before its armour or
				// after it, closes nothing, as one is read back from
				// nothing ([linesBack]).
				if !armoured(line[:e[0]]) && !marked(line) && r.closes(pos, line[:e[0]]) {
					return keyBlock{end: pos + e[1], closed: true, endArmour: pos + e[0]}
				}
				return r.ended()
			}
		}
		if armoured(line) || marked(line) {
			// Another armour, or a line a pass has redacted in, which
			// the read never crosses ([marked]).
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
// the block goes on past it: a full line of the body, its short last line, or a
// line interrupting it. The block ends with no body yet, or where two lines not
// the key's would stand between two that are; a short line is the last for as
// long as it can be. A LINE A CUT WROTE INTO IS A FULL LINE
// ([wrapping.cutInto]), running to [wrapping.cutEnd], since its own last run
// (`MHcCAQEE…49[...truncated]`) is the mark's. A SHORT PIECE OF A KEY
// ([claims.piece]) IS A FULL LINE after the BEGIN (the body's first line cut
// short), with text after it, or above another such piece: read as the last,
// the rest was published. It costs the word under a BEGIN whose one body line
// is such a piece, and prose after a key with no END whose lines each read like
// such a piece, taken as the key's.
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
		// END, follows, since what another process writes mid-key is no
		// reason to publish the rest of it.
		r.sinceFull++
		r.interrupted = true
		return true
	default:
		return false
	}
	// A short line the enclosing text follows (the `"}` closing the string a
	// key was written into, a cut's `…`) is the body's last: the block ends at
	// its base64. What follows a FULL line on its line ends nothing: only the
	// last line is short, so what a tool wrote after a full one, the mark of a
	// viewer that cut every line at its width, does not say the key is over.
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
// redact begins, and whether there is a key before the END at all. It reads
// [readKey]'s structure from the other end, and crosses TWO lines not the
// key's to a full line at an encoder's width, a BRIDGE ([backReading]); else
// the END's line alone ([lineBack]). THE END'S OWN LINE IS READ AS ITS PREFIX
// FIRST: the other reading adds only a body of one line written up to its
// armour. A blank line of the wrapping interrupts here ([backHold]), so a key
// double-spaced and broken by one line is broken by three. A FULL LINE ALSO
// READS LIKE A KEY'S ([keyLike]): both cases, no digit asked, so a live view
// shows hex at once and holds mixed-case test names. prev bounds the look
// back for the line's start, as [readKey]'s scans are bounded.
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
	// count times the line. And it is looked for [MaxReach] past the armour at
	// most, one byte more so that a break at the bound ends the line, as a
	// BEGIN line's rest is read ([readKey]): a line that goes on past that has
	// no suffix a line above it ends in, since none of those runs that far, and
	// what was read of it is all that is asked whether it is marked.
	restEnd, _ := breakWithin(text, e[1], min(len(text), e[1]+MaxReach+1))
	overlong := false
	if restEnd < 0 {
		restEnd = min(len(text), e[1]+MaxReach)
		overlong = restEnd < len(text)
	}
	suffix := trimLine(text[e[1]:restEnd])
	if marked(suffix) || suffix != "" && inBase64(suffix[0]) {
		// Redacted around, or going on past the armour in base64, as
		// [readKey] reads a BEGIN line that does: no wrapping's suffix.
		return 0, false
	}
	if overlong {
		suffix = ""
	}
	gutter := strings.Trim(pre, " \t")
	return wrapping{shape: prefixShape(gutter), suffix: suffix, claims: c}.readBack(text, lineS, e[0])
}

// lineBack is where a key's body written on one line ends at the end of s, the
// text before an END armour on that line, which starts at at in the text,
// begins — read back from the armour as [runChunk] reads one forward:
// OpenPGP's checksum, the body's short last run, its runs at the encoder's
// width, and one more run that is a piece of a key ([claims.piece]) — or -1;
// with no run at the encoder's width, the piece the END follows is a body a
// cut kept. A key on one line is the line a tool cuts, so it reads across ONE
// stretch that is neither, a word or a cut's mark, to the key's runs before
// it, as [pastCut] reads forward ([beforeCut]), and no further, so two cuts
// are read only to the second; never across an armour or a marker, so a
// second pass reads what the first did.
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
		if armoured(line) || marked(line) {
			// Another armour, or a line a pass has redacted in, which
			// the read never crosses ([marked]).
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

// readLine is what a trimmed line starting at at reads back as, and where the
// span begins when a reading reaches it: its run of base64 ([readRun]), or, on
// a line a cut wrote into ([wrapping.cutInto]), a full line from where the
// key's text on it begins ([wrapping.cutStart]), at its head's encoder's width,
// else any ([asAnyWidth]).
//
// A LINE A CUT WROTE INTO IS A FULL LINE WHICHEVER END OF IT THE CUT KEPT
// (`[… 812 bytes truncated]q3O4Ou…`), and the readings go on above it to the
// key's line before, whole or cut alike: read as the first, a key a viewer cut
// on every line was read one line back. AT ANY WIDTH where its head is at none,
// as the cut took the width. It costs prose above a key read from inside whose
// lines open or end in a run like a piece (`=== RUN   TestSHA256Sum`).
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
// for each: a SET, since what a line is can turn on lines not read yet. Run by
// [readBack] and [backHold] alike, so a live view's hold reaches as far back.
//
// BEFORE ANY FULL LINE a reading is the TAIL read so far: the short last line
// (either of two) and at most one line not the key's either side, seven tails.
// AFTER A FULL LINE, whether the last line interrupted the key. ACROSS TWO
// LINES not the key's, a BRIDGE to a FULL LINE AT AN ENCODER'S WIDTH
// ([atWidth]), so a key read from inside keeps what lies above; never three
// lines in a row, never another width, which a mixed-case name all but never
// is. OPENPGP'S CHECKSUM LINE IS READ AS THE END IS ([asChecksum]).
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
	// reading starts again above: read only as a short line, it was the
	// body's last or a line interrupting the key, and spent the one a block
	// may carry, so one line another process wrote between it and the
	// body's last line ended every reading, and two below it cut a bridge
	// off from the body.
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
// odds below one in a trillion. A DIGIT IS NOT ASKED FOR: such a line lacks one
// about once in fifty thousand, but one a key's encoding fixes in part far more
// often (a last line of forty-odd characters, an EC key's second line). Asked
// for, such a line read back ([readBack]) as the one line interrupting the key
// a block may carry, which a double-spaced wrapping had already spent, so the
// read stopped there and published every line above it.
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

// fragment reports whether run, base64 on a line it shares with other text,
// reads like a PIECE OF A KEY — what a cut leaves of a key's line beside its
// mark: at least [minFragment] characters, both cases ([keyLike]) and a digit.
//
// THE PIECE IS WHAT IS READ, NEVER THE MARK: no two tools spell a mark alike,
// and by structure alone every line of prose beside a key read as one of its
// lines. A DIGIT, because what else reads like base64 on such a line has none
// (a word, a CamelCase identifier, a test's name, a path): both cases alone
// took a capitalised word, a capital past the first letter every test's name,
// and `+` or `/` (base64's, no word's) code's `name+"` and every path. It costs
// a piece lacking a digit, published, even straight after a BEGIN, where an RSA
// key's first sixteen (`MIIEowIBAAKCAQEA`) have none.
func fragment(run string) bool {
	// A diff's `+` before the run is no part of the piece.
	run = strings.TrimPrefix(run, "+")
	return len(run) >= minFragment && keyLike(run) && strings.ContainsAny(run, "0123456789")
}

// claims is what the rules REPLACE in a text a key is read in, and what NAMES
// what they replace: every token, a password's key and separator, and apart
// from them its value, as merged ranges in order, read the first time a run
// asks ([claims.load]). A key's body line holds nothing that names
// ([wrapping.bodyLine]), a key's piece nothing they replace ([claims.piece]).
//
// A RUN ANOTHER RULE CLAIMS IS NO PIECE OF A KEY, though one beside a cut reads
// like one (`s3Password: hunter2-Secret!`, `OPENAI_API_KEY=sk-abcdefghij_Ab12…`):
// a span ended inside it left the rest matching no rule. Any of the run
// claimed, since a value a pass replaced leaves its key alone. It costs a
// key's line holding a token's shape whole, read as interrupting it; a run a
// rule left of one it replaced some of is that run still ([claims.glued]).
type claims struct {
	text string
	// whole is what each rule replaces: a token, a password's key and
	// separator, and its value apart from them. named is what names it,
	// which is all a token is and a password's key and separator alone
	// ([claims.names]).
	whole, named [][2]int
	// tokens are the tokens' ranges, an AWS secret's value among them, and
	// passwords where each password's key begins, its separator ends, its
	// value begins and ends, and its seams ([aValue]: the gutter's end, the
	// ending's start and the opening's end, each or 0), and where each AWS
	// secret's key begins and its separator ends ([claimed]).
	tokens    [][2]int
	passwords [][12]int
	// runs are the stretches of a key's last run a password's value was
	// read past, in order and apart ([claims.keyRuns]).
	runs [][2]int
	// endings are where each value read on a line it ends as the key's did
	// has that ending begin, and where the value ends, in the order of the
	// endings ([claims.endingApart]).
	endings [][2]int
	// taken are the blocks read forward from a BEGIN ([claims.take]).
	taken [][2]int
	read  bool
	// footprint, where a stream's keys' stage asks for one, is the hull of
	// what its answers read of the claims ([claims.asked]); begins is the
	// hull of where its answers read which claims BEGIN, and nothing of what
	// they cover ([claims.askedBegins]).
	footprint, begins *[2]int
}

// asked widens the footprint, where one is asked for, by [start, end): the
// claims over it are what an answer read ([keyStage.handed]).
func (c *claims) asked(start, end int) {
	if f := c.footprint; f != nil {
		f[0], f[1] = min(f[0], start), max(f[1], end)
	}
}

// askedBegins widens begins, where one is asked for, by [start, end): an
// answer read which claims begin there, and where on their own lines, and
// nothing of where they end ([claims.apartIn]). A claim handed late that
// begins there is a miss; an end set late is not, since it moves no claim's
// beginning ([keyStage.ended]).
func (c *claims) askedBegins(start, end int) {
	if f := c.begins; f != nil {
		f[0], f[1] = min(f[0], start), max(f[1], end)
	}
}

// any reports whether the rules replace any of text[start:end], a run of
// base64, or it is what a rule left of a run it replaced some of
// ([claims.glued]).
func (c *claims) any(start, end int) bool {
	c.load()
	c.asked(start, end)
	return overlaps(c.whole, start, end) || c.glued(start, end)
}

// names reports whether a token or a password's key and separator takes any
// of text[start:end]: what a line of a key's body may not hold
// ([wrapping.bodyLine]), where a password's value may still be the body's
// short last line, redacted with it.
func (c *claims) names(start, end int) bool {
	c.load()
	c.asked(start, end)
	return overlaps(c.named, start, end)
}

// glued reports whether text[start:end], a run of base64, is written straight
// onto a marker a rule writes, either side of it, with nothing between them:
// what a rule left of a run it replaced some of, which a pass reads as that
// run, claimed as the whole of it was ([claims]).
//
// WHAT A RULE LEAVES OF A RUN IS THE RUN IT CLAIMED: a token's pattern can end
// inside a run (`sk-` stops at a `+`), and the rest, a run of its own once the
// marker took the token's place, was read by a second pass as a key's body
// (`sk-…Ab3+…== -----END RSA PRIVATE KEY-----`). Asked only for what a rule
// replaces ([claims.any]), since no block is read across a marker ([marked]).
// It costs a key's run written straight onto a marker a text quotes.
func (c *claims) glued(start, end int) bool {
	before := start > 0 && c.text[start-1] == ']'
	after := end < len(c.text) && c.text[end] == '['
	if !before && !after {
		return false
	}
	for _, m := range ruleMarkers() {
		if before && strings.HasSuffix(c.text[:start], m) || after && strings.HasPrefix(c.text[end:], m) {
			return true
		}
	}
	return false
}

// ruleMarkers is every marker a rule writes, each once ([claims.glued]): a
// key's is none, since what a key's block leaves beside its marker is no run
// it claimed.
var ruleMarkers = sync.OnceValue(func() []string {
	var markers []string
	for _, r := range rules() {
		if !slices.Contains(markers, r.with) {
			markers = append(markers, r.with)
		}
	}
	return markers
})

// load reads the claims the first time a run asks, since a text holding a
// key's armour seldom holds a token or a password.
func (c *claims) load() {
	if !c.read {
		c.tokens, c.passwords, c.runs = claimed(c.text)
		c.read = true
		c.build()
	}
}

// keyRuns is every stretch of a key's last run a password's value was read
// past ([aValue]), each a block of the key's: the key's line and the lines
// below it that spell the run then read alike, a marker where it stood, and
// the claims and the rule name one value ([valueRead.withRuns]).
func (c *claims) keyRuns() [][2]int {
	c.load()
	return c.runs
}

// take says which blocks the keys read forward from their BEGINs took, before
// any is read back from an END: A KEY A BLOCK TAKES NAMES NO VALUE PAST IT.
// The rules run over the text the keys leave, so a password whose key and
// separator a block takes is no password to them, and what its value runs on
// to past the block is text, which an END below reads back as the key's where
// it reads like one. Claimed as a value, it would stop that read and be shown,
// and a second pass, the key then inside the block's marker, would redact it.
func (c *claims) take(blocks [][2]int) {
	c.taken = blocks
	if c.read {
		c.build()
	}
}

// build makes whole and named from the tokens and the passwords: each
// password's key and separator, and its value apart from them, left out where
// the key is in a block the value runs past ([claims.take]).
func (c *claims) build() {
	// In the order of their keys, which [claims.apart] and
	// [claims.shortOfValues] search: an AWS secret's are claimed apart.
	slices.SortFunc(c.passwords, func(a, b [12]int) int { return cmp.Compare(a[0], b[0]) })
	whole := slices.Clone(c.tokens)
	named := slices.Clone(c.tokens)
	c.endings = c.endings[:0]
	for _, p := range c.passwords {
		if p[5] > 0 && p[3] > p[2] {
			c.endings = append(c.endings, [2]int{p[5], p[3]})
		}
		if p[10] > 0 && p[8] > p[7] {
			c.endings = append(c.endings, [2]int{p[10], p[8]})
		}
	}
	slices.SortFunc(c.endings, func(a, b [2]int) int { return cmp.Compare(a[0], b[0]) })
	for _, p := range c.passwords {
		named = append(named, [2]int{p[0], p[1]})
		whole = append(whole, [2]int{p[0], p[1]})
		if p[3] <= p[2] {
			continue
		}
		i, _ := slices.BinarySearchFunc(c.taken, p[0], func(b [2]int, at int) int {
			if b[1] <= at {
				return -1
			}
			return 1
		})
		if i < len(c.taken) && c.taken[i][0] <= p[0] && p[3] > c.taken[i][1] {
			continue
		}
		whole = append(whole, [2]int{p[2], p[3]})
		if p[8] > p[7] {
			whole = append(whole, [2]int{p[7], p[8]})
		}
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

// shortOfNames is spans, the end of each that falls inside what a rule's
// match names ([claims.names]) moved back to where that begins: A KEY'S BLOCK
// ENDS SHORT OF WHAT A RULE NAMES, so the rule finds it whole and replaces
// what it names.
//
// A key on one line takes its runs without asking what claims them
// ([uncutRuns]), so a token or a key written straight onto a key's run on a
// BEGIN's line began inside the block; the marker took its head, the rest
// matched no rule and was published. ONLY ITS END: a block begins at an
// armour or a run no rule claims ([claims.any]), and ends at a line's run,
// which names nothing ([wrapping.bodyLine]), or at an END.
func (c *claims) shortOfNames(spans [][2]int) [][2]int {
	if len(spans) == 0 {
		return spans
	}
	c.load()
	for i, s := range spans {
		n, ok := within(c.named, s[1])
		if !ok {
			c.asked(s[1], s[1]+1)
			continue
		}
		// From the range's start: a claim merging with it there moves the
		// start without covering the place asked of.
		c.asked(n[0], s[1]+1)
		spans[i][1] = n[0]
	}
	return spans
}

// apart is spans, in order and apart, each WRITTEN APART FROM THE SEAMS of the
// passwords whose values the claims read ([aValue]): from the ending of a
// value's line it begins on ([claims.endingApart]), and, the key outside
// every span, from the gutter of a key's line it holds and the line break
// before it, where it begins on a line above ([claims.gutterApart]), and from
// the markup that line opened ([claims.openingApart]).
func (c *claims) apart(spans [][2]int) [][2]int {
	return c.apartIn(spans, spans)
}

// apartIn is [claims.apart] of spans, every block read in the text so far
// being known, in order and apart: what a key inside one names nothing, and
// what a line holds before a block on it is a marker.
func (c *claims) apartIn(spans, known [][2]int) [][2]int {
	if len(spans) == 0 {
		return spans
	}
	c.load()
	var out [][2]int
	for _, s := range spans {
		s, out = c.endingApart(s, out)
		// The gutter a span may hold is on the line it ends on, before a
		// key after it there, which no further than the bound begins.
		lineEnd, _ := lineAt(c.text, s[1])
		limit := min(lineEnd, s[1]+MaxReach)
		c.askedBegins(s[1], limit+1)
		i, _ := slices.BinarySearchFunc(c.passwords, s[1], func(p [12]int, at int) int { return cmp.Compare(p[0], at) })
		for ; i < len(c.passwords) && c.passwords[i][0] <= limit; i++ {
			p := c.passwords[i]
			if inSpan(known, p[0]) {
				continue
			}
			s, out = c.pieceApart(s, p[0], cmp.Or(p[4], p[9]), out)
			if c.readsAValue(p) {
				s, out = c.gutterApart(s, p[4], known, out)
				s, out = c.openingApart(s, p[4], p[6], out)
			}
			// The second range's own seams, where a reading gave it some.
			if p[8] > p[7] {
				s, out = c.gutterApart(s, p[9], known, out)
				s, out = c.openingApart(s, p[9], p[11], out)
			}
		}
		if s[1] > s[0] {
			out = append(out, s)
		}
	}
	return out
}

// gutterApart is s, a span, written apart from the gutter of the line a
// password's key is on, which ends at gutter, and the line break before it
// ([aValue]), the piece before them appended to out.
func (c *claims) gutterApart(s [2]int, gutter int, known, out [][2]int) ([2]int, [][2]int) {
	if gutter <= 0 || gutter > len(c.text) {
		return s, out
	}
	seg := segmentStart(c.text, gutter)
	lineBreak := breakBefore(c.text, seg)
	if s[0] >= gutter || s[1] <= lineBreak {
		return s, out
	}
	if s[0] < lineBreak && c.opensAlike(s[0], seg, gutter, known) {
		// The line the block begins on opens, before it, with the gutter
		// the key's line opens with: written whole, the key's line opens
		// so still.
		return s, out
	}
	if s[0] < lineBreak {
		out = append(out, [2]int{s[0], lineBreak})
	}
	s[0] = max(s[0], gutter)
	return s, out
}

// pieceApart is s, a span, written apart from the escaped break before the
// piece of a string a password's key, at key, is on, where s runs across it
// from the string's first piece, the piece before appended to out. Joined to
// the first piece, the key's piece was read by the first piece's gutter, and
// its value against the wrong lines. The key's line keeps its own gutter: the
// claim's, ending at gutter, or the one the block's first piece opens with
// where the key's piece opens with it too. A block that begins on a later
// piece stays joined.
func (c *claims) pieceApart(s [2]int, key, gutter int, out [][2]int) ([2]int, [][2]int) {
	seg := segmentStart(c.text, key)
	lineBreak := breakBefore(c.text, seg)
	if seg-lineBreak != 2 || c.text[lineBreak] != '\\' || s[0] >= lineBreak || s[1] <= lineBreak {
		return s, out
	}
	if f := segmentStart(c.text, s[0]); f != 0 && c.text[f-1] != '\n' {
		// The block begins on a later piece: joined as main joins it.
		return s, out
	}
	out = append(out, [2]int{s[0], lineBreak})
	// The gutter the block's first piece has before it, where the key's
	// piece opens with it too, stays as written.
	f, g := segmentStart(c.text, s[0]), s[0]
	for g > f && strings.IndexByte("|>#* \t", c.text[g-1]) >= 0 {
		g--
	}
	opens := c.text[g:s[0]]
	s[0] = max(s[0], seg)
	switch {
	case gutter > s[0] && gutter <= key:
		s[0] = gutter
	case opens != "" && strings.HasPrefix(c.text[seg:], opens) && seg+len(opens) <= key:
		s[0] = seg + len(opens)
	}
	return s, out
}

// openingApart is s, a span, written apart from the markup the key of p, a
// password's claim, has before its label that the read closed, past the
// gutter ([aValue]): the markup and the spaces among it, and, where only
// spaces stand before it on the key's line, those and the line break before
// them, the piece before them appended to out.
func (c *claims) openingApart(s [2]int, gutter, end int, out [][2]int) ([2]int, [][2]int) {
	if end <= gutter || end > len(c.text) || s[0] >= end {
		return s, out
	}
	start := end
	for start > 0 && strings.IndexByte("\"'`*_| \t", c.text[start-1]) >= 0 {
		start--
	}
	if seg := segmentStart(c.text, start); s[0] < seg && seg == start {
		start = breakBefore(c.text, seg)
	}
	if s[1] <= start {
		return s, out
	}
	if s[0] < start {
		out = append(out, [2]int{s[0], start})
	}
	s[0] = end
	return s, out
}

// endingApart is s, a span, written apart from the ending of the line it
// begins on where a password's value was read on that line before it
// ([aValue]): what the line ends in as the key's did, from where it begins,
// and the line break after it, are left out of the span, which is cut in two
// around them, the piece before them appended to out. A span that holds the
// value leaves the rule no value to read there, and is not cut; one on one
// line holds no ending, and asks nothing.
func (c *claims) endingApart(s [2]int, out [][2]int) ([2]int, [][2]int) {
	if end, _ := lineAt(c.text, s[0]); s[1] <= end {
		return s, out
	}
	lineS := segmentStart(c.text, s[0])
	c.asked(lineS, s[0])
	i, _ := slices.BinarySearchFunc(c.endings, lineS, func(e [2]int, at int) int { return cmp.Compare(e[0], at) })
	for ; i < len(c.endings); i++ {
		e := c.endings[i]
		if e[0] > len(c.text) || segmentStart(c.text, e[0]) != lineS {
			break
		}
		lineEnd, next := lineAt(c.text, e[0])
		if _, after := lineAt(c.text, lineRest(c.text, lineEnd, next, len(c.text))); after >= 0 && s[0] >= e[1] && s[1] > after {
			if s[0] < e[0] {
				out = append(out, [2]int{s[0], e[0]})
			}
			s[0] = after
		}
	}
	return s, out
}

// readsAValue reports whether the claims read a value under the key of p, a
// password's claim, or its read stopped on a line holding the password's
// marker: a second pass reads there the value the first redacted, and cuts
// the block the first cut ([claims.apart]). A window places the start of a
// value it holds none of, and of a word that names none, one past its end
// ([keyStage.claimsIn]), where it has no line to read.
func (c *claims) readsAValue(p [12]int) bool {
	if p[3] > p[2] {
		return true
	}
	if p[2] > len(c.text) {
		return false
	}
	at := max(p[2], 0)
	end, _ := lineAt(c.text, at)
	c.asked(at, end)
	return strings.Contains(c.text[at:end], passwordMarker)
}

// opensAlike reports whether what the line holding start has before start,
// none of it in a block and none of it what a rule replaces, is shaped as the
// gutter of the key's line, from its segment's start seg to gutterEnd
// ([prefixShape]): a block that begins there and ends on the key's line
// leaves that line opening, as the rule reads it, as it did.
func (c *claims) opensAlike(start, seg, gutterEnd int, known [][2]int) bool {
	from := segmentStart(c.text, start)
	for _, k := range known {
		if from < k[1] && k[1] <= start {
			return false
		}
	}
	c.asked(from, start)
	if from < start && overlaps(c.whole, from, start) {
		return false
	}
	return prefixShape(strings.TrimLeft(c.text[from:start], " \t")) == prefixShape(strings.TrimLeft(c.text[seg:gutterEnd], " \t"))
}

// shortOfValues is end, the end of a block read forward from a BEGIN that ends
// at or before start that no END closes, moved back to before the first
// password's key in [start, end) whose value runs past end, and the blanks
// before that key: A BLOCK TAKES A PASSWORD'S KEY ONLY WITH ITS VALUE (the
// package doc's "A key's block beside a password").
func (c *claims) shortOfValues(start, end int) int {
	c.load()
	c.asked(start, end+1)
	i, _ := slices.BinarySearchFunc(c.passwords, start, func(p [12]int, at int) int { return cmp.Compare(p[0], at) })
	for ; i < len(c.passwords) && c.passwords[i][0] < end; i++ {
		p := c.passwords[i]
		if p[3] <= p[2] || p[2] < end {
			continue
		}
		// The block ends at the key's line's last run of base64 before the
		// key, which a block's line ends at, or before that line.
		seg := segmentStart(c.text, p[0])
		at := p[0]
		for at > seg && !inBase64(c.text[at-1]) {
			at--
		}
		if at > seg {
			return max(start, at)
		}
		return max(start, breakBefore(c.text, seg))
	}
	return end
}

// inSpan reports whether at is inside one of spans, in order and apart.
func inSpan(spans [][2]int, at int) bool {
	i, _ := slices.BinarySearchFunc(spans, at, func(s [2]int, at int) int {
		if s[1] <= at {
			return -1
		}
		return 1
	})
	return i < len(spans) && spans[i][0] <= at
}

// breakBefore is where the line break that ends at start begins, as
// [lineBefore] reads one, or start where none does.
func breakBefore(s string, start int) int {
	switch {
	case start == 0:
		return 0
	case s[start-1] == '\n' && start >= 4 && s[start-4:start] == "\\n\r\n":
		return start - 4
	case s[start-1] == '\n' && start >= 3 && s[start-3:start] == "\\n\n":
		return start - 3
	case s[start-1] == '\n':
		return start - 1
	case start >= 2 && s[start-2:start] == `\n`:
		return start - 2
	}
	return start
}

// within is the range of ranges, in order and apart, that at falls strictly
// inside, and whether there is one.
func within(ranges [][2]int, at int) ([2]int, bool) {
	i, _ := slices.BinarySearchFunc(ranges, at, func(r [2]int, at int) int {
		if r[1] <= at {
			return -1
		}
		return 1
	})
	if i < len(ranges) && ranges[i][0] < at {
		return ranges[i], true
	}
	return [2]int{}, false
}

// piece reports whether text[start:end], a run of base64, is a piece of a key
// ([fragment]) that no rule replaces any of. A cut read by its pieces
// ([keyBlocks]) publishes a piece between two marks, or too short to read like
// a key's, and a key's tail after a mark on its own line after its short last
// line, with no END below.
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
// — or -1. An END reads back across a prompt as across a blank line
// ([wrapping.readBack]), and a live view counts one no more ([backHold]): a
// block may take it whole, its value with it. ASKED OF THE KEY ALONE, since a
// pass replaces what comes before it (`hunter2x9  # rotate the password:`),
// never the key. A KEY THAT IS ANOTHER'S VALUE IS NO PROMPT
// (`ffAt0gun passwd:Pwd:`, [rule.next]), NOR IS A KEY A TOKEN TAKES
// (`sk-…Pwd:`): the line is asked as the rules before the password's leave it
// ([passwordInput]), or a second pass would read it otherwise than the first.
func promptAt(line string) int {
	at := passwordPrompt().indexAtEnd(line)
	if at < 0 {
		return -1
	}
	w := passwordInput(line, nil)
	if len(w.edits) > 0 {
		at = passwordPrompt().indexAtEnd(w.text)
	}
	var prior [][2]int
	for from := 0; at > 0; {
		start, end, ok := passwordRule().next(w.text, from)
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
// one, and every password: where its key begins, where its separator ends, and
// where the value it names begins and ends, the separator's end for both where
// it names none ([claims]).
//
// IN THE RULES' ORDER, and as they take them: passwords are read in the text
// the rules before them leave ([passwordInput]), so a key a token took is none
// and a value ends at a token in it, never running on across an escaped break
// into a key's piece (`pwd=\nsk-…\n…u1SU1Lf9x= -----END`); and
// a key inside a value replaced is none ([rule.next]); read on their own, a
// claim named what the rule did not replace. A key and separator, an AWS
// secret's too, are claimed whether or not a value follows yet.
func claimed(text string) (tokens [][2]int, passwords [][12]int, runs [][2]int) {
	w := passwordInput(text, nil)
	for _, e := range w.edits {
		tokens = append(tokens, [2]int{e[2], e[3]})
	}
	tokens = mergeSpans(tokens)
	// An AWS secret's key and separator are claimed among the passwords', as
	// what names a value: its value is claimed as a token's is, a marker in
	// the text the passwords are read in, but its key is no token's, and a key
	// run into its value (`SessionToken=` before its base64) is one run with
	// the value as written and another beside the value's marker, which a
	// second pass would read as a key's text where the first read a claimed
	// run.
	for _, r := range rules() {
		if r.keyed == passwordRule() {
			break
		}
		if r.value == nil {
			continue
		}
		for from := 0; ; {
			start, end, ok := r.find(w.text, from)
			if !ok {
				break
			}
			passwords, from = append(passwords, [12]int{w.at(start), w.at(end), w.at(end), w.at(end)}), end
		}
	}
	password := rules()[len(rules())-1]
	var prior [][2]int
	for from := 0; ; {
		key, v, ok := decideReadIn(password, w.text, from, prior)
		if !ok {
			break
		}
		value, seams, also := [2]int{v.from, v.to}, v.seams(), v.also
		for i, at := range seams {
			if at > 0 {
				seams[i] = w.at(at)
			}
		}
		if value[1] > value[0] {
			a, as := [2]int{}, [3]int{}
			if also[1] > also[0] {
				a, as = [2]int{w.at(also[0]), w.at(also[1])}, [3]int(seams[3:])
			}
			passwords = append(passwords, [12]int{w.at(key[0]), w.at(key[1]), w.at(value[0]), w.at(value[1]), seams[0], seams[1], seams[2], a[0], a[1], as[0], as[1], as[2]})
			for _, r := range v.runs {
				runs = append(runs, [2]int{w.at(r[0]), w.at(r[1])})
			}
			prior, from = append(prior, value), value[1]
			if also[1] > also[0] {
				prior, from = append(prior, also), also[1]
			}
			continue
		}
		// A key and separator no value follows yet are claimed all the same,
		// so what a run is does not change when the line after it is written.
		passwords, from = append(passwords, [12]int{w.at(key[0]), w.at(key[1]), w.at(key[1]), w.at(key[1]), seams[0], seams[1], seams[2]}), beyond(w.text, key[0])
	}
	return tokens, passwords, mergeSpans(runs)
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
// or -1. It runs the read back's readings ([backReading]) over everything each
// line could be under ANY wrapping, the END's not yet written: an interrupting
// or short last line always, a full line where a run at a body's width reads
// like a key's ([keyLike]), or one at any width where it holds a piece
// ([fragment]), so its reach is never shorter than the real read's. A held
// line is released by five lines in a row holding no such run (blank and glue
// lines count for none, a possible OpenPGP checksum restarts the count): two
// end every reading's tail and one more its interruption, and a bridge crosses
// two more; four do where no line at an encoder's width gives a bridge a
// landing.
func backHold(text string) int {
	r, hold := everyReading, -1
	for start := len(text); start > 0 && r != 0; {
		var end int
		start, end = lineBefore(text, start)
		if len(text)-start > MaxKeyBlockBytes {
			break
		}
		line := text[start:end]
		if armoured(line) || marked(line) {
			// What the read back stops at ([wrapping.readBack]).
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
func shapeAt[T textual](p T, i int) (byte, int) {
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
var oneLineHeader = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[^A-Za-z0-9+/=]*(?:Proc-Type:\s*4,ENCRYPTED|DEK-Info:\s*[A-Za-z0-9-]+,(?:[0-9A-Fa-f]{32}|[0-9A-Fa-f]{16}))`)
})

// skipHeaders is how much of s, a key's body written on one line, is its RFC
// 1421 headers: the one-line reader ([runChunk]) would take a header's first
// word for a word of prose, and the line for no key at all.
func skipHeaders(s string) int {
	at := 0
	for {
		loc := oneLineHeader().FindStringIndex(s[at:])
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
// A tool cuts a key on one line first (an environment dump, a flattened JSON
// value), keeping head and tail around its mark, so read only to the mark the
// far part was published. ONLY TO A RUN AT THE ENCODER'S WIDTH, since a piece
// alone is what a word with a digit reads like; and ONCE, so a key two cuts
// left on one line is read up to the second.
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
// start ([lineBack]), begins once it is read back across one cut, as [pastCut]
// reads one forward — or -1 where no run of the key at a full line's width lies
// across one, or none as wide as an encoder writes one when wide is true: when
// nothing between the cut and the END reads as the key's, since a path in mixed
// case is as wide as a full line
// (`…/certs/ServerPrivateKey.pem:28:-----END PRIVATE KEY-----`). It reads
// nothing before the last five dashes, which open every armour, nor at or
// before the last marker, which may be what replaced the armour that stopped
// the first pass, so a second pass reads what the first did ([Secrets]).
// [pastCut] needs no such stop: a BEGIN a second pass still reads a body after,
// the first read the same body after and redacted.
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
func segmentStart[T textual](s T, i int) int {
	return segmentStartAbove(s, i, max(0, i-MaxKeyBlockBytes))
}

// segmentStartAbove is [segmentStart] looking back no further than floor, and
// floor when no break lies between it and i.
func segmentStartAbove[T textual](s T, i, floor int) int {
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
func lineAt[T textual](s T, pos int) (int, int) {
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
func breakWithin[T textual](s T, pos, limit int) (int, int) {
	for i := pos; i < limit; i++ {
		switch {
		case s[i] == '\n':
			return i, i + 1
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == 'n':
			next := i + 2
			if tHasPrefix(s[next:], "\r\n") {
				next += 2
			} else if tHasPrefix(s[next:], "\n") {
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
func trimLine[T textual](line T) T {
	for {
		trimmed := tTrimRight(line, " \t\r\\")
		trimmed = tTrimSuffix(trimmed, `\r`)
		if len(trimmed) == len(line) {
			return tTrimLeft(line, " \t")
		}
		line = trimmed
	}
}

// leadingSpace is how many spaces and tabs s begins with.
func leadingSpace[T textual](s T) int { return len(s) - len(tTrimLeft(s, " \t")) }

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

// Settled is how much of text, a text still being written a complete line at
// a time, can be redacted and shown now: the length of its longest prefix
// ending on a line break whose redaction nothing written after it can change.
// THE PROMISE IS EXACT: redacting the text in pieces cut at each point it
// returns is redacting it whole, however the rest arrives. A line is held back
// by a key's block still OPEN ([keyBlocks]); by lines an END NOT WRITTEN YET
// could read back over ([backHold]), so a mixed-case name waits as a key's
// line; by a key whose block runs across the point; and by a password key
// whose value, as the rule ([valueHolds]) or the claims read it (unalike after
// `-----END RSA PRIVATE KEY-----|passwd:`), a later line decides, within
// [MaxReach]. Nothing past the last line break settles. READ IN ANY PIECES,
// IT ANSWERS WHAT IT ANSWERS WHOLE ([settling]), and never falls.
func Settled(text string) int {
	s := settling[string]{text: text}
	return s.next()
}

// settling is [Settled] of a text as it arrives: the text past the point
// settled so far, how much of it was looked at for a line break, and how
// many times it asked Settled's readers ([settle]). It asks only once a line
// break has arrived since it last asked, since Settled reads nothing past
// the last one, and only of the text from its point to there, which Settled
// chains at. It holds the line being written and the lines Settled holds
// back, which a run of key lines can make any length. Over a string, as
// Settled reads one, an ask copies nothing.
type settling[T textual] struct {
	text          T
	scanned, asks int
	// ask, where an in-package test sets it, answers in settle's place, so
	// the tests' HeldToTheLoop holds every ask a stream makes to a second
	// reading of its holds; nil, the ask is settle's. Held here, an ask
	// costs the loop alone: holding the refusing layouts at sampled cuts
	// instead read each layout's megabytes again, 545 s without race.
	ask func(string) int
}

// next is how much more of the text is settled, the text left as it is.
func (s *settling[T]) next() int {
	from := s.scanned
	s.scanned = len(s.text)
	i := tLastIndexByte(s.text[from:], '\n')
	if i < 0 {
		return 0
	}
	s.asks++
	text := string(s.text[:from+i+1])
	if s.ask != nil {
		return s.ask(text)
	}
	n, _ := settle(text)
	return n
}

// growing is a settling over bytes a stream holds: it takes the text as it
// is written, and hands on what settles.
type growing struct{ settling[[]byte] }

// write takes p as the next of the text.
func (s *growing) write(p []byte) { s.text = append(s.text, p...) }

// settlingKeeps is the most buffer a settling keeps once what it holds fits
// in it again: a line being written and the lines Settled holds back in an
// ordinary text, for a block's and a value's reach ([MaxKeyBlockBytes],
// [MaxReach]), with room for append to double. A buffer grown past it held a
// long line, or a longer stretch Settled held back, which a stream would
// otherwise keep the size of until it closed.
const settlingKeeps = 4 * MaxReach

// settle hands hand the text settled since it last answered, if any, and
// keeps the rest.
func (s *growing) settle(hand func([]byte)) {
	n := s.next()
	if n == 0 {
		return
	}
	hand(s.text[:n])
	if rest := s.text[n:]; cap(s.text) > settlingKeeps && len(rest) <= settlingKeeps {
		s.text = bytes.Clone(rest)
	} else {
		s.text = s.text[:copy(s.text, rest)]
	}
	s.scanned -= n
}

// settle is [Settled], and the bytes it handed its readers to read: the keys'
// ([readKeys]), the back hold's ([backHold]), the password's rule's
// ([passwordInput]) and the holds' ([valueHolds]), each over the text once,
// and the last two once more where the keys were read beside the values
// ([claims]). A reader called again for every line the text holds is the cost
// this counts; a rescan inside one reader is that reader's own test's. The
// test of one read judges the count, not a time: timed against [Secrets], the
// ratio moved with how busy the runner was, and no scheduler moves the bytes.
// The holds they read are then resolved by [heldLines.settled].
func settle(text string) (settled, read int) {
	h, read := holding(text)
	return h.settled(), read
}

// heldLines is what holds a text's complete lines back from settling: the
// keys' blocks, the one still open and the back hold ([readKeys],
// [backHold]), and the password keys whose value a later line decides
// ([valueHolds]); claims are those of the holds read where the keys were read
// beside the values ([claims]).
type heldLines struct {
	whole      string
	spans      [][2]int
	open, back int
	holds      []valueHold
	claims     []valueHold
}

// holding reads text's complete lines for what holds them back, and counts
// the bytes it handed its readers ([settle]).
func holding(text string) (h heldLines, read int) {
	end := strings.LastIndexByte(text, '\n') + 1
	h = heldLines{whole: text[:end], open: -1, back: -1}
	if end == 0 {
		return h, 0
	}
	reading := func(s string) string { read += len(s); return s }
	var c *claims
	h.spans, h.open, c = readKeys(reading(h.whole))
	h.back = backHold(reading(h.whole))
	written := h.spans
	if c != nil {
		written = c.apart(slices.Clone(h.spans))
	}
	input := passwordInput(reading(h.whole), written)
	h.holds = valueHolds(input.text, &read)
	for k, v := range h.holds {
		h.holds[k].start, h.holds[k].decided = input.at(v.start), input.at(v.decided)
	}
	// The keys were read beside the passwords' values as the text reads
	// with no block replaced ([claims]), and a value read so is decided
	// by its own lines too: a key whose claim a cut would decide otherwise
	// is held with it, since the text after the cut is read with no claim
	// on that value, and an END there reads it back as the key's.
	if c != nil && c.read {
		raw := passwordInput(reading(h.whole), nil)
		for _, v := range valueHolds(raw.text, &read) {
			v.start, v.decided = raw.at(v.start), raw.at(v.decided)
			h.holds = append(h.holds, v)
			h.claims = append(h.claims, v)
		}
	}
	return h, read
}

// bar is a hold as the line starts it keeps the settled point from: it stands
// over each q with lo < q <= hi, and moves a point there to lo's line. An
// unread hold's stands only where hi is at or past the rewritable point
// ([heldLines.sweep]).
type bar struct {
	lo, hi int
	unread bool
}

// settled is the largest line start no bar stands over, an unread hold's
// counted at the rewritable point of that line start (the package doc's
// "Settled in one pass"): a sweep of the keys' bars finds where that point
// begins, and a sweep of every bar, the claims' pairs lowering it, the line
// start.
func (h heldLines) settled() int {
	end := len(h.whole)
	// Room on the stack for the bars a live view's ask most often holds, at
	// a line end of what it has not shown: an open block, the back hold, a
	// block and a hold, so the ask allocates nothing for them, an allocation
	// being a few percent of reading a short text. More grow onto the heap.
	var room [4]bar
	bars := room[:0]
	if h.open >= 0 {
		bars = append(bars, bar{lo: h.open, hi: end})
	}
	if h.back >= 0 {
		bars = append(bars, bar{lo: h.back, hi: end})
	}
	for _, s := range h.spans {
		bars = append(bars, bar{lo: s[0], hi: s[1] - 1})
	}
	slices.SortFunc(bars, byHi)
	rw := h.sweep(bars, end, end, nil)
	for _, v := range h.holds {
		bars = append(bars, bar{lo: v.start, hi: v.decided, unread: v.unread})
	}
	slices.SortFunc(bars, byHi)
	return h.sweep(bars, end, rw, h.claimPairs())
}

// byHi orders bars by hi, highest first.
func byHi(a, b bar) int { return cmp.Compare(b.hi, a.hi) }

// sweep is the largest line start at or below q no bar stands over, bars in
// [byHi] order. rw is the rewritable point, where the lines the keys hold
// begin: what is written next may still read any of them as a key's, or as no
// longer one (a one-line key an END below reads back into a block, a block an
// END closes, a block a claim may yet cut apart), and a password's value
// decided on such a line may be decided again ([passwordInput]), so an unread
// hold decided there stands. Each of pairs lowers rw to its lo once q is at or
// below its hi.
func (h heldLines) sweep(bars []bar, q, rw int, pairs []bar) int {
	var waiting []bar // unread bars passed while rw stood above their hi
	next, woken := 0, 0
	lower := func() {
		for {
			for ; next < len(pairs) && pairs[next].hi >= q; next++ {
				rw = min(rw, pairs[next].lo)
			}
			if woken == len(waiting) || waiting[woken].hi < rw {
				return
			}
			if b := waiting[woken]; b.lo < q {
				q = lineStart(h.whole, b.lo)
			}
			woken++
		}
	}
	lower()
	for _, b := range bars {
		switch {
		case b.hi < q:
			return q
		case b.unread && b.hi < rw:
			waiting = append(waiting, b)
		case b.lo < q:
			q = lineStart(h.whole, b.lo)
			lower()
		}
	}
	return q
}

// claimPairs are the claims' holds each beside a block that ends on its key's
// line before the key, in [byHi] order: until the value's line, the cut apart
// from that line's gutter ([claims.apart]) is undecided, so the block's lines
// are rewritable from lo, the block's line, and a key line above whose value
// the rule read no further than the block may yet read the gutter as its own.
func (h heldLines) claimPairs() []bar {
	var pairs []bar
	for _, v := range h.claims {
		ls := lineStart(h.whole, v.start)
		// The spans are apart and in order, so their ends are too.
		i, _ := slices.BinarySearchFunc(h.spans, ls, func(s [2]int, ls int) int { return cmp.Compare(s[1], ls+1) })
		for ; i < len(h.spans) && h.spans[i][1] <= v.start; i++ {
			pairs = append(pairs, bar{lo: lineStart(h.whole, h.spans[i][0]), hi: v.decided})
		}
	}
	slices.SortFunc(pairs, byHi)
	return pairs
}

// lineStart is the start of the line holding byte i.
func lineStart(text string, i int) int {
	return strings.LastIndexByte(text[:i], '\n') + 1
}
