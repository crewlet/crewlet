package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE SCRUB A KEPT SESSION LEAVES ITS BOX THROUGH: every secret the engine put
// in the box taken out by value, then redact.Secrets by shape, each file
// rewritten as what it is (a transcript line by line, a document a token at a
// time, anything else as text) so its harness still restores it, and the tar's
// own words scrubbed too. No line, string or stretch of text is held whole or
// refused for its length; a tar and an export are each read twice, from a plan
// of bits; and the scrub is held to its bounds and to [scrubHeldBytes]. The
// argument is the package doc's: "A kept session leaves its box scrubbed",
// "The scrub holds no value whole" and "The scrub's bounds, and what it holds".

// archiveLayout is how a harness's archive is laid out.
type archiveLayout int

const (
	// archiveTar is a gzipped tar of files (Claude Code's).
	archiveTar archiveLayout = iota + 1
	// archiveDocument is one gzipped JSON document (OpenCode's export).
	archiveDocument
)

// scrubMarker is what a value the engine put in a box becomes in a kept
// session: redact's own marker, so a reader — and redact's second pass — knows
// it for one, and nothing a JSON string would have to escape.
const scrubMarker = redact.Marker + "engine-secret]"

// credentialMarker is what a string under a credential's own key becomes
// ([credentialKey]).
const credentialMarker = redact.Marker + "credential]"

// minScrubBytes is the shortest value taken out by value.
//
// EIGHT BYTES. A value shorter is as likely to be ordinary text in a transcript
// — a port, a version, a word — as a secret, and taking every occurrence of it
// out would corrupt the very transcript that is kept; nothing the engine hands
// a box from a credential position is that short, and redact's rules stay the
// backstop for one that is.
const minScrubBytes = 8

// scrubPlainMax is the most plaintext one reading of an archive yields to a
// scrub: a session the scrub reads further than that is refused, as
// [executorkeep.ErrSessionPlainTooLarge] where a take reads it ([Runner.TakeSession]).
//
// 256 MiB, apart from the archive's bound (executorkeep.SessionMaxBytes): it is
// what the take's wait (internal/sandbox's SessionScrubWait) is derived for,
// read once, and again from a unit it goes on from. A text-only session gzip shrinks further than the two bounds'
// ratio is refused here first; media, already compressed, inflates to about
// its archive's size, so it is held by the archive's bound and no ratio.
const scrubPlainMax = executorkeep.SessionPlainMaxBytes

// keptGzipLevel is the level the scrub writes a kept archive at: seven, the
// lowest of Go's compress/flate levels that entropy-codes base64. The take
// holds the box's archive and the seal the rewrite to one bound, and media is
// where the rewrite would grow: at one to six flate stores base64 of bytes
// already compressed (an image's, a PDF's) as it is, 0.996 of its size, where
// seven and the box's GNU gzip -6 reach 0.75. A Claude Code session holding a
// 20 MiB PDF read, a 40.36 MiB box archive, was kept as 53.10 MiB at six (the
// take admits it, the seal refuses it) and 40.13 MiB at seven, for about 0.4 s
// more a PDF read and 1.2 s more on 118 MiB of text (build VM, two cores).
const keptGzipLevel = 7

// newKeptGzip is the gzip writer a kept archive is written through.
func newKeptGzip(dst io.Writer) (*gzip.Writer, error) {
	zw, err := gzip.NewWriterLevel(dst, keptGzipLevel)
	if err != nil {
		return nil, fmt.Errorf("executor: opening the kept archive's gzip at level %d: %w", keptGzipLevel, err)
	}
	return zw, nil
}

// scrubPieceBaseBytes is the longest piece the scrub reads before it holds the
// large slot: 256 KiB, four times the census's p99 line (the package doc's "A
// collection's read of a thread"), so an ordinary line, string or file never
// waits. A piece is a text the scrub redacts as one, whose redaction may hold
// up to its length ([pieceGate.pieces]), and the plan is held past it too
// ([tarPlan.holdPast]); a walk's line costs only its facts
// ([collectionBaseLineCharge]).
const scrubPieceBaseBytes = 256 << 10

// errScrubBound is a session past what its scrub reads: refused.
var errScrubBound = errors.New("executor: the session is past the bounds a kept session is scrubbed within")

// scrubBounds is how much of an archive one scrub reads.
type scrubBounds struct {
	// plain is the most plaintext one reading of the archive yields.
	plain int64
	// gate is what the scrub holds while it holds a piece past
	// [scrubPieceBaseBytes] ([scrubBounds.gatedBy]); its hold is nil
	// where no slot gates the scrub's pieces.
	gate pieceGate
	// memo is the take's way into the process's memo
	// ([scrubBounds.remembering]); nil looks nothing up.
	memo *memoUse
	// spool is where the first reading spools what it writes
	// ([scrubBounds.spooling]).
	spool spoolPlace
	// prefix is the check of the box's digest of a chat file's run start,
	// fed the raw bytes of each member as the first reading reads it
	// ([scrubBounds.checking]); nil checks none.
	prefix *prefixCheck
	// tally is what the scrub took out of the members a meter counts
	// ([scrubBounds.tallying]); nil keeps none.
	tally *scrubTally
	// split is where a take that keeps its media apart writes its body
	// ([scrubBounds.splitting]); a nil body keeps none apart.
	split mediaSplit
}

// plainBounds is a scrub that reads at most plain bytes of plaintext in one
// reading.
func plainBounds(plain int64) scrubBounds {
	return scrubBounds{plain: plain}
}

// gatedBy is b with every piece past the base held under large's one slot
// while the scrub holds it, waited for within ctx: a take's scrub, one pass
// with its meter (the package doc's "A collection's read of a thread").
func (b scrubBounds) gatedBy(ctx context.Context, large *passBudget) scrubBounds {
	b.gate = large.gate(ctx)
	return b
}

// remembering is b looking its texts up in use's memo, and keeping there what
// it found unchanged.
func (b scrubBounds) remembering(use *memoUse) scrubBounds {
	b.memo = use
	return b
}

// checking is b feeding check the raw bytes of the archive's members as its
// first reading reads them, before a byte is rewritten.
func (b scrubBounds) checking(check *prefixCheck) scrubBounds {
	b.prefix = check
	return b
}

// pieces is src beneath a scrub's reader: a piece past the base holds g's slot
// before its bytes are handed on, until the reader above is done with it
// ([lineRun]), a piece each line where lines is set and else the whole of src;
// done drops what it still holds. A PIECE IS WHAT THE SCRUB REDACTS AS ONE
// TEXT, whose redaction may hold up to its length (the package doc's "The
// scrub's bounds, and what it holds").
func (g pieceGate) pieces(src io.Reader, lines bool) (r io.Reader, done func()) {
	if g.hold == nil {
		return src, func() {}
	}
	run := &lineRun{r: src, gate: g, whole: !lines}
	return run, run.done
}

// lineRun is a scrub's member reader that holds gate BEFORE it hands on a byte
// of a line past [scrubPieceBaseBytes], counting the bytes since the last
// break, and drops it at the next read once that line's break has passed:
// beneath the scrub's line reader, that read comes only once the reader above
// has read the line to its break and handed it on. Read-ahead may hold early,
// never late, and nothing it hands on is copied. A hold's error is its own;
// done drops what it still holds.
type lineRun struct {
	r    io.Reader
	gate pieceGate
	run  int
	// held is whether it holds the gate; open, whether the line it holds for
	// has not yet broken. whole says the piece is the whole reader, which no
	// break ends: a text the scrub redacts as one ([pieceGate.pieces]).
	held, open, whole bool
}

func (l *lineRun) Read(p []byte) (int, error) {
	if l.held && !l.open && l.gate.drop != nil {
		l.gate.drop()
		l.held = false
	}
	n, err := l.r.Read(p)
	if l.held && l.gate.drop == nil {
		return n, err
	}
	for rest := p[:n]; ; {
		brk := -1
		if !l.whole {
			brk = bytes.IndexByte(rest, '\n')
		}
		if brk < 0 {
			l.run += len(rest)
		} else {
			l.run += brk
		}
		if l.run > scrubPieceBaseBytes {
			if !l.held {
				if holdErr := l.gate.hold(); holdErr != nil {
					return 0, holdErr
				}
				l.held = true
			}
			l.open = true
			if l.gate.drop == nil {
				return n, err
			}
		}
		if brk < 0 {
			return n, err
		}
		l.open, l.run, rest = false, 0, rest[brk+1:]
	}
}

