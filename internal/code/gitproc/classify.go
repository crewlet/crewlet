package gitproc

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/textcut"
)

// The four kinds a failed git process is classified into, from its own stderr.
var (
	// ErrCorrupt: git read a pack, an index, a reverse index, a
	// multi-pack-index or a commit-graph that is damaged.
	ErrCorrupt = errors.New("gitproc: git read a pack, index or multi-pack-index that is damaged")

	// ErrMissingObject: git named an object it could not find.
	ErrMissingObject = errors.New("gitproc: git named an object it could not find")

	// ErrNotOurRef: upload-pack refused a want. NEVER a rebuild trigger: an
	// absent want and a cache that lost a whole pack print the same line, so
	// it can never tell them apart — and the next linearizable sync
	// re-downloads any pack whose file is absent anyway.
	ErrNotOurRef = errors.New("gitproc: upload-pack refused a want this repository does not serve")

	// ErrFailed: git failed for a reason its message does not classify, or
	// with no message at all.
	ErrFailed = errors.New("gitproc: git failed for a reason its message does not classify")
)

// ExitError is a git process that exited with a status that is not one of its
// command's answers ([Cmd.Answers]). Kind is exactly one of [ErrCorrupt],
// [ErrMissingObject], [ErrNotOurRef] and [ErrFailed]; errors.Is reaches it.
//
// THE KIND DESCRIBES THE REPOSITORY THE PROCESS READ, and only the caller
// knows which that was. A receive-pack runs in a scratch and its index-pack
// reads the CLIENT's pack, so a body that ended short makes it print "early
// EOF" — ErrCorrupt, correctly (that pack is damaged), and nothing about the
// cache. ErrCorrupt and ErrMissingObject are a rebuild trigger only from a
// process whose Cmd.Dir is a cache.
type ExitError struct {
	Command string // Args[0]
	Result  Result
	Kind    error
}

// Error is "gitproc: git <command> exited <status> — <kind>: <its last line>".
func (e *ExitError) Error() string {
	ended := "exited " + strconv.Itoa(e.Result.Exit)
	if e.Result.Exit < 0 {
		ended = "was ended by a signal"
	}
	msg := fmt.Sprintf("gitproc: git %s %s — %s", e.Command, ended, strings.TrimPrefix(e.Kind.Error(), "gitproc: "))
	if last := e.Result.LastLine(); last != "" {
		msg += ": " + textcut.Ellipsis(last, 512)
	}
	return msg
}

// Unwrap is the kind, so errors.Is(err, ErrCorrupt) and its siblings hold.
func (e *ExitError) Unwrap() error { return e.Kind }

