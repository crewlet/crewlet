package query

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
)

// A PAGE IS MEASURED AS ITS JSON, because what bounds it is the bytes of the
// answer the asker reads (code.MaxGitAnswerBytes), and text grows on its way
// into JSON: a quote or a backslash is two bytes there, a control character
// six, every `<`, `>` and `&` six (encoding/json escapes them for HTML), and
// every byte that is not UTF-8 six. Counting raw bytes, a page of a file full
// of `<` would encode at six times its bound. So every item a page takes is
// costed in the encoder's own terms — a tree entry or a diff file by encoding
// it, text by the per-rune table below, which init builds FROM encoding/json
// itself so the two cannot disagree — against the room an answer has once its
// envelope is spent.

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

// jsonLen is v's encoded bytes.
func jsonLen(v any) int {
	out, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("query: encoding an answer: %v", err))
	}
	return len(out)
}

// room is what a page's items may take in the read's answer whose envelope —
// the answer with no items, every number and the cursor at their longest — is
// envelope. THE FIELDS EVERY ANSWER CARRIES are filled here, the repository
// and the position Read stamps on whatever the kind answered, the push that
// introduced each commit it names with the coverage of the index that says so
// (step 9, decorated before any page is cut), and Memo set — the page may be
// read from the memo — so no kind can measure its envelope without them and
// cut a page that its stamp then pushes past the bound.
func (r *read) room(envelope code.GitAnswer) int {
	envelope.RepoID, envelope.Position = r.q.RepoID, r.synced.Position
	envelope.PushedBy, envelope.Coverage, envelope.Memo = r.pushedBy, r.coverage, true
	return r.s.pageBytes - jsonLen(envelope)
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
// n-th file of a diff's stat, "l:<n>" line n of a file, "b:<n>:<o>" byte o of
// line n, "p:<o>" byte o of a patch. Each is a position in an answer that is
// a function of object ids alone — the same listing, file or patch on every
// node at every position — so a cursor any node issued is sound on any other.

// cursor is a parsed Next: its letter and positions; the zero cursor is a
// first page.
type cursor struct {
	letter byte
	a, b   int
}

// errBadCursor is a cursor the kind does not issue.
func errBadCursor(raw string, kind code.GitReadKind, want string) error {
	return fmt.Errorf("%w: cursor %q is not one a %s read issues (%s): hand back a page's next as it came",
		code.ErrBadCursor, raw, kind, want)
}

// parseCursor reads raw as one of letters' cursors, each at its arity; "" is
// the first page.
func parseCursor(raw string, kind code.GitReadKind, letters map[byte]int) (cursor, error) {
	if raw == "" {
		return cursor{}, nil
	}
	var want []string
	for l, n := range letters {
		want = append(want, widestCursor(l, n))
	}
	if len(raw) < 3 || raw[1] != ':' {
		return cursor{}, errBadCursor(raw, kind, strings.Join(want, " or "))
	}
	arity, ok := letters[raw[0]]
	parts := strings.Split(raw[2:], ":")
	if !ok || len(parts) != arity {
		return cursor{}, errBadCursor(raw, kind, strings.Join(want, " or "))
	}
	c := cursor{letter: raw[0]}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		// CANONICAL DIGITS ONLY: a sign, a leading zero or a space is a
		// cursor no node issued, and two spellings of one position would
		// be two pages of one memo.
		if err != nil || n < 1 || strconv.Itoa(n) != p {
			return cursor{}, errBadCursor(raw, kind, strings.Join(want, " or "))
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
	case 'b':
		return fmt.Sprintf("b:%d:%d", c.a, c.b)
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