// done drops the gate where the run still holds it and drops at all.
func (l *lineRun) done() {
	if l.held && l.gate.drop != nil {
		l.gate.drop()
		l.held = false
	}
}

// The figures [scrubHeldBytes] adds up, each the most one part of the scrub
// holds.
const (
	// streamHeldBytes is one redact.Stream: its stages' windows, at most
	// about 21 MiB of text and 28 MiB of what they keep beside it, as
	// redact's package doc states and its own test holds (21,589 and 29,109
	// KiB), and for a moment the 484 KiB its keys' stage outgrows: 49.98 MiB.
	streamHeldBytes = 50 << 20
	// scannerHeldBytes is one jsonscan.Scanner: its read buffer and its
	// struct, the open containers' bits among it, as jsonscan's package doc
	// states and its own test holds. The scrub never captures a value, so
	// it holds no capture's buffer beside it.
	scannerHeldBytes = 67_072
	// rewriteFrameBytes is the most one of a JSON rewrite's frames takes
	// (rewriteFrame), which a test holds it to.
	rewriteFrameBytes = 40
	// rewriteHeldBytes is one JSON rewrite: its scanner, the buffer it
	// reads a text's pieces into, and a frame for each container open, at
	// most jsonscan.MaxDepth.
	rewriteHeldBytes = scannerHeldBytes + textWholeBytes + jsonscan.MaxDepth*rewriteFrameBytes
	// archiveHeldBytes is the archive's own readers and writers, the gzip
	// writer's compressor the largest: flate's hash chains and window, under
	// a mebibyte at [keptGzipLevel], beside gzip's reader, tar's reader
	// and writer, the member split's frame ([memberOut]) and the buffers of
	// the readers and writers above them.
	archiveHeldBytes = 2 << 20
	// planBlockBytes is the room of the [bitList] blocks being filled beyond
	// their bits: each of the plan's two streams, the three lists a line's
	// cut reads at once, and the block a list's bits move into while the
	// list's last one goes ([bitList.moveTo]) each round up to one.
	planBlockBytes = 6 * bitBlockWords * 8
	// spoolHeldBytes is what the spool adds ([unitSpool]): the two ways a
	// unit is held in until its way is known ([candidate]), and the buffer
	// before the spool's gzip, the first reading's; the second's, reading
	// the spool back, are fewer. Its gzip's writer and reader are the
	// archive's ([archiveHeldBytes]): the box's reader and the spool's
	// writer in the first reading, the spool's reader and the kept
	// archive's writer in the second, each pair as large as the take's one.
	spoolHeldBytes = 2*wayRoomBytes + 64<<10
	// pieceHeldBytes is the first reading's stage of one text
	// ([firstPiece]): the chunk it holds, and under a KiB of SHA-256
	// states and the memo's answers beside it.
	pieceHeldBytes = scrubPieceBaseBytes + 1<<10
)

// scrubHeldBytes is the most the scrub of a reading of at most plain bytes
// holds, longest the longest value it takes out ([scrubber]): the sum of
// [scrubTerms].
//
// TWO OF EACH TEXT'S PARTS, because a first reading reads a line, or a
// `.json` file, from the first that is no JSON both ways at once
// ([measureLines], [measureFile]); the second reads one.
func scrubHeldBytes(plain int64, longest int) int64 {
	var sum int64
	for _, term := range scrubTerms {
		sum += term.figure(plain, longest)
	}
	return sum
}

// scrubTerm is one part of what a scrub holds: its name, by which a test of
// its own holds it to its figure, and that figure at a reading of plain bytes
// whose longest value is longest.
type scrubTerm struct {
	name   string
	figure func(plain int64, longest int) int64
}

// scrubTerms are the parts [scrubHeldBytes] adds up: two text scrubs, each a
// redaction's stream, its stage's chunk, a [valueStream] (fewer bytes than the
// longest value held, and beside them a copy of those joined to twice it read
// of the next piece) and a string's encoder
// (a rune's bytes and the most of a text the cut reads); two JSON rewrites; the
// archive's readers and writers; the spool's ([spoolHeldBytes]); and the plan,
// a sixteenth of the plaintext ([tarPlan]), beside its blocks.
var scrubTerms = []scrubTerm{
	{"streams", func(int64, int) int64 { return 2 * streamHeldBytes }},
	{"pieces", func(int64, int) int64 { return 2 * pieceHeldBytes }},
	{"values", func(_ int64, longest int) int64 { return 2 * 4 * int64(longest) }},
	{"encoders", func(int64, int) int64 { return 2 * (utf8.UTFMax + providerToolNameMax + 1) }},
	{"rewrites", func(int64, int) int64 { return 2 * rewriteHeldBytes }},
	{"archive", func(int64, int) int64 { return archiveHeldBytes }},
	{"spool", func(int64, int) int64 { return spoolHeldBytes }},
	{"plan", func(plain int64, _ int) int64 { return plain / 16 }},
	{"plan blocks", func(int64, int) int64 { return planBlockBytes }},
}

// scrubLongestMax is the longest value a take's scrub can hold ([scrubber]):
// one line of every byte a capture scrubs out (sandbox.MaxScrubBytes), in its
// JSON form, six bytes a byte where each is a control byte, `\u00XX`: no byte
// escapes longer ([jsonEscaped]; an invalid one writes U+FFFD, three).
const scrubLongestMax = 6 * sandbox.MaxScrubBytes

// scrubber takes the engine's own values out of text: a whole text through
// strings.Replacer ([scrubber.apply]), and a text that arrives in pieces
// through a [valueStream] of the same values ([scrubber.stream]).
//
// first, where it is set, stages each text the first reading of a take
// scrubs before its shape pass ([scrubber.firstReading]); nil, the shape pass
// alone.
type scrubber struct {
	r      *strings.Replacer
	values *valueTable
	first  *firstWay
	// tally says each text's scrub keeps tally of what it takes out
	// ([scrubber.tallying]).
	tally bool
	// media is the first reading's plan of the payloads it keeps apart,
	// which each string value's rewrite tells ([mediaRun]); nil finds none.
	media *mediaPlan
}

// firstReading is s staging its texts through pieces, on its text way first
// ([scrubber.way]).
func (s scrubber) firstReading(pieces *firstPieces) scrubber {
	s.first = &pieces.ways[textWay]
	return s
}

// finding is s telling media of each payload its string values hold, nil
// telling none.
func (s scrubber) finding(media *mediaPlan) scrubber {
	s.media = media
	return s
}

// way is s on way of its first reading, and s itself in any other.
func (s scrubber) way(way int) scrubber {
	if s.first != nil {
		s.first = &s.first.r.ways[way]
	}
	return s
}

// newScrubber is the scrubber for values: each line of each value at least
// [minScrubBytes] long — a multi-line value (a key file a setup step wrote) is
// taken out a line at a time, which is how a transcript holds it, and a PEM
// armour line left where it is (redact.Unarmoured) — in its raw form and as a
// JSON string escapes it, the longest first, so a value holding another is
// taken out whole.
func newScrubber(values []string) scrubber {
	seen := map[string]bool{}
	var olds []string
	add := func(v string) {
		if len(v) >= minScrubBytes && !seen[v] {
			seen[v] = true
			olds = append(olds, v)
		}
	}
	for _, value := range values {
		for _, line := range strings.Split(value, "\n") {
			// A KEY'S ARMOUR IS NO SECRET OF IT: every key of its kind
			// carries the same BEGIN and END lines, and the shape pass reads
			// a key's block by them. Taken out by value, every other key of
			// that kind in the session — one the run printed, a fixture in the
			// repository — lost the armour the shape pass looks for, and its
			// body was kept. The rest of the line is taken out as any value is.
			for _, piece := range redact.Unarmoured(line) {
				add(piece)
				add(jsonEscaped(piece))
			}
		}
	}
	if len(olds) == 0 {
		return scrubber{}
	}
	slices.SortStableFunc(olds, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(olds))
	for _, old := range olds {
		pairs = append(pairs, old, scrubMarker)
	}
	return scrubber{r: strings.NewReplacer(pairs...), values: newValueTable(olds)}
}

// apply is s with every value taken out.
func (s scrubber) apply(text string) string {
	if s.r == nil {
		return text
	}
	return s.r.Replace(text)
}

