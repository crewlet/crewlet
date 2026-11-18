package gitproc

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// The sink classifies a run by each error event's FORMAT, never its message
// nor stderr, which carry the client's request text (package doc, "What went
// wrong is classified from git's own format strings").
//
// The stream is untrusted volume, read to its end always: a pipe the sink
// stopped reading would block git's next write until the run's deadline. A
// line past MaxLine is skipped unparsed and counted (Trace.Unread): no damage
// event nears it (the longest measured is 559 bytes), but upload-pack can echo a
// request as one 390 KB event. Only lines beginning with a prefix below are
// decoded, each as ONE JSON value: trace2 escapes quotes and control bytes in
// strings, so request text never begins such a line, and two interleaved
// writes fail the whole-line decode. A bare-"%s" damage is a Hint (hintOf).

// The two event lines the sink decodes, told by their first bytes, so every
// other line costs only a newline scan.
var (
	errorPrefix   = []byte(`{"event":"error",`)
	versionPrefix = []byte(`{"event":"version",`)
)

// maxHints is the most hints one run records: a damaged pack names many
// objects, but a repair confirms hints only until one's pack is not its row's,
// hashing each pack once (cache.Cache.ConfirmHint), so hints past the first
// few name packs already decided.
const maxHints = 16

// maxFormats is the most distinct formats one run records (Trace.Formats): a
// real run reports a few, so the bound only holds an abnormal record to a few
// kilobytes.
const maxFormats = 64

// Trace is what a run's trace2 event stream said, read whole.
type Trace struct {
	// Events is every complete line the stream held; Errors the error
	// events among them that decoded.
	Events, Errors int
	// Unread is the lines skipped: past MaxLine, or beginning as an error or
	// version event and not decoding as one. A request can cause it (an
	// echoed pkt-line); it is never a damage event.
	Unread int
	// Version is the git version the leader's version event names (`exe`),
	// empty for a git that wrote no stream; EventVersion is that event's
	// `evt`, the stream's format (4 on 2.55 and 2.56), logged so a later
	// git's new format is seen.
	Version, EventVersion string
	// Kinds is every kind an event's format classifies as.
	Kinds KindSet
	// Formats is the canonical format of every error event, each once, in
	// the order first met, at most maxFormats, for logs and the suite.
	Formats []string
	// Hints is the packs a bare-"%s" event nominated as holding a corrupt
	// object (hintOf), each once, at most maxHints.
	Hints []Hint
	// Crashes is the error events reporting a process of the run's tree
	// dying of a signal (crashFormats). Never a kind: a crash says something
	// died reading the repository, not what it could not read; Result.Crashed
	// reads it.
	Crashes int
}

// Reported reports whether an error event of the run printed any of formats,
// each as git's source spells it (compared canonically). It lets a caller
// answer one of git's refusals in its own words (a blame's range past the
// file's end) by the format, never the message, which carries what the
// request named.
func (t Trace) Reported(formats ...string) bool {
	for _, f := range formats {
		c := canonicalFormat(f)
		for _, seen := range t.Formats {
			if seen == c {
				return true
			}
		}
	}
	return false
}

// crashFormats are git's reports of a process of the run's tree dying of a
// signal: run-command's wait_or_whine for any child (every git), and 2.56's
// for upload-pack's pack-objects. Both leave out SIGPIPE, a consumer that hung
// up, and run-command SIGINT and SIGQUIT too. Matched by format, never message, though the first's "%s" is only the
// child's argv[0].
var crashFormats = map[string]bool{
	canonicalFormat(`%s died of signal %d`):           true,
	canonicalFormat(`pack-objects died of signal %d`): true,
}

// Hint is a pack git named as holding a corrupt object in words a request
// could reach: the object, and the pack by its checksum (as
// gitlet.PackRef.GitChecksum spells it). It decides nothing until the pack's
// bytes are held to its row.
type Hint struct {
	Object, Pack string
}

