package query

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/words"
)

// A PAGE IS MEASURED AS ITS JSON, because its bound (gitlet.MaxGitAnswerBytes)
// is the answer's bytes and text grows into JSON: a quote or a backslash is two
// bytes, a control character, a `<`, `>` or `&` (escaped for HTML) or a byte
// that is not UTF-8 six. Counted raw, a page of `<` would encode at six times
// its bound. A tree entry or a diff file is costed by encoding it, text by the
// per-rune table below, which init builds FROM encoding/json so the two cannot
// disagree.

// asciiCost is what each ASCII byte costs inside a JSON string, read off
// encoding/json at init; sepCost a U+2028 or U+2029 (each escaped, since a
// JavaScript string cannot hold one raw); badCost a byte that is not UTF-8,
// which the encoder writes as �.
var (
	asciiCost [utf8.RuneSelf]int
	sepCost   int
	badCost   int
)

func init() {
	for b := range utf8.RuneSelf {
		asciiCost[b] = encodedLen(string([]byte{byte(b)}))
	}
	sepCost = encodedLen(" ")
	badCost = encodedLen(string([]byte{0xff}))
}

// encodedLen is s's bytes inside a JSON string, its quotes left out.
func encodedLen(s string) int {
	out, err := json.Marshal(s)
	if err != nil {
		// A string always encodes; reaching here is a broken encoder,
		// which no answer could then be measured against.
		panic(fmt.Sprintf("query: encoding a string: %v", err))
	}
	return len(out) - 2
}

// runeCost is the cost of the rune b begins with and its size, or ok false
// where b holds only the start of one — more bytes may complete it, so a
// stream holds them back until they come or the text ends ([textCost]).
func runeCost(b []byte) (cost, size int, ok bool) {
	if c := b[0]; c < utf8.RuneSelf {
		return asciiCost[c], 1, true
	}
	if !utf8.FullRune(b) {
		return 0, 0, false
	}
	r, n := utf8.DecodeRune(b)
	switch {
	case r == utf8.RuneError && n == 1:
		return badCost, 1, true
	case r == ' ' || r == ' ':
		return sepCost, n, true
	}
	return n, n, true
}

// textCost is b's bytes inside a JSON string, b ending the text: a rune
// left incomplete at its end is bytes that are not UTF-8.
func textCost(b []byte) int {
	cost := 0
	for len(b) > 0 {
		c, n, ok := runeCost(b)
		if !ok {
			return cost + len(b)*badCost
		}
		cost, b = cost+c, b[n:]
	}
	return cost
}

// A FIELD THE CODE TOOLS FENCE IS COSTED AS THEY SEND IT. A file's page fences
// one text, which the room beside a tool's envelope holds; but a log's page
// fences every subject, a blame's every line and subject, and a diff's patches
// every patch, so each is costed as internal/agent/builtin sends it: fenced
// naming no writer at trust external (provenance.Repository) and NEUTRALISED
// (provenance.Neutralise lengthens every marker-shaped `[`). The fence is read
// off provenance at init, so a longer fence costs longer. Only the turn's own
// boundary, which no Gitlet node knows, is not costed: text spelling it is the
// turn's own echo, and a page it lengthens past one answer is refused by the
// tool naming a narrower read, never cut.

// fenceCost is what the fence of each source a log, a blame or a diff's
// patches fence adds to a text once encoded: its opening line, the line feed
// before its closer and the closer.
var fenceCost = map[provenance.Source]int{}

// costModel is what a fenced page was measured with (each fence's cost and one
// marker's neutral growth), in every memo key of such a page: a build whose
// fence or escape is longer cuts its pages anew rather than serve one measured
// short of it.
var costModel string

// init runs after the one above, whose table it reads: a file's inits run in
// the order it declares them.
func init() {
	for _, src := range []provenance.Source{provenance.SourceRepoFile, provenance.SourceRepoCommit,
		provenance.SourceRepoPatch} {
		const text = "x"
		fenced := provenance.Repository(src).Fence(provenance.NewBoundary(), text)
		fenceCost[src] = encodedLen(fenced) - encodedLen(text)
	}
	costModel = fmt.Sprintf("fence/%d/%d/%d/%d", fenceCost[provenance.SourceRepoFile],
		fenceCost[provenance.SourceRepoCommit], fenceCost[provenance.SourceRepoPatch], neutralGrowth("[untrusted"))
}

// neutralCost is s's bytes inside a JSON string once a code tool's answer has
// neutralised it — what every string of a log's or a blame's page costs, and
// what each field's bound (gitlet.MaxLogSubjectBytes and its kin) is held to.
func neutralCost(s string) int {
	return textCost([]byte(provenance.Neutralise("", s)))
}

// neutralGrowth is what neutralising s adds to its encoding: nothing but for
// text spelling a fence's marker.
func neutralGrowth(s string) int {
	return neutralCost(s) - textCost([]byte(s))
}

// jsonLen is v's encoded bytes.
func jsonLen(v any) int {
	out, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("query: encoding an answer: %v", err))
	}
	return len(out)
}

// room is what a page's items may take in an answer whose envelope (no items,
// every number and the cursor at their longest) is envelope. The envelope is
// stamped here ([read.stamp], Memo set; step 9 decorates before any page is
// cut), so no kind cuts a page its stamp then pushes past the bound. An
// unstamped read ([gitlet.GitQuery.Unstamped]) is stamped with none, so its
// page is a function of what it names alone.
func (r *read) room(envelope gitlet.GitAnswer) int {
	r.stamp(&envelope, true)
	return r.s.pageBytes - jsonLen(envelope)
}