// valueStream is the by-value pass over a text that arrives in pieces, cut
// anywhere: what it writes to w, put together, is [scrubber.apply] of the
// whole text, however the text was cut, once Close has written the last of it.
// It decides a place at a time as the replacer does, a value taken out
// leftmost and longest first, and holds fewer bytes than the longest value
// from one piece to the next. The package doc's "The scrub holds no value
// whole" says why.
type valueStream struct {
	w      io.Writer
	values *valueTable
	// held is the text past the last place decided, fewer bytes than the
	// longest value.
	held []byte
	// joined is held and the start of the next piece, read together.
	joined []byte
	// err is the first error of w, which ends the stream.
	err error
}

// valueTable is the values a [scrubber] takes out, as a [valueStream] reads
// them: by their first byte, longest first, and the longest one's length.
type valueTable struct {
	byFirst [256][][]byte
	longest int
}

// newValueTable is the table of olds, ordered longest first.
func newValueTable(olds []string) *valueTable {
	t := &valueTable{}
	for _, old := range olds {
		t.byFirst[old[0]] = append(t.byFirst[old[0]], []byte(old))
		t.longest = max(t.longest, len(old))
	}
	return t
}

// stream is s's by-value pass onto w, for a text written to it a piece at a
// time.
func (s scrubber) stream(w io.Writer) *valueStream {
	return &valueStream{w: w, values: s.values}
}

// Write reads the next piece of the text, and writes what of the text it has
// decided.
func (v *valueStream) Write(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	if v.values == nil {
		return v.result(len(p), v.literal(p))
	}
	n := len(p)
	if len(v.held) > 0 {
		// Every place in held is decided on held and the first twice the
		// longest value's length of p: a value read from a place in held
		// ends within the longest value's length past it.
		take := min(len(p), 2*v.values.longest)
		v.joined = append(append(v.joined[:0], v.held...), p[:take]...)
		if take == len(p) {
			at, err := v.step(v.joined, len(v.joined), false)
			v.held = append(v.held[:0], v.joined[at:]...)
			return v.result(n, err)
		}
		at, err := v.step(v.joined, len(v.held), false)
		if err != nil {
			return v.result(n, err)
		}
		p = p[at-len(v.held):]
	}
	at, err := v.step(p, len(p), false)
	v.held = append(v.held[:0], p[at:]...)
	return v.result(n, err)
}

// Close decides every place still held, as the text's end decides them, and
// writes them. It closes nothing it was handed.
func (v *valueStream) Close() error {
	if v.err != nil || v.values == nil {
		return v.err
	}
	_, err := v.step(v.held, len(v.held), true)
	v.held = v.held[:0]
	_, err = v.result(0, err)
	return err
}

// step decides the places of text before stop, from its start, and writes
// them, a value taken out as [scrubMarker]: every place, once the text has
// ended, and otherwise each with the longest value's length of text read from
// it. It answers the first place it did not decide, every byte before which
// is written.
func (v *valueStream) step(text []byte, stop int, ended bool) (int, error) {
	at, last := 0, 0
	for at < stop {
		if !ended && at+v.values.longest > len(text) {
			break
		}
		matched := 0
		for _, value := range v.values.byFirst[text[at]] {
			if bytes.HasPrefix(text[at:], value) {
				matched = len(value)
				break
			}
		}
		if matched == 0 {
			at++
			continue
		}
		if err := v.literal(text[last:at]); err != nil {
			return at, err
		}
		if _, err := io.WriteString(v.w, scrubMarker); err != nil {
			return at, err
		}
		at += matched
		last = at
	}
	return at, v.literal(text[last:at])
}

// literal writes text as it is, and nothing where it is empty.
func (v *valueStream) literal(text []byte) error {
	if len(text) == 0 {
		return nil
	}
	_, err := v.w.Write(text)
	return err
}

// result is n read and the stream's error once err ends it.
func (v *valueStream) result(n int, err error) (int, error) {
	if err != nil {
		v.err = err
		return 0, err
	}
	return n, nil
}

// memberName is a tar member's name as the kept archive holds it: scrubbed by
// value and then by shape, as the transcript's paths to the member are (every
// string of a transcript passes both), so the restored file is where the
// transcript says it is — and a credential in a file's name, which a harness
// writes a tool's output under, does not survive in the archive's own words.
func memberName(s scrubber, name string) string {
	return redact.Secrets(s.apply(name))
}

// jsonEscaped is s as plainjson writes it in a JSON string, without its
// quotes: the form a JSON string a harness writes holds it in, escaped as
// JSON.stringify escapes with `<`, `>` and `&` left as they are, for every s
// but one holding U+2028 or U+2029, which plainjson writes as escapes and
// JSON.stringify as they are.
func jsonEscaped(s string) string {
	out, err := plainjson.Marshal(s, "")
	if err != nil {
		return s
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(out), `"`), `"`)
}

// credentialKey reports whether a JSON object's key names a credential, whose
// string or number value is taken out whole however it is shaped: read on the
// key's own words, lowercase, with its separators dropped, `token`, `key` and
// `auth` only as the whole key, and an OpenID Connect ID token's `id` only as a
// word of its own ([keyReading.idToken]). It is a [keyReading] handed the whole
// key, so a key read in pieces and one read whole share one rule. The names,
// and why neither the separators nor the short words are read at large, are in
// the package doc's "A kept session leaves its box scrubbed".
func credentialKey(key string) bool {
	var k keyReading
	k.WriteString(key)
	return k.Credential()
}

// wholeCredentialKeys are the keys [credentialKey] reads only as the whole key,
// lowercase: never at large, where a transcript's own keys spell them.
var wholeCredentialKeys = []string{"token", "key", "auth"}

// credentialNames are the names [credentialKey] reads anywhere in a key once
// it is lowercase and its separators are dropped.
var credentialNames = [][]byte{[]byte("apikey"), []byte("apitoken"), []byte("accesstoken"),
	[]byte("refreshtoken"), []byte("sessiontoken"), []byte("securitytoken"), []byte("authtoken"),
	[]byte("bearertoken"), []byte("privatekey"), []byte("privatetoken"), []byte("authorization"),
	[]byte("secret"), []byte("credential"), []byte("identitytoken"), []byte("registrytoken")}

// keyReadingReach is how far back each of [keyReading]'s windows has to reach:
// the longest whole key, the longest name, and the most runes a password's
// key spans ([redact.PasswordKeyMaxBytes]).
var keyReadingReach = func() (r struct{ whole, name, password int }) {
	for _, key := range wholeCredentialKeys {
		r.whole = max(r.whole, len(key))
	}
	for _, name := range credentialNames {
		r.name = max(r.name, len(name))
	}
	r.password = max(redact.PasswordKeyMaxBytes(), 1)
	return r
}()

// keyReading is a JSON object's key read a piece at a time, as its decoded
// text arrives, cut anywhere, a rune's bytes included: Write hands it the next
// piece, and Credential ends the key and reports whether it names a
// credential, [credentialKey]'s reading of the whole key.
//
// ITS STATE DOES NOT GROW WITH THE KEY, since a box's code can write a key a
// gibibyte long: a rune's cut bytes, the key lowered while it is short, and two
// windows over its last runes, twice the most a password's key spans and twice
// the longest name, each cut back to one fewer than its span when it fills, so
// a key not yet seen whole is still in it. A byte that begins no rune is U+FFFD
// wherever the key is cut ([utf8.FullRune]).
type keyReading struct {
	// cut is the bytes of a rune the last piece ended inside.
	cut  [utf8.UTFMax - 1]byte
	ncut int
	// lowered is the key lowered while it is no longer than the longest
	// whole key; long once it is.
	lowered []byte
	long    bool
	// written is the key's last runes as written, U+FFFD for a byte that
	// begins none, and runes how many it holds.
	written []byte
	runes   int
	// joined is the lowered key's last letters and digits.
	joined []byte
	// prev is the key's last rune; any whether it has one.
	prev rune
	any  bool
	// id is how far an ID token has read, and token how much of `token`.
	id    idTokenRead
	token int
	// named is whether a credential is named already: nothing read after
	// changes it.
	named bool
}

// idTokenRead is how far [keyReading.idToken] has read an ID token's words.
type idTokenRead int

const (
	// idNone: no `id` that starts a word is being read.
	idNone idTokenRead = iota
	// idI: an `i` that starts a word, its `d` next.
	idI
	// idWord: `id`, its separators being read, `token` next.
	idWord
	// idToken: `id` and its separators, and keyReading.token bytes of
	// `token`.
	idToken
)