// errorEvent is the part of an error or a version event the sink reads.
type errorEvent struct {
	Event string `json:"event"`
	Fmt   string `json:"fmt"`
	Msg   string `json:"msg"`
	Exe   string `json:"exe"`
	Evt   string `json:"evt"`
}

// traceSink reads a run's trace2 event stream as it arrives. Written only by
// the trace copier; read after that copier has ended (finish).
type traceSink struct {
	// dir is the run's Cmd.Dir: the one directory a hint's pack may lie in.
	dir string
	// line is the line in progress, held only while it may be an event the
	// sink decodes and never past MaxLine; skip passes the rest over to its
	// newline once it is not, and long marks one past MaxLine (unread).
	line       []byte
	skip, long bool

	t     Trace
	rank  int
	parts [rankCorrupt + 1]Part
	first [rankCorrupt + 1]struct{ format, message string }
	seen  map[string]bool
}

// newTraceSink is the sink for a run in dir.
func newTraceSink(dir string) *traceSink { return &traceSink{dir: dir} }

// Write reads p, the next bytes of the stream. It never fails: a pipe the
// sink stopped reading would block git.
func (s *traceSink) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.take(p)
			break
		}
		s.take(p[:i])
		s.end()
		p = p[i+1:]
	}
	return n, nil
}

// take adds b, more of the line in progress, keeping it only while it may be
// an event the sink decodes and only to MaxLine.
func (s *traceSink) take(b []byte) {
	if s.skip || len(b) == 0 {
		return
	}
	if len(s.line)+len(b) > MaxLine {
		s.line, s.skip, s.long = s.line[:0], true, true
		return
	}
	s.line = appendBounded(s.line, b)
	if !mayBeRead(s.line, s.t.Version == "") {
		s.line, s.skip = s.line[:0], true
	}
}

// appendBounded is append that never grows dst's capacity past MaxLine: the
// sink holds at most one line of at most MaxLine whatever the stream.
func appendBounded(dst, b []byte) []byte {
	if need := len(dst) + len(b); need > cap(dst) {
		grown := make([]byte, len(dst), min(max(2*cap(dst), need, 256), MaxLine))
		copy(grown, dst)
		dst = grown
	}
	return append(dst, b...)
}

// mayBeRead reports whether line, the start of a line, begins or may yet
// begin an event the sink decodes: an error event, or a version event while
// version (none read yet). Only the FIRST version event is the leader's,
// written before it can start a child, so children's are passed over: one
// decode a run, not one a process.
func mayBeRead(line []byte, version bool) bool {
	return begins(line, errorPrefix) || version && begins(line, versionPrefix)
}

// begins reports whether line begins with prefix, or is a start of it.
func begins(line, prefix []byte) bool {
	if len(line) >= len(prefix) {
		return bytes.HasPrefix(line, prefix)
	}
	return bytes.HasPrefix(prefix, line)
}

// end ends the line in progress.
func (s *traceSink) end() {
	s.t.Events++
	switch {
	case s.long:
		s.t.Unread++
	case !s.skip && (bytes.HasPrefix(s.line, errorPrefix) ||
		s.t.Version == "" && bytes.HasPrefix(s.line, versionPrefix)):
		s.decode(s.line)
	}
	s.line, s.skip, s.long = s.line[:0], false, false
}

// finish reads an unterminated last line (a process killed mid-write) as a
// line. Called once, after the copier has ended.
func (s *traceSink) finish() {
	if len(s.line) > 0 || s.skip {
		s.end()
	}
	s.line = nil
}

// decode reads one whole line as one event: a line that is not one JSON
// value and nothing after it is unread.
func (s *traceSink) decode(line []byte) {
	var ev errorEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		s.t.Unread++
		return
	}
	switch ev.Event {
	case "version":
		if s.t.Version == "" {
			s.t.Version, s.t.EventVersion = ev.Exe, ev.Evt
		}
	case "error":
		s.t.Errors++
		s.classify(ev.Fmt, ev.Msg)
	}
}

