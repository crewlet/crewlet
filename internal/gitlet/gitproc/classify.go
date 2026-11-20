package gitproc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// The four kinds a failed git process is classified into, from the trace2
// error events of its process tree (trace.go).
var (
	// ErrCorrupt: git read a pack, an index, a reverse index, a
	// multi-pack-index, a bitmap or a commit-graph that is damaged.
	ErrCorrupt = errors.New("gitproc: git read a pack, index, multi-pack-index or commit-graph that is damaged")

	// ErrMissingObject: git named an object it could not find.
	ErrMissingObject = errors.New("gitproc: git named an object it could not find")

	// ErrNotOurRef: upload-pack refused a want. NEVER a rebuild trigger: an
	// absent want and a cache that lost a whole pack print the same line, and
	// the next linearizable sync re-downloads any absent pack anyway.
	ErrNotOurRef = errors.New("gitproc: upload-pack refused a want this repository does not serve")

	// ErrFailed: git failed for a reason its reports do not classify, or
	// with no report at all.
	ErrFailed = errors.New("gitproc: git failed for a reason its reports do not classify")
)

// ExitError is a git process that exited with a status that is not one of its
// command's answers ([Cmd.Answers]). Kind is the HIGHEST of [ErrCorrupt],
// [ErrMissingObject], [ErrNotOurRef] and [ErrFailed] the run's reports named;
// errors.Is reaches it AND EVERY OTHER KIND THE RUN NAMED ([Trace.Kinds]), and
// a caller asking about several asks the highest first (package doc, "What
// went wrong is classified from git's own format strings"). errors.As reaches
// the run's [*Damage] where it named one.
//
// THE KIND DESCRIBES THE REPOSITORY THE PROCESS READ, which only the caller
// knows: a receive-pack's index-pack reads the CLIENT's pack, so its "early
// EOF" is about that pack, not the cache. ErrCorrupt and ErrMissingObject are
// a rebuild trigger only from a process whose Cmd.Dir is a cache.
type ExitError struct {
	Command string // Args[0]
	Result  Result
	Kind    error
}

// Error is "gitproc: git <command> exited <status> — <kind>: <its last line>",
// and, when the run named damage, "; git named: <its message>": the last line
// of a fetch that met damage is upload-pack's "aborting due to possible
// repository corruption", and the useful words are its pack-objects'.
func (e *ExitError) Error() string {
	ended := "exited " + strconv.Itoa(e.Result.Exit)
	if e.Result.Exit < 0 {
		ended = "was ended by a signal"
	}
	msg := fmt.Sprintf("gitproc: git %s %s — %s", e.Command, ended, strings.TrimPrefix(e.Kind.Error(), "gitproc: "))
	if last := e.Result.LastLine(); last != "" {
		msg += ": " + textcut.Ellipsis(last, 512)
	}
	var d *Damage
	if errors.As(e.Result.Damage, &d) {
		msg += "; git named: " + textcut.Ellipsis(d.Message, 512)
	}
	return msg
}

// Unwrap is the kind and, when the run named damage, the [*Damage], so
// errors.Is(err, ErrCorrupt) and its siblings hold and errors.As reaches the
// parts the damage named.
func (e *ExitError) Unwrap() []error {
	if e.Result.Damage != nil {
		return []error{e.Kind, e.Result.Damage}
	}
	return []error{e.Kind}
}

// Is reports whether target is one of the kinds the run named
// ([Trace.Kinds]), the highest among them its Kind, which Unwrap reaches as
// well.
func (e *ExitError) Is(target error) bool { return e.Result.Trace.Kinds.Has(target) }