// reset readies k for another key, keeping its windows' room.
func (k *keyReading) reset() {
	*k = keyReading{lowered: k.lowered[:0], written: k.written[:0], joined: k.joined[:0]}
}

// WriteString is Write of s's bytes.
func (k *keyReading) WriteString(s string) {
	for len(s) > 0 {
		var chunk [256]byte
		n := copy(chunk[:], s)
		_, _ = k.Write(chunk[:n])
		s = s[n:]
	}
}

// Write reads the next piece of the key. It never fails.
func (k *keyReading) Write(p []byte) (int, error) {
	n := len(p)
	if k.named {
		return n, nil
	}
	if k.ncut > 0 {
		// The rune a piece ended inside, finished from this one: at most
		// three of its bytes are needed, and a run that is no rune is read
		// a byte at a time, as the whole key's decode reads it.
		var joined [2*utf8.UTFMax - 2]byte
		s := append(joined[:0], k.cut[:k.ncut]...)
		s = append(s, p[:min(len(p), utf8.UTFMax-1)]...)
		i := 0
		for i < k.ncut {
			if !utf8.FullRune(s[i:]) {
				// The whole of p is in s, and still no rune.
				k.ncut = copy(k.cut[:], s[i:])
				return n, nil
			}
			r, w := utf8.DecodeRune(s[i:])
			k.rune(r)
			i += w
		}
		p = p[i-k.ncut:]
		k.ncut = 0
	}
	for i := 0; i < len(p); {
		if c := p[i]; c < utf8.RuneSelf {
			k.rune(rune(c))
			i++
			continue
		}
		if !utf8.FullRune(p[i:]) {
			k.ncut = copy(k.cut[:], p[i:])
			break
		}
		r, w := utf8.DecodeRune(p[i:])
		k.rune(r)
		i += w
	}
	return n, nil
}

// Credential ends the key, a rune a piece ended inside read as the whole
// key's decode reads it, and reports whether the key names a credential.
func (k *keyReading) Credential() bool {
	for i := 0; i < k.ncut; {
		r, w := utf8.DecodeRune(k.cut[i:k.ncut])
		k.rune(r)
		i += w
	}
	k.ncut = 0
	if k.named {
		return true
	}
	if k.runes > 0 && redact.NamesPassword(string(k.written)) {
		return true
	}
	if !k.long && slices.Contains(wholeCredentialKeys, string(k.lowered)) {
		return true
	}
	return k.joinedNames()
}

// rune reads the key's next rune.
func (k *keyReading) rune(r rune) {
	if k.named {
		return
	}
	reach := keyReadingReach
	k.written = utf8.AppendRune(k.written, r)
	if k.runes++; k.runes == 2*reach.password {
		if redact.NamesPassword(string(k.written)) {
			k.named = true
			return
		}
		k.written, k.runes = lastRunes(k.written, reach.password-1), reach.password-1
	}
	lower := unicode.ToLower(r)
	if !k.long {
		if k.lowered = utf8.AppendRune(k.lowered, lower); len(k.lowered) > reach.whole {
			k.long, k.lowered = true, nil
		}
	}
	if unicode.IsLetter(lower) || unicode.IsDigit(lower) {
		if k.joined = utf8.AppendRune(k.joined, lower); len(k.joined) >= 2*reach.name {
			if k.joinedNames() {
				k.named = true
				return
			}
			// A name still to be read whole begins in the last bytes
			// but one of its length: bytes, not runes, since the names
			// are matched as bytes.
			k.joined = append(k.joined[:0], k.joined[len(k.joined)-(reach.name-1):]...)
		}
	}
	k.idToken(r)
	k.prev, k.any = r, true
}

// joinedNames reports whether the joined window holds one of
// [credentialNames].
func (k *keyReading) joinedNames() bool {
	for _, name := range credentialNames {
		if bytes.Contains(k.joined, name) {
			return true
		}
	}
	return false
}

// idToken reads r for an OpenID Connect ID token: `id` a word of its own and
// `token` after it, however a format joins the two (`id_token`, `idToken`,
// `OIDC_ID_TOKEN`), each of its letters in either case.
//
// A WORD OF ITS OWN, where every other name is read anywhere in the key,
// because `idtoken` is what a count spells by chance (`paid_tokens`,
// `invalid_tokens`), and a number under such a key is no credential's.
//
// ONE `id` AT A TIME: no letter of `d`, the separators or `token` is an `i`,
// so an `id` read on is never beside another that could start, and a rune that
// ends one reading is read again as the start of the next.
func (k *keyReading) idToken(r rune) {
	switch k.id {
	case idI:
		if r == 'd' || r == 'D' {
			k.id = idWord
			return
		}
	case idWord:
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return
		}
		if r == 't' || r == 'T' {
			k.id, k.token = idToken, 1
			return
		}
	case idToken:
		if r|0x20 == rune("token"[k.token]) {
			if k.token++; k.token == len("token") {
				k.named = true
			}
			return
		}
	}
	k.id = idNone
	if (r == 'i' || r == 'I') && k.wordStarts(r) {
		k.id = idI
	}
}

// wordStarts reports whether one of the key's words begins at r, read after
// the key's last rune: at the key's start, after a character that is neither
// a letter nor a digit, or at an uppercase letter after a lowercase one or a
// digit (`oidcIdToken`).
func (k *keyReading) wordStarts(r rune) bool {
	if !k.any || !unicode.IsLetter(k.prev) && !unicode.IsDigit(k.prev) {
		return true
	}
	return unicode.IsUpper(r) && (unicode.IsLower(k.prev) || unicode.IsDigit(k.prev))
}

// lastRunes is b's last n runes, moved to b's start.
func lastRunes(b []byte, n int) []byte {
	at := len(b)
	for ; n > 0 && at > 0; n-- {
		_, w := utf8.DecodeLastRune(b[:at])
		at -= w
	}
	return append(b[:0], b[at:]...)
}

// scrubArchive streams the archive first holds, of layout, scrubbed onto dst
// whole, within bounds, and framed onto their body where they keep media
// apart ([scrubBounds.splitting]). reopen opens the same archive again from its start, for the
// second of the two readings every layout is read in. It closes nothing it was
// handed; the caller owns every reader.
func scrubArchive(layout archiveLayout, first io.Reader, reopen func() (io.ReadCloser, error),
	dst io.Writer, s scrubber, b scrubBounds) error {

	switch layout {
	case archiveTar:
		return scrubTar(first, reopen, dst, s, b)
	case archiveDocument:
		return scrubDocument(first, reopen, dst, s, b)
	}
	return fmt.Errorf("executor: no scrub for an archive of layout %d", layout)
}

// scrubDocument streams a gzipped JSON document, rewritten string by string:
// rewritten once into the take's spool ([firstReadings]), then written from
// it.
//
// SPOOLED FIRST, because a reading refuses a document only once it has read
// it, and read once onto the take's pipe, the bytes rewritten before a refusal
// would reach its uploader, its checksum and its meter, a published key among
// them where the redaction refused. The package doc's "The scrub holds no
// value whole" has the whole argument.
func scrubDocument(first io.Reader, reopen func() (io.ReadCloser, error), dst io.Writer, s scrubber,
	b scrubBounds) error {

	spool, err := newUnitSpool(b.spool)
	if err != nil {
		return err
	}
	defer spool.remove()
	s = s.tallying(b.tally.member(""))
	plan, err := firstReadings(first, reopen, s, b, spool, func(src io.Reader, s scrubber, plan *tarPlan) error {
		if exportErr := rewriteExport(src, plan.spool, s.way(jsonWay), b); exportErr != nil {
			return exportErr
		}
		return plan.holdPast()
	})
	defer plan.letGo()
	if err != nil {
		return fmt.Errorf("executor: scrubbing the session's export: %w", err)
	}
	b.tally.took(plan.spool)
	kept, err := plan.spool.reader()
	if err != nil {
		return err
	}
	plan.media.paced(plan.spool.disk.n)
	zw, err := newMemberSplit(dst, b.split.body, &plan.media)
	if err != nil {
		return err
	}
	from := newSpoolCopy(kept, plan, zw)
	if _, err = io.Copy(zw, from); err != nil {
		return err
	}
	if !from.done() {
		return errSpoolRead
	}
	return zw.Close()
}

