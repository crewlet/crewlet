package gitproc

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// WHAT A GIT PROCESS SAYS WENT WRONG IS READ FROM ITS TRACE2 FORMAT STRINGS.
//
// Every process the pool runs writes git's trace2 EVENT stream to a pipe the
// pool gives it as fd 3 (GIT_TRACE2_EVENT=3; brief, nesting 1 — gitproc.go's
// base environment), and so does every process it starts — upload-pack's
// pack-objects, receive-pack's index-pack and rev-list, an alternate's
// for-each-ref — since git passes the environment and the descriptor on. Each
// of git's reports (die, error, warning, BUG) is one `error` event carrying
// the report's FORMAT (`fmt`) — the constant git passed, the one its source
// fixes — beside the formatted message (`msg`). The classification reads the
// format and NEVER the message, nor stderr: upload-pack copies the CLIENT's
// request text into the messages it dies with ("unexpected line: '<arg>'", a
// filter spec, an unparsed want, newlines and all), so a reading of the words
// let any token that may fetch force a full re-download of a healthy cache.
// The request's text lands in an event's arguments only; every format a
// request was measured to reach (74 forms on four gits) is a constant of
// git's that names no damage.
//
// The stream is untrusted VOLUME and is read to its end, always: a pipe the
// sink stopped reading would block git's next write until the run's deadline.
// A line longer than MaxLine is skipped whole, unparsed, and counted
// (Trace.Unread) — no damage event comes near it (the longest measured is
// 559 bytes, bounded by its format, object ids and the cache's own paths),
// while a request can make upload-pack echo one pkt-line as a single event of
// 390 KB. Only lines beginning with the two prefixes below are decoded, each
// as ONE JSON value with nothing after it; trace2 escapes every quote and
// control byte inside a string, so a fragment of request text can never
// begin such a line, and a line one escaped write interleaved with another
// fails the whole-line decode.
//
// A DAMAGE GIT NAMES ONLY THROUGH A BARE "%s" is a HINT, never a kind: git
// 2.56 reports a corrupt packed object by formatting the sentence into a
// buffer and dying with die("%s", buf), and sometimes (a header whose size is
// off) with no other report beside it. The sink recognises exactly one
// sentence in such an event, anchored at both ends and to the run's own pack
// directory, and records the pack it nominates (Trace.Hints); what decides
// is that pack's bytes against its row (cache.Cache.ConfirmHint), never the
// message.

// The two event lines the sink decodes: a constant check of a line's first
// bytes, so every other line costs a newline scan.
var (
	errorPrefix   = []byte(`{"event":"error",`)
	versionPrefix = []byte(`{"event":"version",`)
)

// maxHints is the most hints one run records: a damaged pack names many
// objects, and a repair confirms them in turn only until one's pack is not
// its row's — each pack hashed once until something writes into the copy,
// however many hints name it (cache.Cache.ConfirmHint) — so a run's hints
// past the first few name packs already decided.
const maxHints = 16

// maxFormats is the most distinct formats one run records (Trace.Formats):
// git's formats are constants of its own, a few each run, so the bound is
// never met by a real run and holds the record to a few kilobytes if one is.
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
	// Version is the git version the leader's version event names (its
	// `exe`), and EventVersion that event's `evt` — the format of the stream
	// (4 on every git a pool runs, measured on 2.55.0 and 2.56.0), logged so
	// a later git's new format is seen; Version is empty for a git that wrote
	// no stream.
	Version, EventVersion string
	// Kinds is every kind an event's format classifies as.
	Kinds KindSet
	// Formats is the canonical format of every error event, each once, in
	// the order first met, at most maxFormats: what a run reported, for
	// logs and the suite.
	Formats []string
	// Hints is the packs a bare-"%s" event nominated as holding a corrupt
	// object (trace.go), each once, at most maxHints.
	Hints []Hint
	// Crashes is the error events reporting a process of the run's tree
	// dying of a signal (crashFormats): upload-pack's report of its
	// pack-objects, run-command's of any child it waited for. Never a row
	// and never a kind — a crash says something read the repository and
	// died, not what it could not read — but Result.Crashed reads it.
	Crashes int
}

// Reported reports whether an error event of the run printed any of formats —
// each a printf format as git's source spells it, read in the canonical form
// every event's format is recorded in. For a caller that answers one of git's
// own refusals of its request in its own words (a blame's range past the
// file's end): by the FORMAT, never the message, which carries whatever the
// request named — a path among it.
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
// signal, by canonical format: run-command's wait_or_whine for any child a
// git process waited for (every git), and 2.56's own for the pack-objects
// upload-pack runs. Both leave SIGPIPE out — a consumer that hung up — and
// run-command SIGINT and SIGQUIT too. Recognised by their formats and never
// their messages: the first's "%s" is the child's argv[0], never a request's
// text, but nothing here needs to trust that.
var crashFormats = map[string]bool{
	canonicalFormat(`%s died of signal %d`):           true,
	canonicalFormat(`pack-objects died of signal %d`): true,
}

// Hint is a pack git named as holding a corrupt object in words a request
// could reach: the object, and the pack by its checksum (as
// code.PackRef.GitChecksum spells it). It decides nothing until the pack's
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
	// sink decodes, and never past MaxLine; skip is set once it is not —
	// the rest of it is passed over to its newline — and long once it ran
	// past MaxLine, which is counted as unread.
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

// mayBeRead reports whether line, the start of a line, begins — or may yet
// begin — an event the sink decodes: an error event, and a version event
// while version (none has been read yet). Only the FIRST version event is
// the run's — the leader writes it before it can start a child — so every
// child's is passed over like any other line rather than decoded: a clone's
// stream then costs one decode, not one a process.
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

// finish reads a last line the stream did not end with a newline — a process
// killed part way through a write — as a line. Called once, after the copier
// has ended.
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

// classify reads one error event: by its format against the table — the row
// naming a corrupt packed object read for its hint as well — and, a bare
// "%s", for the one sentence a hint is.
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
			// The same sentence 2.56 reports through "%s" is a ROW on
			// 2.55, and its message names the same pack: read as a hint
			// too, under the same anchoring, so the repair can name the pack
			// it would otherwise have to find (the cache's ConfirmHint).
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

// packedObjectRow is the canonical format of git's sentence for a corrupt
// packed object, a row of the table on 2.55 (classify.go), whose
// message is the sentence a hint is read from (hintOf).
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

// hintOf is the hint message names when it is EXACTLY git's sentence for a
// corrupt packed object — "packed object <id> (stored in <pack>) is corrupt",
// nothing before it and nothing after, reported through a bare "%s" (2.56) or
// as the row whose format it is (2.55) — with <pack> a pack file of
// dir's own pack directory, as rev-list spells it
// (dir/objects/pack/pack-<checksum>.pack) or as upload-pack does, from inside
// the repository (./objects/pack/pack-<checksum>.pack). Anything else names no
// pack, and a run with no directory has no pack directory for a hint to be
// anchored to.
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