// LastLine is the last non-empty line of Stderr, trimmed: what a message
// about the run quotes of git's own words.
func (r Result) LastLine() string {
	s := strings.TrimRightFunc(r.Stderr, unicode.IsSpace)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// kinds is the classification table, in rank order: the HIGHEST RANK SEEN ON
// ANY LINE WINS. Every pattern is a message git prints (its own format
// strings, and the lines classify_test.go provokes from real damaged
// repositories), matched against one stderr line under LC_ALL=C — 2.43's
// messages, and the one a multi-pack-index's object-position check prints
// from 2.47.
//
// Corrupt outranks Missing outranks NotOurRef because damage explains the
// other two: a truncated .idx makes upload-pack print "index file … is too
// small" and then "not our ref" for the tip it can no longer find, and that is
// a damaged cache, not an absent want.
//
// THE MULTI-PACK-INDEX'S OBJECT POSITIONS: git from 2.47 checks every
// position it reads through a multi-pack-index — the reverse index its bitmap
// is read by among them — and dies "invalid MIDX object position, MIDX is
// likely corrupt" on one past its objects, naming neither the file nor the
// word multi-pack-index. A bitmapped walk over a damaged reverse index prints
// nothing else, and pack-objects under upload-pack prints it on every clone
// that uses the bitmap, so without it a damaged index was answered as git's
// own failure on every fetch and never repaired. Both halves are patterns, so
// a git that rewords one still names the damage by the other. (git before
// 2.47 has no such check: the same walk dies of a segmentation fault, which
// says nothing a table can read.)
var kinds = []struct {
	kind     error
	patterns []string
	re       *regexp.Regexp
}{
	{kind: ErrCorrupt, patterns: append([]string{
		`is corrupt`,
		`bad packed object`,
		`does not match index`,
		`index file .* is too small`,
		`unable to read index`,
		`inflate`,
		`pack has bad object`,
		`pack has [0-9]+ unresolved delta`,
		`early EOF`,
		`pack checksum mismatch`,
		`CRC mismatch`,
		`cannot unpack`,
		`^(error|fatal): .*commit-graph`,
	}, midxPatterns...)},
	{kind: ErrMissingObject, patterns: []string{
		`bad object [0-9a-f]{40}`,
		`missing (blob|tree|commit|tag) [0-9a-f]{40}`,
		`invalid sha1 pointer [0-9a-f]{40}`,
		`broken link from`,
		`(unable to|Could not) read [0-9a-f]{40}`,
		`Not a valid (commit|object) name [0-9a-f]{40}`,
		`could not get object info`,
		`Failed to traverse parents of commit [0-9a-f]{40}`,
		`did not receive expected object [0-9a-f]{40}`,
	}},
	{kind: ErrNotOurRef, patterns: []string{
		`not our ref [0-9a-f]{40}`,
	}},
}

// midxPatterns are the [ErrCorrupt] patterns that name the MULTI-PACK-INDEX —
// its object-position check (git 2.47+), its signature or any other error git
// prints about it. They are their own list because the host reads them apart
// ([NamesMultiPackIndex]): a damaged multi-pack-index is a DERIVED file, so a
// fetch that meets one is repaired IN PLACE by dropping and rewriting it over
// the same packs, where other corruption needs the whole repository read
// again. Each is one line, each half a pattern, so a rewording of either half
// still names the damage.
var midxPatterns = []string{
	`MIDX is likely corrupt`,
	`invalid MIDX object position`,
	`^(error|fatal): .*multi-pack-index`,
}

// midxRe matches a line naming the multi-pack-index, compiled once.
var midxRe = regexp.MustCompile(strings.Join(midxPatterns, "|"))

// NamesMultiPackIndex reports whether git's line names the multi-pack-index —
// a derived file the cache drops and rewrites in place rather than reading the
// whole repository again. The caller has already classified the line as
// [ErrCorrupt]; this says which kind of corruption it is.
func NamesMultiPackIndex(line string) bool {
	if len(line) > MaxLine {
		line = line[:MaxLine]
	}
	return midxRe.MatchString(line)
}

// Each kind's patterns are ONE alternation compiled once, so a line costs at
// most three regexp runs whatever the table grows to.
func init() {
	for i := range kinds {
		kinds[i].re = regexp.MustCompile(strings.Join(kinds[i].patterns, "|"))
	}
}

// classifyLine is the rank of the highest kind line matches: len(kinds) for
// the first entry down to 1 for the last, 0 for none.
func classifyLine(line []byte) int {
	if len(line) > MaxLine {
		line = line[:MaxLine]
	}
	for i, k := range kinds {
		if k.re.Match(line) {
			return len(kinds) - i
		}
	}
	return 0
}

// kindOf is the error a rank stands for.
func kindOf(rank int) error {
	if rank <= 0 {
		return ErrFailed
	}
	return kinds[len(kinds)-rank].kind
}

// Words classifies git's own words read SOMEWHERE OTHER THAN A PROCESS'S
// STDERR — the text upload-pack relays from its pack-objects on side band 2
// and 3 of its answer, which is where a fetch's damage is named, upload-pack's
// own stderr then saying only that pack-objects died — by the table a run's
// stderr is read by, the highest kind on any line winning. A run's stderr is
// read through one too (stderrSink), so there is one reading of what git says,
// wherever it says it. The zero value has read nothing; it is not safe for
// concurrent use.
type Words struct {
	rank  int
	named string // the first line read at rank
}

// Add classifies one line, without its line ending — a line past MaxLine by
// its first MaxLine bytes, as a stderr line is.
func (w *Words) Add(line []byte) {
	if len(line) > MaxLine {
		line = line[:MaxLine]
	}
	if r := classifyLine(line); r > w.rank {
		w.rank, w.named = r, string(line)
	}
}

// Kind is the highest kind read: [ErrFailed] when no line classified.
func (w *Words) Kind() error { return kindOf(w.rank) }

// Damage is [Words.Kind] when it is damage to the repository the words are
// about — [ErrCorrupt] or [ErrMissingObject] — and nil otherwise.
func (w *Words) Damage() error {
	if k := w.Kind(); errors.Is(k, ErrCorrupt) || errors.Is(k, ErrMissingObject) {
		return k
	}
	return nil
}

// Named is the first line read of [Words.Kind] — the words that named it —
// and "" when no line classified.
func (w *Words) Named() string { return w.named }

// stderrSink reads git's stderr as it arrives: it counts every byte (the
// stall watchdog's third term — a receive-pack relaying its hook's keepalives
// on stderr is moving), classifies every complete line, and keeps the tail a
// Result carries.
//
// Written only by the stderr copier; kind and tail are read after that copier
// has finished, so only the counter is shared, and it is atomic.
type stderrSink struct {
	n       atomic.Int64
	pending []byte
	tailBuf []byte
	cut     bool // the tail buffer has dropped bytes from its front
	words   Words
}

func (s *stderrSink) Write(p []byte) (int, error) {
	s.n.Add(int64(len(p)))
	written := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.pending = append(s.pending, p...)
			// A line longer than any git writes is classified as one and
			// dropped from the buffer, so a process writing without
			// newlines cannot grow it without bound.
			if len(s.pending) > MaxLine {
				s.line(s.pending)
				s.pending = s.pending[:0]
			}
			break
		}
		if len(s.pending) > 0 {
			s.pending = append(s.pending, p[:i]...)
			s.line(s.pending)
			s.pending = s.pending[:0]
		} else {
			s.line(p[:i])
		}
		p = p[i+1:]
	}
	return written, nil
}