// classify reads one error event by its format against the table, and reads
// a hint from a bare "%s" or the corrupt-packed-object row (packedObjectRow).
func (s *traceSink) classify(format, message string) {
	c := canonicalFormat(format)
	if !s.seen[c] && len(s.t.Formats) < maxFormats {
		if s.seen == nil {
			s.seen = map[string]bool{}
		}
		s.seen[c] = true
		s.t.Formats = append(s.t.Formats, c)
	}
	if r, ok := formats[c]; ok {
		rank := rankOf(r.kind)
		s.t.Kinds |= kindBit(rank)
		s.parts[rank] |= r.part
		if s.first[rank].format == "" {
			s.first[rank].format, s.first[rank].message = c, message
		}
		s.rank = max(s.rank, rank)
		if c == packedObjectRow {
			// 2.55's row for 2.56's "%s" sentence names the same pack:
			// a hint too, so the repair need not look for the pack.
			if h, ok := hintOf(message, s.dir); ok {
				s.hint(h)
			}
		}
		return
	}
	if crashFormats[c] {
		s.t.Crashes++
		return
	}
	if c == "%_" {
		if h, ok := hintOf(message, s.dir); ok {
			s.hint(h)
		}
	}
}

// packedObjectRow is the canonical format of git's corrupt-packed-object
// sentence, a row of the table on 2.55 (classify.go) whose message hintOf reads.
var packedObjectRow = canonicalFormat(`packed object %s (stored in %s) is corrupt`)

// hint records h, once, up to maxHints.
func (s *traceSink) hint(h Hint) {
	for _, have := range s.t.Hints {
		if have == h {
			return
		}
	}
	if len(s.t.Hints) < maxHints {
		s.t.Hints = append(s.t.Hints, h)
	}
}

// hintOf is the hint message names when it is EXACTLY "packed object <id>
// (stored in <pack>) is corrupt", nothing before or after, with <pack> a file
// of dir's own pack directory as rev-list spells it
// (dir/objects/pack/pack-<checksum>.pack) or upload-pack does
// (./objects/pack/pack-<checksum>.pack). Anything else names no pack, nor does
// a run with no dir to anchor to: the message may carry request text.
func hintOf(message, dir string) (Hint, bool) {
	if dir == "" {
		return Hint{}, false
	}
	const head, mid, tail = "packed object ", " (stored in ", ") is corrupt"
	rest, ok := strings.CutPrefix(message, head)
	if !ok {
		return Hint{}, false
	}
	object, rest, ok := strings.Cut(rest, mid)
	if !ok || !isObjectID(object) {
		return Hint{}, false
	}
	path, ok := strings.CutSuffix(rest, tail)
	if !ok {
		return Hint{}, false
	}
	var file string
	for _, base := range []string{"./objects/pack/", strings.TrimSuffix(dir, "/") + "/objects/pack/"} {
		if f, found := strings.CutPrefix(path, base); found {
			file = f
			break
		}
	}
	checksum, ok := strings.CutPrefix(file, "pack-")
	if !ok {
		return Hint{}, false
	}
	checksum, ok = strings.CutSuffix(checksum, ".pack")
	if !ok || !isObjectID(checksum) {
		return Hint{}, false
	}
	return Hint{Object: object, Pack: checksum}, true
}

// isObjectID reports whether s is forty lowercase hex digits: a SHA-1 id as
// git prints one.
func isObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; ('0' > c || c > '9') && ('a' > c || c > 'f') {
			return false
		}
	}
	return true
}

// kind is the highest kind any event's format named: ErrFailed for none.
func (s *traceSink) kind() error { return kindOf(s.rank) }

// damage is the run's *Damage when the highest kind named is damage to the
// repository — ErrCorrupt or ErrMissingObject — and nil otherwise
// (Result.Damage).
func (s *traceSink) damage(began time.Time) error {
	if s.rank < rankMissing {
		return nil
	}
	return &Damage{Kind: kindOf(s.rank), Parts: s.parts[s.rank], Format: s.first[s.rank].format,
		Message: s.first[s.rank].message, Began: began}
}

// summary is what the stream said, as a Result carries it.
func (s *traceSink) summary() Trace { return s.t }