// rewriteExport is one reading of a gzipped export, rewritten onto dst.
func rewriteExport(src io.Reader, dst io.Writer, s scrubber, b scrubBounds) error {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("executor: the session's export is not gzip: %w", err)
	}
	return rewriteDocument(dst, boundedPlain(zr, b.plain), s, b.gate)
}

// scrubTar streams a gzipped tar with every regular file rewritten: read once
// into the take's spool ([firstReadings]), each file as what its name says it
// is ([measureFile]) behind what its header keeps ([writeMemberHeader]), its
// size as rewritten planned; and written from the spool under a header that
// says so.
//
// THE SECOND READING IS THE SPOOL, NEVER THE BOX: every header it writes
// gives the size of the bytes it copies after it, which the first reading
// counted as it spooled them, so no header can lie however the box's archive
// changes while it is taken.
func scrubTar(first io.Reader, reopen func() (io.ReadCloser, error), dst io.Writer, s scrubber,
	b scrubBounds) error {

	spool, err := newUnitSpool(b.spool)
	if err != nil {
		return err
	}
	defer spool.remove()
	plan, err := firstTarReadings(first, reopen, s, b, spool)
	defer plan.letGo()
	if err != nil {
		return err
	}
	b.tally.took(plan.spool)
	kept, err := plan.spool.reader()
	if err != nil {
		return err
	}
	plan.media.paced(plan.spool.disk.n)
	zw, err := newMemberSplit(dst, b.split.body, &plan.media)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	from := newSpoolCopy(kept, plan, zw)
	for !plan.members.done() {
		kind, ok := plan.members.takeByte()
		if !ok {
			return errSpoolRead
		}
		if kind != tar.TypeReg && kind != tar.TypeDir {
			continue
		}
		header, headerErr := readMemberHeader(from, kind)
		if headerErr != nil {
			return headerErr
		}
		if kind == tar.TypeReg {
			size, sized := plan.members.takeNumber()
			if !sized {
				return errSpoolRead
			}
			header.Size = int64(size)
		}
		if err = tw.WriteHeader(&header); err != nil {
			return err
		}
		if _, err = io.CopyN(tw, from, header.Size); err != nil {
			return fmt.Errorf("%w: %w", errSpoolRead, err)
		}
	}
	if !from.done() {
		return errSpoolRead
	}
	if err = tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// firstTarReadings is a tar's first readings into spool, and the plan the
// second reads it back by ([firstReadings]).
func firstTarReadings(first io.Reader, reopen func() (io.ReadCloser, error), s scrubber, b scrubBounds,
	spool *unitSpool) (*tarPlan, error) {
	return firstReadings(first, reopen, s, b, spool, func(src io.Reader, s scrubber, plan *tarPlan) error {
		member := -1
		return eachMember(src, b, func(h *tar.Header, body io.Reader) error {
			member++
			body = b.prefix.member(member, h, body)
			resumed, memberErr := plan.memberAt(member, h, body)
			if !resumed {
				return memberErr
			}
			membersBits := plan.members.count()
			plan.members.putByte(h.Typeflag)
			if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
				return plan.holdPast()
			}
			kept := memberName(s, h.Name)
			if plan.carry == nil {
				if memberErr = writeMemberHeader(plan.spool, kept, h.Mode&0o777, h.ModTime); memberErr != nil {
					return memberErr
				}
			}
			if h.Typeflag == tar.TypeDir {
				return plan.holdPast()
			}
			plan.markMember(member, membersBits, h.Name)
			size, memberErr := measureFile(h.Name, body, s.tallying(b.tally.member(kept)), plan)
			if memberErr != nil {
				return memberErr
			}
			plan.members.putNumber(uint64(size))
			return plan.holdPast()
		})
	})
}

// firstReadings runs read, a first reading, on first, writing into spool
// (made by its caller, who removes it on every way out) and planning on a
// plan holding the large slot until its letGo. EVERY RE-READ OF THE BOX IS
// HERE: the second reading reads the spool alone and is never handed reopen.
// It reads again on from a unit to read as text alone, the plan and spool up
// to it kept and the unit's spooled ways cut back ([errReadBothWays],
// [errTextAgain], [tarPlan.memberAt]); and from the start, the spool emptied,
// where the memo's answer for a text diverged ([errMemoDiverged]), once with
// that text's first chunk looked up no more, then with no lookups at all.
func firstReadings(first io.Reader, reopen func() (io.ReadCloser, error), s scrubber, b scrubBounds,
	spool *unitSpool, read func(io.Reader, scrubber, *tarPlan) error) (*tarPlan, error) {

	pieces := newFirstPieces(b.memo)
	plan := &tarPlan{gate: b.gate, spool: spool,
		media: mediaPlan{on: b.split.body != nil, allowance: b.split.allowance}}
	for src, diverged := first, 0; ; {
		err := read(src, s.firstReading(pieces).finding(plan.media.finding()), plan)
		switch {
		case errors.Is(err, errReadBothWays) || errors.Is(err, errTextAgain):
			// THE UNIT'S JSON IS CUT BACK OUT ALREADY, so the spool stands
			// where the unit began, where the next reading goes on.
			if spool.at != plan.resume.spoolAt {
				return plan, errSpoolRead
			}
		case errors.Is(err, errMemoDiverged):
			if diverged++; diverged == 2 {
				pieces.lookups = false
			}
			plan.letGo()
			if err = spool.empty(); err != nil {
				return plan, err
			}
			plan = &tarPlan{gate: b.gate, spool: spool, bothFrom: plan.bothFrom, textUnit: plan.textUnit,
				media: plan.media.restarted()}
		default:
			// THE WAYS' ROOM IS THE FIRST READING'S ALONE: the second holds
			// none of it.
			plan.candidates = [2][]byte{}
			return plan, err
		}
		next, openErr := reopen()
		if openErr != nil {
			return plan, fmt.Errorf("executor: opening the session's archive again to read it: %w", openErr)
		}
		src = next
	}
}

// memberNamed is an archive member's name as a failure quotes it: its first
// line, redacted and held to what a transcript line quotes of a path
// ([firstLine], [transcriptDetailLimit]).
//
// THE NAME IS THE ARCHIVE'S, which the box wrote, and a tar can name a member
// in up to a MiB (a PAX record); the failure becomes the reason the run's
// session was not kept, which the run's row, the launch's record and a box
// host's reply to the node that asked for the read all carry.
func memberNamed(name string) string { return firstLine(name, transcriptDetailLimit) }

// eachMember reads a gzipped tar member by member, its plaintext bounded.
func eachMember(src io.Reader, b scrubBounds, member func(*tar.Header, io.Reader) error) error {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("executor: the session's archive is not gzip: %w", err)
	}
	tr := tar.NewReader(boundedPlain(zr, b.plain))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("executor: reading the session's archive: %w", err)
		}
		if err = member(h, tr); err != nil {
			return err
		}
	}
}

// boundedPlain is a session's decompressed stream, refused past bound bytes:
// an archive a box's own code wrote can expand without end.
func boundedPlain(r io.Reader, bound int64) io.Reader {
	return &plainBound{r: r, left: bound, bound: bound}
}

// plainBound is [boundedPlain]'s reader.
type plainBound struct {
	r           io.Reader
	left, bound int64
}

func (p *plainBound) Read(buf []byte) (int, error) {
	if p.left <= 0 {
		// ONE BYTE MORE says whether the stream ends at the bound or goes
		// past it.
		var one [1]byte
		n, err := p.r.Read(one[:])
		if n > 0 {
			return 0, fmt.Errorf("%w: it expands past %d bytes", errScrubBound, p.bound)
		}
		return 0, err
	}
	if int64(len(buf)) > p.left {
		buf = buf[:p.left]
	}
	n, err := p.r.Read(buf)
	p.left -= int64(n)
	return n, err
}

// counted counts what passes through it to w, or counts alone where w is nil.
type counted struct {
	w io.Writer
	n int64
}

func (c *counted) tally(taken int64) { tallyOnto(c.w, taken) }