// LastLine is the last non-empty line of Stderr, trimmed: what a message
// about the run quotes of git's own words.
func (r Result) LastLine() string {
	s := strings.TrimRightFunc(r.Stderr, unicode.IsSpace)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// KindSet is the kinds a run's reports named — [ErrCorrupt],
// [ErrMissingObject] and [ErrNotOurRef], each once however many reports named
// it. [ErrFailed] is no member: it is what a run that named none is.
type KindSet uint8

// Has reports whether kind is one the set holds.
func (s KindSet) Has(kind error) bool {
	for rank := rankNotOurRef; rank <= rankCorrupt; rank++ {
		if errors.Is(kind, kindOf(rank)) {
			return s&kindBit(rank) != 0
		}
	}
	return false
}

// The ranks of the kinds: the HIGHEST NAMED BY ANY REPORT WINS. Corrupt
// outranks Missing outranks NotOurRef because damage explains the other two: a
// truncated .idx makes upload-pack report "index file … is too small" and then
// "not our ref" for the tip it can no longer find, and that is a damaged cache,
// not an absent want.
const (
	rankNone = iota
	rankNotOurRef
	rankMissing
	rankCorrupt
)

// kindOf is the error a rank stands for.
func kindOf(rank int) error {
	switch rank {
	case rankCorrupt:
		return ErrCorrupt
	case rankMissing:
		return ErrMissingObject
	case rankNotOurRef:
		return ErrNotOurRef
	}
	return ErrFailed
}

// rankOf is the rank of a kind; rankNone for ErrFailed or anything else.
func rankOf(kind error) int {
	for rank := rankNotOurRef; rank <= rankCorrupt; rank++ {
		if errors.Is(kind, kindOf(rank)) {
			return rank
		}
	}
	return rankNone
}

// kindBit is the member of a KindSet a rank stands for; none for rankNone.
func kindBit(rank int) KindSet {
	if rank <= rankNone {
		return 0
	}
	return 1 << (rank - 1)
}

// Part is the file of a repository a damage report named: what a repair must
// read again or drop. A set — a run can name several.
type Part uint8

const (
	// PartPack is a pack, its index or its reverse index: the stored bytes
	// of a push, which only a read of the pack again repairs.
	PartPack Part = 1 << iota
	// PartObject is an object git looked for and did not find.
	PartObject
	// PartMultiPackIndex is the multi-pack-index, its bitmap and its
	// reverse index: derived by the node from its packs.
	PartMultiPackIndex
	// PartCommitGraph is the commit-graph: derived by the node from its
	// packs.
	PartCommitGraph
	// PartChunk is a chunk-format file whose damage git reports without
	// naming it: the multi-pack-index OR the commit-graph, either of which
	// prints these reports.
	PartChunk
)

// derivedParts are the parts a node derives from its packs and so drops in
// place to repair.
const derivedParts = PartMultiPackIndex | PartCommitGraph | PartChunk

// String is the parts' names, joined by "+": what a log line says went.
func (p Part) String() string {
	var names []string
	for _, n := range []struct {
		part Part
		name string
	}{{PartPack, "pack"}, {PartObject, "object"}, {PartMultiPackIndex, "multi-pack-index"},
		{PartCommitGraph, "commit-graph"}, {PartChunk, "chunk-format file"}} {
		if p&n.part != 0 {
			names = append(names, n.name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, "+")
}

// Damage is what a run's trace2 error events named about the repository it
// read: the highest kind any event's format classifies as, every part the
// events of that kind named, and the first such event's canonical format and
// its message. The message is for errors and logs ONLY, never matched: a
// request's text lands there. Began is when the naming process began
// ([Result.Began]), carried wherever the damage goes.
type Damage struct {
	Kind    error // ErrCorrupt or ErrMissingObject
	Parts   Part
	Format  string
	Message string
	Began   time.Time
}

// Error is "gitproc: <kind>: <the message, at most 512 bytes>".
func (d *Damage) Error() string {
	return strings.TrimSuffix(d.Kind.Error(), ".") + ": " + textcut.Ellipsis(d.Message, 512)
}

// Unwrap is the kind.
func (d *Damage) Unwrap() error { return d.Kind }

// Derived reports whether every part d names is one the node derives from
// its packs and drops in place — [PartMultiPackIndex], [PartCommitGraph] and
// [PartChunk] — and nothing else.
func (d *Damage) Derived() bool {
	return errors.Is(d.Kind, ErrCorrupt) && d.Parts != 0 && d.Parts&^derivedParts == 0
}

// Drops is the derived files a repair of d drops: the index's for
// [PartMultiPackIndex], the graph for [PartCommitGraph], and BOTH for
// [PartChunk] — its message names neither, and both are chunk-format files.
// Always a subset of PartMultiPackIndex|PartCommitGraph.
func (d *Damage) Drops() Part {
	var drops Part
	if d.Parts&(PartMultiPackIndex|PartChunk) != 0 {
		drops |= PartMultiPackIndex
	}
	if d.Parts&(PartCommitGraph|PartChunk) != 0 {
		drops |= PartCommitGraph
	}
	return drops
}

// Narrow is d as named by a process that loads, of the two chunk-format
// files, only those loads names ([PartMultiPackIndex], [PartCommitGraph]): a
// chunk-format report, which names neither file and so reads as both
// ([Damage.Drops]), is then the one file that process loaded. Only the caller
// knows its input: a cat-file asked about object ids alone (the cache's wants
// check) never loads the commit-graph, and reading its chunk report as both
// would drop a sound graph and slow every walk until the maintainer rewrites
// it; asked to PEEL a tag ("<id>^{}"), it does load the graph. d itself when
// loads names both files or neither, or d names no chunk.
func (d *Damage) Narrow(loads Part) *Damage {
	only := loads & (PartMultiPackIndex | PartCommitGraph)
	if d == nil || d.Parts&PartChunk == 0 || only == 0 || only == PartMultiPackIndex|PartCommitGraph {
		return d
	}
	n := *d
	n.Parts = d.Parts&^PartChunk | only
	return &n
}

// row is what one of git's report formats classifies as.
type row struct {
	kind error
	part Part
}

// formatTable is every format git reports a repository file or object it
// failed to read with, AS GIT SPELLS IT, by what it classifies as: exactly the
// formats classify_test.go provokes from real damaged repositories, held both
// ways on the node pin and floor (TestTheFormatTableIsWhatGitSays). A version
// comment names the gits that print a row; every other row is printed by every
// git from [NodeMinVersion], and a row only older gits print is deleted: no
// pool meets it and no run can check it. A row has words of git's own, never
// bare conversions (`%s`), a format a request makes upload-pack print, or one
// a HEALTHY repository prints (`unable to find all commit-graph files` from a
// reader racing a split write; TestNoRowIsAFormatARequestWrites). Formats read
// from git's source and left out, with why: package doc, "Formats left out of
// the table".
var formatTable = map[row][]string{
	{ErrCorrupt, PartPack}: {
		`index file %s is too small`,
		`packfile %s does not match index`,
		`inflate: %s (%s)`,
		`packed object %s (stored in %s) is corrupt`, // 2.55: 2.56 reports it through "%s" (trace.go's hint)
		`%s pack checksum mismatch`,
		`index CRC mismatch for object %s from %s at offset %lu`,
		`cannot unpack %s from %s at offset %lu`,
		`pack has bad object at offset %lu: %s`,
		`early EOF`,
		`unknown object type %i at offset %lu in %s`,
		`offset beyond end of packfile (truncated pack?)`,
		`bad packed object CRC for %s`,
		`failed to unpack compressed delta at offset %lu from %s`,
		`failed to read delta base object %s at offset %lu from %s`,
		`pack has %d unresolved delta`,
		`pack has %d unresolved deltas`,
		// A pack's reverse index, which git reads around (it builds one in
		// memory) and reports.
		`reverse-index file %s is too small`,
		`reverse-index file %s is corrupt`,
		`reverse-index file %s has unknown signature`,
		`reverse-index file %s has unsupported version %u`,
		`reverse-index file %s has unsupported hash id %u`,
		// pack-objects where a pack's .idx or .rev place an object at bytes
		// that are not its entry. Its reuse of a stored entry reads the
		// pack's own indexes, which a multi-pack-index answers every other
		// lookup past, so on a copy holding one a damaged .idx is named only
		// here.
		`bad offset for revindex`,
		`write_reuse_object: could not locate %s, expected at offset %lu in pack %s`,
		// The bytes git read for an object are not its name: the pack, its
		// index or the multi-pack-index placed it there, and only the bytes
		// say which (the cache's scrub).
		`hash mismatch %s`,
	},
	{ErrCorrupt, PartMultiPackIndex}: {
		`multi-pack-index signature 0x%08x does not match signature 0x%08x`,
		`multi-pack-index version %d not recognized`,
		`multi-pack-index hash version %u does not match version %u`,
		`multi-pack-index file %s is too small`,
		`invalid MIDX object position, MIDX is likely corrupt`,
		`multi-pack-index required pack-name chunk missing or corrupted`,
		`multi-pack-index required OID fanout chunk missing or corrupted`,
		`multi-pack-index required OID lookup chunk missing or corrupted`,
		`multi-pack-index required object offsets chunk missing or corrupted`,
		`multi-pack-index OID fanout is of the wrong size`, // beside the required-fanout row
		`multi-pack-index pack names out of order: '%s' before '%s'`,
		`oid fanout out of order: fanout[%d] = %x > %x = fanout[%d]`, // beside the required-fanout row
		// The maintainer's write over an index whose checksum fails: git
		// writes on, keeping what it could read. The one damaged-index
		// report a successful run makes.
		`ignoring existing multi-pack-index; checksum mismatch`,
		// The index's bitmap, dropped with it.
		`corrupted bitmap index file (wrong header)`,
		`corrupted bitmap index (too small)`,
		`unsupported version '%d' for bitmap index file`,
	},
	{ErrCorrupt, PartCommitGraph}: {
		`commit-graph signature %X does not match signature %X`,
		`commit-graph file is too small`,
		`commit-graph version %X does not match version %X`,
		`commit-graph hash version %X does not match version %X`,
		`commit-graph oid fanout chunk is wrong size`,
		`commit-graph required OID fanout chunk missing or corrupted`,
		`commit-graph fanout values out of order`,
		`invalid parent position %u`,                                  // fatal
		`commit-graph requires overflow generation data but has none`, // fatal
	},
	{ErrCorrupt, PartChunk}: {
		`final chunk has non-zero id %x`,
		`terminating chunk id appears earlier than expected`,
		`improper chunk offset(s) %lx and %lx`,
		`chunk id %x not %d-byte aligned`,
		`duplicate chunk ID %x found`,
	},
	{ErrMissingObject, PartObject}: {
		`bad object %s`,
		`git cat-file: could not get object info`,
		`Not a valid commit name %s`,
		`Not a valid object name %s`,
		`%s: invalid sha1 pointer %s`,
		`unable to read %s`,
		`missing %s object '%s'`,
		`Could not read %s`,
		`Failed to traverse parents of commit %s`,
		`did not receive expected object %s`,
	},
	{ErrNotOurRef, 0}: {
		`git upload-pack: not our ref %s`,
	},
}

// formats is formatTable keyed by each format's canonical form
// (canonicalFormat), which is what an event is looked up by.
var formats = func() map[string]row {
	out := map[string]row{}
	for r, list := range formatTable {
		for _, f := range list {
			out[canonicalFormat(f)] = r
		}
	}
	return out
}()

// canonicalFormat is format with every printf conversion specification — a
// '%', its flags, width, precision and length, and the conversion — written
// "%_", and every "%%" kept: so one row matches a format whatever length a
// platform's PRIuMAX expands to ("%lu" on Linux, "%ju" on macOS), and
// whatever width a later git gives a hex field.
func canonicalFormat(format string) string {
	if strings.IndexByte(format, '%') < 0 {
		return format
	}
	var b strings.Builder
	b.Grow(len(format))
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+1 < len(format) && format[i+1] == '%' {
			b.WriteString("%%")
			i++
			continue
		}
		j := i + 1
		for j < len(format) && strings.IndexByte("-+ #0'", format[j]) >= 0 {
			j++
		}
		for j < len(format) && (format[j] == '*' || ('0' <= format[j] && format[j] <= '9')) {
			j++
		}
		if j < len(format) && format[j] == '.' {
			j++
			for j < len(format) && (format[j] == '*' || ('0' <= format[j] && format[j] <= '9')) {
				j++
			}
		}
		for j < len(format) && strings.IndexByte("hlLqjzt", format[j]) >= 0 {
			j++
		}
		if j < len(format) && strings.IndexByte("diouxXeEfFgGaAcspn", format[j]) >= 0 {
			b.WriteString("%_")
			i = j
			continue
		}
		b.WriteByte(c) // a '%' that begins no conversion is git's text
	}
	return b.String()
}

// stderrSink reads git's stderr as it arrives: it counts every byte (the
// stall watchdog's third term — a receive-pack relaying its hook's keepalives
// on stderr is moving) and keeps the tail a Result carries. It classifies
// nothing: a request's text lands in stderr (package doc, "What went wrong is
// classified from git's own format strings").
//
// Written only by the stderr copier; the tail is read after that copier has
// finished, so only the counter is shared, and it is atomic.
type stderrSink struct {
	n       atomic.Int64
	pending []byte
	kept    keptTail
	// known takes a remote run's secrets out of each line AS IT ARRIVES
	// (Remote.known), or is nil: before the tail is cut, so no cut splits a
	// secret. A secret holds no line break (Remote.WithHeader), so a line
	// holds it whole, but for a line past MaxLine, kept in pieces, across
	// whose cut only the secret's own remote could have written it.
	known *strings.Replacer
	// redacting is a remote run's way into the tail, or nil: the tail is
	// then the end of what redact.Secrets makes of ALL git wrote, cut after
	// the redaction. Cut first, the cut could split what a rule reads (a key
	// from its value, a token in two) and leave the rest unread. refused is
	// the stream's refusal, after which nothing it wrote is shown. A
	// [redact.Stream] in every run, a test's own where one refuses.
	redacting io.WriteCloser
	refused   error
}

// newStderrSink is the sink of a run whose remote, if any, has the secrets
// known; a remote run's text is from outside, so it is redacted ([stderrSink.redacting]).
func newStderrSink(remote bool, known *strings.Replacer) *stderrSink {
	s := &stderrSink{known: known}
	if remote {
		s.redacting = redact.NewStream(&s.kept)
	}
	return s
}

func (s *stderrSink) Write(p []byte) (int, error) {
	s.n.Add(int64(len(p)))
	written := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.pending = append(s.pending, p...)
			// A line longer than any git writes is kept as one and dropped
			// from the buffer, so a process writing without newlines
			// cannot grow it without bound.
			if len(s.pending) > MaxLine {
				s.piece(s.pending)
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

// line appends one line to the tail.
func (s *stderrSink) line(l []byte) {
	s.piece(l)
	s.keep([]byte{'\n'})
}

// piece appends part of a line, its secrets taken out where it has a remote:
// a line past MaxLine goes on in the next, unbroken, so the redaction reads it
// as git wrote it.
func (s *stderrSink) piece(l []byte) {
	if s.known != nil {
		l = []byte(s.known.Replace(string(l)))
	}
	s.keep(l)
}

// keep hands b to the tail, through the redaction where there is one.
func (s *stderrSink) keep(b []byte) {
	switch {
	case s.redacting == nil:
		_, _ = s.kept.Write(b)
	case s.refused == nil:
		_, s.refused = s.redacting.Write(b)
	}
}

// finish takes a last line with no newline as a line, and ends the
// redaction. Called once, after the copier has ended.
func (s *stderrSink) finish() {
	if len(s.pending) > 0 {
		s.line(s.pending)
		s.pending = nil
	}
	if s.redacting != nil && s.refused == nil {
		s.refused = s.redacting.Close()
	}
}

// withheldStderr is a remote run's tail where its redaction refused the text
// ([redact.ErrDecidedEarly]): what the stream wrote may hold what redaction
// takes out, so none of it is shown.
const withheldStderr = "gitproc: the remote's error output is withheld: its redaction was refused\n"

// tail is the last StderrTail bytes git wrote, redacted first for a remote
// run, marked where it was cut — including a cut the buffer's own trimming
// made, which leaves fewer bytes than textcut would mark on its own.
func (s *stderrSink) tail() string {
	if s.refused != nil {
		return withheldStderr
	}
	if s.kept.cut && len(s.kept.b) <= StderrTail {
		return "…" + string(s.kept.b)
	}
	return textcut.Tail(string(s.kept.b), StderrTail)
}

// keptTail is at least the last StderrTail bytes written to it, kept to at
// most twice that so the copy it trims to is amortised; cut says it dropped
// bytes from its front.
type keptTail struct {
	b   []byte
	cut bool
}

func (k *keptTail) Write(p []byte) (int, error) {
	k.b = append(k.b, p...)
	if len(k.b) > 2*StderrTail {
		start := len(k.b) - StderrTail
		for start < len(k.b) && !utf8.RuneStart(k.b[start]) {
			start++
		}
		k.b = append(k.b[:0], k.b[start:]...)
		k.cut = true
	}
	return len(p), nil
}