// stamp sets on a every field Read stamps on an answer, or none on an
// unstamped read: THE ONE PLACE they are set, so the envelope a page is cut
// against, a memoized page's budget and the answer itself carry the same.
func (r *read) stamp(a *gitlet.GitAnswer, memo bool) {
	if r.q.Unstamped {
		return
	}
	a.RepoID, a.Position = r.q.RepoID, r.synced.Position
	a.PushedBy, a.Coverage, a.Memo = r.pushedBy, r.coverage, memo
	a.Tagged = r.tagged
}

// widest is the longest a page's count or position can encode as: every
// envelope is measured with its numbers at it, so a page cut against the
// envelope fits whatever its counts turn out to be.
const widest = math.MaxInt

// widestCursor is a cursor of letter at its longest.
func widestCursor(letter byte, numbers int) string {
	n := strconv.Itoa(widest)
	parts := []string{string(letter)}
	for range numbers {
		parts = append(parts, n)
	}
	return strings.Join(parts, ":")
}

// THE CURSORS. A page's Next is a letter and one or two positions, each
// decimal: "e:<n>" the n-th entry of a listing (counted from 0), "f:<n>" the
// n-th file of a diff's stat, "l:<n>" line n of a file — or of a blame's range
// — "b:<n>:<o>" byte o of line n, "p:<o>" byte o of a patch, "s:<n>" the n-th
// commit of a history's walk (counted from 0, the commits git skips first),
// "x:<n>" the n-th anchor of an excerpts read, "w:<n>:<o>" byte o of the n-th
// file's patch of a diff's patches (both from 1), and "d:<seconds>:<name>" a
// tags page's last tag (tags.go), a KEY that a listing at another position
// resumes past. Every other is a position in an answer that is a function of
// object ids alone, so a cursor any node issued is sound on any other.

// cursor is a parsed Next: its letter and positions; the zero cursor is a
// first page.
type cursor struct {
	letter byte
	a, b   int
}

// errBadCursor is a cursor the kind does not issue; want is what it does
// issue there, one item per cursor, joined as a set a person reads.
func errBadCursor(raw string, kind gitlet.GitReadKind, want ...string) error {
	return fmt.Errorf("%w: cursor %q is not one a %s read issues (%s): hand back a page's next as it came",
		gitlet.ErrBadCursor, raw, kind, words.Or(want))
}

// parseCursor reads raw as one of letters' cursors, each at its arity; "" is
// the first page.
func parseCursor(raw string, kind gitlet.GitReadKind, letters map[byte]int) (cursor, error) {
	if raw == "" {
		return cursor{}, nil
	}
	var want []string
	for l, n := range letters {
		want = append(want, widestCursor(l, n))
	}
	// IN ONE ORDER, because the letters are a map's and a refusal a person
	// reads twice should read the same.
	slices.Sort(want)
	if len(raw) < 3 || raw[1] != ':' {
		return cursor{}, errBadCursor(raw, kind, want...)
	}
	arity, ok := letters[raw[0]]
	parts := strings.Split(raw[2:], ":")
	if !ok || len(parts) != arity {
		return cursor{}, errBadCursor(raw, kind, want...)
	}
	c := cursor{letter: raw[0]}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		// CANONICAL DIGITS ONLY: a sign, a leading zero or a space is a
		// cursor no node issued, and two spellings of one position would
		// be two pages of one memo.
		if err != nil || n < 1 || strconv.Itoa(n) != p {
			return cursor{}, errBadCursor(raw, kind, want...)
		}
		if i == 0 {
			c.a = n
		} else {
			c.b = n
		}
	}
	return c, nil
}

// formatCursor is c as a Next.
func formatCursor(c cursor) string {
	switch c.letter {
	case 0:
		return ""
	case 'b', 'w':
		return fmt.Sprintf("%c:%d:%d", c.letter, c.a, c.b)
	}
	return fmt.Sprintf("%c:%d", c.letter, c.a)
}

// records is a Cmd.Stdout handing each record git ends with sep to fn as it
// arrives — a NUL-terminated listing (-z) or a line — with no record held
// whole past max: one longer is handed to fn cut to its first max bytes with
// long set, the rest of it dropped, so a caller decides what an overlong
// record means without the stream ever holding it. fn's slice is valid only
// during the call; an error from fn ends the run, as gitproc.Lines' does.
type records struct {
	sep   byte
	limit int
	fn    func(rec []byte, long bool) error
	buf   []byte
	long  bool
}

func newRecords(sep byte, limit int, fn func(rec []byte, long bool) error) *records {
	return &records{sep: sep, limit: limit, fn: fn}
}

func (r *records) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, r.sep)
		if i < 0 {
			r.keep(p)
			break
		}
		r.keep(p[:i])
		if err := r.fn(r.buf, r.long); err != nil {
			return n, err
		}
		r.buf, r.long, p = r.buf[:0], false, p[i+1:]
	}
	return n, nil
}

// keep adds p to the record in hand, up to its limit.
func (r *records) keep(p []byte) {
	if left := r.limit - len(r.buf); len(p) > left {
		p, r.long = p[:max(left, 0)], true
	}
	r.buf = append(r.buf, p...)
}

// close reports a stream that ended inside a record — git ends every record
// it writes — as git's own failure to.
func (r *records) close() error {
	if len(r.buf) > 0 || r.long {
		return errors.New("query: git's output ended inside a record")
	}
	return nil
}