func (c *counted) Write(p []byte) (int, error) {
	if c.w == nil {
		c.n += int64(len(p))
		return len(p), nil
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// measureFile spools one file of an archive as it is kept, read through by
// the tar's first reading, and says how many bytes it comes to and, for a
// `.json` file, whether it is kept as text because it is not JSON; a
// transcript is spooled a line at a time ([measureLines]).
//
// A `.json` FILE IS READ AS A DOCUMENT ALONE, and both ways from the first
// unit that is no JSON ([tarPlan.unitAhead]), at once ON ONE GOROUTINE, THE
// SCRUB'S OWN: the rewrite hands every byte it reads to the text's scrub
// ([teeRead]), so every byte is read where the take contains a panic
// (scrubContained). Goroutines fed through pipes would put the file's rewrite
// and redaction outside that containment (the package doc's "The scrub holds
// no value whole").
func measureFile(name string, body io.Reader, s scrubber, plan *tarPlan) (size int64, err error) {
	switch path.Ext(name) {
	case ".jsonl":
		return measureLines(body, s, plan)
	case ".json":
	default:
		n := &counted{w: plan.spool}
		err = rewriteText(n, body, s, plan.gate)
		return n.n, err
	}
	plan.carry = nil
	ways := plan.unitAhead()
	// ONE PIECE, THE WHOLE FILE: its text is redacted as one, so the
	// document's strings within it need no gate of their own.
	body, done := plan.gate.pieces(body, false)
	defer done()
	switch ways {
	case jsonAlone:
		return measureDocument(body, s, plan)
	case textAlone:
		n := &counted{w: plan.spool}
		err = rewriteText(n, body, s, pieceGate{})
		return n.n, err
	}
	asText, asJSON := plan.candidate(textWay), plan.candidate(jsonWay)
	textN := &counted{w: asText}
	textual := s.way(textWay).text(textN)
	tee := &teeRead{r: body, w: textual}
	documentN, documentTaken, documentErr := rewriteDocumentOnto(asJSON, tee, s.way(jsonWay), pieceGate{})
	asJSON.tally(documentTaken)
	// A DOCUMENT THE REWRITE CANNOT READ, FOR WHATEVER REASON, IS TEXT: torn,
	// more than one value, nested past the scanner's depth, or holding text
	// its redaction could not redact; kept as text only where its text's own
	// redaction did not fail. The archive's own failure is neither's, and
	// refuses the session below; so is a memo's answer that diverged, read
	// again rather than kept as text, which loses a credential key's value.
	if documentErr != nil && !errors.Is(documentErr, errMemoDiverged) {
		if err = asJSON.discard(); err != nil {
			return 0, err
		}
		if asText.dropped {
			return 0, plan.textAgain()
		}
		if err = asText.keep(); err != nil {
			return 0, err
		}
	}
	// THE REST IS THE TEXT'S ALONE: a document the rewrite stopped reading
	// is read on to its end, for its text.
	if _, err = io.Copy(io.Discard, tee); err == nil {
		err = tee.readErr
	}
	if err != nil {
		return 0, err
	}
	textErr := tee.writeErr
	if closeErr := textual.Close(); textErr == nil {
		textErr = closeErr
	}
	textN.tally(textual.taken)
	switch {
	case errors.Is(documentErr, errMemoDiverged):
		return 0, documentErr
	case documentErr == nil:
		if err = asJSON.keep(); err != nil {
			return 0, err
		}
		return documentN, nil
	case textErr != nil:
		return 0, textErr
	}
	return textN.n, nil
}

// measureDocument spools a `.json` file read as a document alone
// ([measureFile]) and says how many bytes it comes to, or, where it is none,
// [tarPlan.notJSON], its JSON cut back out of the spool. One the rewrite stops
// reading for its archive's failure is that failure.
func measureDocument(body io.Reader, s scrubber, plan *tarPlan) (int64, error) {
	read := &teeRead{r: body, w: io.Discard}
	asJSON := plan.candidate(jsonWay)
	n, taken, err := rewriteDocumentOnto(asJSON, read, s.way(jsonWay), pieceGate{})
	asJSON.tally(taken)
	switch {
	case read.readErr != nil:
		return 0, read.readErr
	case err == nil:
		return n, asJSON.keep()
	case errors.Is(err, errMemoDiverged):
		return 0, err
	}
	if err = asJSON.discard(); err != nil {
		return 0, err
	}
	return 0, plan.notJSON()
}

// teeRead reads r and hands every byte it reads to w as well: a `.json`
// file's text scrub, fed what the document's rewrite reads of the file
// ([measureFile]).
type teeRead struct {
	r io.Reader
	w io.Writer
	// readErr is r's failure, other than its end, which no reading of the
	// file survives; writeErr is w's first, after which w is handed nothing
	// more and the document is read on.
	readErr, writeErr error
}

func (t *teeRead) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && t.writeErr == nil {
		_, t.writeErr = t.w.Write(p[:n])
	}
	if err != nil && !errors.Is(err, io.EOF) && t.readErr == nil {
		t.readErr = err
	}
	return n, err
}

// tarPlan is what the first reading of a tar, or of an export, learned as it
// spooled it, which the second reads back as it copies the spool: two
// streams of bits ([planBits]), each written by the first reading in the order
// it decides them and read by the second in that order: members (each one's
// kind, and a regular file's size as kept) and edits (each line a cut takes
// loaded tool definitions from, [tarPlan.addCut]).
//
// SO IT IS AT MOST A SIXTEENTH OF THE PLAINTEXT (the package doc's "The scrub
// holds no value whole").
type tarPlan struct {
	members, edits planBits
	// spool is the take's spool, which the plan's marks and edits name
	// places in ([unitSpool.at]); candidates is the room a unit holds each
	// of its ways in until its way is known ([candidate]).
	spool      *unitSpool
	candidates [2][]byte
	// units counts the lines of two bytes or more and the `.json` files the
	// first reading has begun, from 1; bothFrom is the first it reads both
	// ways, and every one after it, 0 for none; textUnit is one it reads as
	// text alone, 0 for none ([tarPlan.unitAhead]).
	units, bothFrom, textUnit int
	// mark is the plan where the first reading's current unit began; resume
	// the mark of the unit the next reading goes on from
	// ([tarPlan.memberAt]); carry what of the unit's member that reading
	// reads past, until the member's own reading takes it.
	mark          planMark
	resume, carry *planMark
	// edited is where in the spool the last cut's line begins, which the
	// next one's place counts from; editRead the same for the second
	// reading.
	edited, editRead int64
	// gate is the scrub's way to the large slot ([scrubBounds.gate]), which
	// the plan holds once it is past the base ([tarPlan.holdPast]); held
	// says it does.
	gate pieceGate
	held bool
	// media is the payloads the take keeps apart ([mediaPlan]).
	media mediaPlan
}

// holdPast holds the large slot once the plan is past [scrubPieceBaseBytes]
// of bits, to the take's end ([tarPlan.letGo]): THE ONE THING THE SCRUB HOLDS
// ACROSS PIECES, which only a box's archive of millions of short lines or
// members brings there.
func (p *tarPlan) holdPast() error {
	bits := p.members.count() + p.edits.count()
	if p.held || p.gate.hold == nil || bits/8 <= scrubPieceBaseBytes {
		return nil
	}
	if err := p.gate.hold(); err != nil {
		return err
	}
	p.held = true
	return nil
}

// addCut plans the cut of list from the line the spool holds from at, which
// takes taken bytes from it ([definitionsReading.decide]): its bits move to
// the plan, and list holds none after it.
func (p *tarPlan) addCut(at int64, list *listRead, taken int64) {
	p.edits.putNumber(uint64(at - p.edited))
	p.edits.putNumber(uint64(list.container))
	p.edits.putNumber(uint64(list.n))
	p.edits.putNumber(uint64(taken))
	list.bits.moveTo(&p.edits)
	p.edited = at
}

// candidate is way of a unit, in the room the plan keeps for it: the text way
// drops what passes its room, the JSON way spills it.
func (p *tarPlan) candidate(way int) *candidate {
	if p.candidates[way] == nil {
		p.candidates[way] = make([]byte, 0, wayRoomBytes)
	}
	return newCandidate(p.spool, p.candidates[way], way == textWay)
}

// unitWays is how the first reading reads a unit: as JSON alone, as text
// alone, or both ways at once, each spooled as a [candidate] until the unit's
// way is known.
type unitWays int

const (
	jsonAlone unitWays = iota
	bothWays
	textAlone
)

// unitAhead begins the first reading's next unit, a transcript's line of two
// bytes or more or a `.json` file, and says how it is read: as text alone
// where a reading must read it so; both ways from the first an earlier
// reading found is no JSON on; and else as JSON alone.
//
// A UNIT IS READ BOTH WAYS ONLY FROM THE FIRST THAT IS NO JSON: which it is is
// known only at its end and none is held, and a JSON unit's text scrub, never
// written, cost two fifths of the scrub of a password-dense string. So the
// first that is not ends the reading at once ([tarPlan.notJSON]), and the
// next goes on from it ([tarPlan.memberAt]), reading it as text alone and
// every one after it both ways: a transcript of text costs what it did, and
// one torn at its last line nothing more.
func (p *tarPlan) unitAhead() unitWays {
	p.units++
	switch {
	case p.units == p.textUnit:
		return textAlone
	case p.bothFrom > 0 && p.units >= p.bothFrom:
		return bothWays
	}
	return jsonAlone
}

// notJSON is the failure of the unit [tarPlan.unitAhead] began, read as JSON
// alone and found no JSON: errReadBothWays, the reading to end there and the
// next to go on from its mark, reading it as text alone and every unit after
// it both ways.
func (p *tarPlan) notJSON() error {
	p.bothFrom = p.units
	p.resumeAtMark()
	return errReadBothWays
}

// errTextAgain ends a first reading at a unit it read both ways whose text it
// let go before its JSON ended as none ([candidate]), the next to read on from
// it as text alone ([firstReadings]).
var errTextAgain = errors.New("executor: a line or a `.json` file is no JSON past where its text was held, " +
	"read again as text")

// textAgain is the failure of a unit read both ways that must be read again
// as text alone: errTextAgain, the reading to end there and the next to go
// on from its mark.
func (p *tarPlan) textAgain() error {
	p.resumeAtMark()
	return errTextAgain
}

// resumeAtMark has the next reading go on from the current unit, read as
// text alone.
func (p *tarPlan) resumeAtMark() {
	p.textUnit = p.units
	mark := p.mark
	p.resume = &mark
}

// planMark is where a unit began: its member, by its place among the
// archive's members and its name, and the bits the members' stream held
// before the member's own; the line's offset in the member and what the
// member's lines before it come to as kept; the units begun before it; and
// where the spool stood. A reading ends at a unit having planned nothing of
// it but its member's kind ([tarPlan.notJSON]), and the unit's JSON cut back
// out of the spool, so both stand at the mark as the reading left them.
type planMark struct {
	member, membersBits, units, media int
	offset, size, spoolAt             int64
	name                              string
}

// markMember marks the regular member named name at its place among the
// archive's members, its kind's bits written past membersBits, as its unit
// begins.
func (p *tarPlan) markMember(member, membersBits int, name string) {
	p.mark.member, p.mark.membersBits, p.mark.name = member, membersBits, name
	p.markUnit(0, 0)
}

// markUnit marks where the member's next unit begins: offset bytes into it,
// its lines before it come to size as kept.
func (p *tarPlan) markUnit(offset, size int64) {
	p.mark.offset, p.mark.size, p.mark.units, p.mark.spoolAt = offset, size, p.units, p.spool.at
	p.mark.media, p.media.unitAt = len(p.media.found), p.spool.at
}

// memberAt begins the member at its place among the archive's members, h its
// header and body its bytes, in a reading that goes on from resume
// ([firstReadings]): one before resume's is read past (resumed false);
// resume's own has its kind's bits cut and its bytes before the unit read
// past, and is refused as changed where its name, its kind or its length is
// not what the reading before read; every later one is read as it comes.
//
// A READING GOES ON FROM WHERE THE ONE BEFORE ENDED, not from the start: the
// plan and the spool before that unit are the same whoever reads it, and read
// again, a transcript torn at its last line, as any take that lands mid-write
// finds, cost its JSON a second reading.
func (p *tarPlan) memberAt(member int, h *tar.Header, body io.Reader) (resumed bool, err error) {
	r := p.resume
	switch {
	case r == nil || member > r.member:
		p.resume = nil
		return true, nil
	case member < r.member:
		return false, nil
	case h.Typeflag != tar.TypeReg || h.Name != r.name:
		return false, errMemberChanged(h.Name)
	}
	p.members.list.truncate(r.membersBits)
	p.media.dropTo(r.media)
	p.units = r.units
	p.resume, p.carry = nil, r
	if _, err = io.CopyN(io.Discard, body, r.offset); errors.Is(err, io.EOF) {
		return false, errMemberChanged(h.Name)
	} else if err != nil {
		return false, fmt.Errorf("executor: reading the session's archive: %w", err)
	}
	return true, nil
}

// errMemberChanged is the member named name, as a failure quotes it, found
// changed by a reading that goes on from it.
func errMemberChanged(name string) error {
	return fmt.Errorf("executor: the session's archive changed its member %s while it was read",
		memberNamed(name))
}

// errReadBothWays ends a first reading at the first unit it read as JSON alone
// that is no JSON, the next to read on from it ([firstReadings]).
var errReadBothWays = errors.New("executor: a line or a `.json` file is no JSON, read again both ways")

// letGo drops the slot the plan holds, once the take is done with it.
func (p *tarPlan) letGo() {
	if p.held {
		p.gate.drop()
		p.held = false
	}
}

// lineReader is a transcript's lines, read one at a time ([lineReader.next])
// as the scanner's text or the text scrub's: a line's bytes up to its break,
// never the break, with io.EOF at its end. tee is handed every byte of the line
// read, where it is set: the first reading's text candidate.
type lineReader struct {
	r   *bufio.Reader
	tee io.Writer
	// done says the line is read to its end; ended that its break was read.
	done, ended bool
	// err is an error of the transcript's reader, which ends the reading.
	err error
	// at is how many of the text's bytes are read: where the next line
	// begins.
	at int64
	// sc and j rewrite a line as JSON, each allocated once for every line
	// of a reading; drain is what a line's rest is read into.
	sc    *jsonscan.Scanner
	j     *jsonRewrite
	drain []byte
}

// newLineReader reads src's lines, rewriting them with s's values.
func newLineReader(src io.Reader, s scrubber) *lineReader {
	l := &lineReader{r: bufio.NewReaderSize(src, 64<<10), j: newJSONRewrite(s), drain: make([]byte, 32<<10)}
	l.sc = jsonscan.New(l)
	return l
}

// next readies the next line, tee handed its bytes: short says it is shorter
// than two bytes before its break, and more is false at the transcript's end,
// where no line is left.
func (l *lineReader) next(tee io.Writer) (short, more bool, err error) {
	head, err := l.r.Peek(2)
	if len(head) == 0 {
		if errors.Is(err, io.EOF) {
			return false, false, nil
		}
		return false, false, err
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return false, false, err
	}
	l.tee, l.done, l.ended, l.err = tee, false, false, nil
	return head[0] == '\n' || len(head) == 1 || head[1] == '\n', true, nil
}

// Read reads the line's next bytes, never past its break.
func (l *lineReader) Read(p []byte) (int, error) {
	if l.done {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if l.r.Buffered() == 0 {
		if _, err := l.r.Peek(1); err != nil {
			l.done = true
			if !errors.Is(err, io.EOF) {
				l.err = err
				return 0, err
			}
			return 0, io.EOF
		}
	}
	chunk, _ := l.r.Peek(min(l.r.Buffered(), len(p)))
	took := len(chunk)
	if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
		chunk, took = chunk[:i], i+1
		l.done, l.ended = true, true
	}
	n := copy(p, chunk)
	if l.tee != nil && n > 0 {
		if _, err := l.tee.Write(p[:n]); err != nil {
			l.err, l.done = err, true
			return 0, err
		}
	}
	_, _ = l.r.Discard(took)
	l.at += int64(took)
	if n == 0 && l.done {
		return 0, io.EOF
	}
	return n, nil
}

// rest reads the line to its end, its bytes handed to tee.
func (l *lineReader) rest() error {
	for {
		_, err := l.Read(l.drain)
		if errors.Is(err, io.EOF) {
			return l.err
		}
		if err != nil {
			return err
		}
	}
}

// measureLines spools a transcript as it is kept, read through by the tar's
// first reading, and says how many bytes it comes to.
//
// A LINE IS READ AS JSON ALONE, AND BOTH WAYS FROM THE FIRST THAT IS NO JSON
// ([tarPlan.unitAhead]): as JSON for as long as its grammar holds, and as
// text, two scrubs side by side, each a [candidate] for the spool. The one
// kept is written exactly as the scrub of that line whole writes it, and the
// other passed over.
func measureLines(src io.Reader, s scrubber, plan *tarPlan) (int64, error) {
	src, done := plan.gate.pieces(src, true)
	defer done()
	l := newLineReader(src, s.way(jsonWay))
	defs := &definitionsReading{}
	var asJSON rewriteOut
	var size int64
	if c := plan.carry; c != nil {
		l.at, size, plan.carry = c.offset, c.size, nil
	}
	for {
		plan.markUnit(l.at, size)
		short, more, err := l.next(nil)
		if err != nil || !more {
			return size, err
		}
		ways := textAlone
		if !short {
			ways = plan.unitAhead()
		}
		asText := &counted{w: plan.spool}
		var text *textScrub
		var textC, jsonC *candidate
		if ways == bothWays {
			textC = plan.candidate(textWay)
			asText.w = textC
		}
		if ways != jsonAlone {
			text = s.way(textWay).text(asText)
			l.tee = text
		}
		var jsonErr, textErr error
		if ways != textAlone {
			jsonC = plan.candidate(jsonWay)
			asJSON.reset(jsonC, true)
			l.sc.Reset(l)
			l.j.reset(l.sc, &asJSON, defs)
			jsonErr = l.j.run()
			jsonC.tally(asJSON.taken)
			// NO JSON: its JSON is cut back out of the spool, and its text
			// is the line's, written on into the spool from here, or read
			// again alone where it was let go.
			var fault *jsonscan.Error
			if textC != nil && errors.As(jsonErr, &fault) {
				if err = jsonC.discard(); err != nil {
					return 0, err
				}
				if textC.dropped {
					return 0, plan.textAgain()
				}
				if err = textC.keep(); err != nil {
					return 0, err
				}
			}
		}
		if err = l.rest(); err != nil {
			return 0, err
		}
		if text != nil {
			textErr = text.Close()
			asText.tally(text.taken)
		}
		keptAsText, err := lineKeptAs(ways == textAlone, jsonErr, textErr)
		switch {
		case err != nil:
			return 0, err
		case keptAsText && ways == jsonAlone:
			if err = jsonC.discard(); err != nil {
				return 0, err
			}
			return 0, plan.notJSON()
		case keptAsText:
			size += asText.n
		default:
			if err = jsonC.keep(); err != nil {
				return 0, err
			}
			n := asJSON.n
			if list, taken, ok := defs.decide(asJSON.marked); ok {
				// A LINE THAT LOSES A DEFINITION KEEPS ITS PAYLOADS INLINE, so
				// the second reading's cut never meets a member's edge.
				plan.media.dropTo(plan.mark.media)
				plan.addCut(plan.mark.spoolAt, list, taken)
				n -= taken
				// THE CUT TAKES OUT BYTES OF THE LINE AS WRITTEN, which the
				// box's own copy still holds.
				if s.tally {
					jsonC.tally(taken)
				}
			}
			size += n
		}
		if l.ended {
			if _, err = plan.spool.Write([]byte{'\n'}); err != nil {
				return 0, err
			}
			size++
		}
		if err = plan.holdPast(); err != nil {
			return 0, err
		}
	}
}

// lineKeptAs is which of a line's two readings the first reading keeps, alone
// saying the line is read as text alone (a short line, or one known to be no
// JSON), jsonErr what ended its JSON reading and textErr what its text's scrub
// answered at its close: the text where it is read alone or is no JSON value
// (a *jsonscan.Error), and its JSON otherwise. err is the kept reading's own
// failure, which refuses the session.
//
// NEVER THE OTHER'S: a reading that is not written is no miss, so a text the
// redaction could not finish deciding refuses a line kept as text and not one
// kept as JSON, whose strings the redaction reads on their own; as a `.json`
// file's document read whole leaves its text's failure unread
// ([measureFile]).
func lineKeptAs(alone bool, jsonErr, textErr error) (text bool, err error) {
	var fault *jsonscan.Error
	switch {
	case alone || errors.As(jsonErr, &fault):
		return true, textErr
	case jsonErr != nil:
		return false, jsonErr
	}
	return false, nil
}

// loadedToolsRecord is the `type` of the attachment Claude Code keeps in its
// transcript for the tool definitions a session loaded through tool search,
// each sent again as a tool of every request a resumed session makes.
//
// A DEFINITION THE SCRUB RENAMED IS CUT OUT OF ITS LINE ([definitionsReading],
// [definitionCut]): one named outside what [providerToolName] takes, in a
// line that holds a marker, would have every request of the restored session
// refused. It is cut from the line as the rewrite wrote it, every other byte
// where it was, never the record written again from a map, whose sorted keys
// Claude Code misreads past 5 MiB; decided in the first reading, applied in
// the second. The package doc's "A definition the scrub renamed is cut out of
// its line" has the reading of both releases.
const loadedToolsRecord = "deferred_tools_record"

// providerToolNameMax is the longest name the provider takes for a tool.
const providerToolNameMax = 128

// providerToolName reports whether name is one the provider takes for a tool
// (`^[a-zA-Z0-9_-]{1,128}$`); a request carrying a definition named outside
// that is refused whole. Spelled out, not a compiled pattern: a package-level
// one is allocated at every start of the binary, the git hooks' included.
func providerToolName(name string) bool {
	if len(name) < 1 || len(name) > providerToolNameMax {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// rewriteDocument streams one JSON value from src to dst a token at a time,
// every string value and every key scrubbed and redacted, and a value under a
// credential's key taken out whole ([jsonRewrite]). A text that is no one JSON
// value ends it on the scanner's *jsonscan.Error: more than one value, one
// cut short, or one nested past jsonscan.MaxDepth. A name or a string past
// the base holds gate's slot while it is scrubbed ([jsonRewrite.text]).
//
// BUT A DOCUMENT OF BLANKS ALONE, or of nothing, holds no value a harness
// reads, and is written as nothing rather than refused as no JSON, as
// encoding/json's decoder answers such a text with its end. A transcript's
// line of blanks is kept as its blanks ([measureLines]).
func rewriteDocument(dst io.Writer, src io.Reader, s scrubber, gate pieceGate) error {
	w := bufio.NewWriterSize(dst, 64<<10)
	_, taken, err := rewriteDocumentOnto(w, src, s, gate)
	if err != nil {
		return err
	}
	if err = w.Flush(); err != nil {
		return err
	}
	tallyOnto(dst, taken)
	return nil
}

// rewriteDocumentOnto is [rewriteDocument] onto w, which holds its own bytes
// (a buffer, or a unit's way, [candidate]): how many bytes it wrote, and what
// its scrub took out where it tallies ([tallier]), which its caller hands on.
func rewriteDocumentOnto(w textSink, src io.Reader, s scrubber, gate pieceGate) (n, taken int64, err error) {
	var out rewriteOut
	out.reset(w, false)
	j := newJSONRewrite(s)
	j.reset(jsonscan.New(src), &out, nil)
	j.gate = gate
	var fault *jsonscan.Error
	if err = j.run(); err != nil && (!errors.As(err, &fault) || fault.Fault != jsonscan.FaultNoValue) {
		return 0, out.taken, err
	}
	return out.n, out.taken, nil
}

// keptString is a string value under a key that is no credential's, as a kept
// document holds it: every value the take names taken out, then every shape of
// a secret redacted. The ONE rule for it, so what the take keeps beside an
// archive of a document — where its run began in it ([keptRunStart]) — is
// spelled as the document spells the same text, whose rewrite scrubs a string
// by this rule whole or, past [textWholeBytes], through the streams that write
// exactly what it writes.
func keptString(scrub scrubber, s string) string {
	return keptText(scrub, s)
}

// rewriteText streams text scrubbed by value and redacted by shape as it
// arrives ([scrubber.text]): what it writes is exactly redact.Secrets of the
// whole text with every value taken out, held to the redaction's own windows
// rather than to a line or a stretch of it, and to gate's slot once the text
// is past the base ([pieceGate.pieces]).
func rewriteText(dst io.Writer, src io.Reader, s scrubber, gate pieceGate) error {
	src, done := gate.pieces(src, false)
	defer done()
	w := bufio.NewWriterSize(dst, 64<<10)
	text := s.text(w)
	if _, err := io.Copy(text, src); err != nil {
		return err
	}
	if err := text.Close(); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	tallyOnto(dst, text.taken)
	return nil
}