// line classifies one line and appends it to the tail, kept to at most
// twice StderrTail so the copy it trims to is amortised.
func (s *stderrSink) line(l []byte) {
	s.words.Add(l)
	s.tailBuf = append(s.tailBuf, l...)
	s.tailBuf = append(s.tailBuf, '\n')
	if len(s.tailBuf) > 2*StderrTail {
		start := len(s.tailBuf) - StderrTail
		for start < len(s.tailBuf) && !utf8.RuneStart(s.tailBuf[start]) {
			start++
		}
		s.tailBuf = append(s.tailBuf[:0], s.tailBuf[start:]...)
		s.cut = true
	}
}

// finish takes a last line with no newline as a line. Called once, after the
// copier has ended.
func (s *stderrSink) finish() {
	if len(s.pending) > 0 {
		s.line(s.pending)
		s.pending = nil
	}
}

func (s *stderrSink) kind() error { return s.words.Kind() }

// damage is the kind when it is damage to the repository — ErrCorrupt or
// ErrMissingObject — and nil otherwise (Result.Damage).
func (s *stderrSink) damage() error { return s.words.Damage() }

// tail is the last StderrTail bytes git wrote, marked where it was cut —
// including a cut the buffer's own trimming made, which leaves fewer bytes
// than textcut would mark on its own.
func (s *stderrSink) tail() string {
	if s.cut && len(s.tailBuf) <= StderrTail {
		return "…" + string(s.tailBuf)
	}
	return textcut.Tail(string(s.tailBuf), StderrTail)
}
