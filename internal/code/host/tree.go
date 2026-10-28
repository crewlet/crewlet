package host

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// WHAT AN ENGINE WRITE READS AS IT STREAMS: a tree's entries, a file an edit
// rewrites, and cat-file's batch answers — each read as the bytes arrive and
// handed on, so what a build holds in memory is the change it was asked and
// one entry or one read buffer, never a directory or a file whole.

// treeEntry is one entry of a tree as git stores it: `<mode> SP <name> NUL
// <20-byte id>`, the mode as the tree spells it — "40000" for a directory, as
// git writes one, or whatever octal an older writer left (canon).
type treeEntry struct {
	mode string
	name string
	id   [20]byte
}

// The modes a tree spells, beside code's file modes.
const (
	modeTree      = "40000"
	modeSubmodule = "160000"
)

// maxEntryName bounds one entry's name as a tree is parsed: gitproc.MaxLine,
// the longest line any read here takes. A filesystem takes a name of 255
// bytes; a tree naming one past this bound was written by something that was
// not a checkout, and is refused rather than read into memory whole.
const maxEntryName = gitproc.MaxLine

// maxEntryMode bounds an entry's mode as a tree is parsed: git reads any run
// of octal digits (tree-walk.c's get_mode), and a mode is six of them, seven
// with an old writer's zero padding — eleven is past every one git can hold
// in the 32 bits it reads one into.
const maxEntryMode = 11

// errBadTree is a tree whose bytes are not git's tree format.
var errBadTree = errors.New("host: not a tree as git writes one")

// parseTree reads the raw tree r — git's own format, as `cat-file --batch`
// sends a tree's contents — handing each entry to each as it is read, in the
// order the tree holds them. An entry that does not parse is errBadTree naming
// what is wrong; each's error ends the read.
func parseTree(r io.Reader, each func(treeEntry) error) error {
	br := bufio.NewReader(r)
	for {
		mode, err := readMode(br)
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
		name, err := readName(br)
		if err != nil {
			return err
		}
		var e treeEntry
		e.mode, e.name = mode, name
		if _, err := io.ReadFull(br, e.id[:]); err != nil {
			return fmt.Errorf("%w: the entry %q ends before its object id (%v)", errBadTree, name, err)
		}
		if err := each(e); err != nil {
			return err
		}
	}
}

// readMode reads an entry's mode up to its space: one to maxEntryMode octal
// digits, git's own reading of one. io.EOF is the tree's end, between entries.
func readMode(br *bufio.Reader) (string, error) {
	var mode []byte
	for {
		c, err := br.ReadByte()
		switch {
		case errors.Is(err, io.EOF) && len(mode) == 0:
			return "", io.EOF
		case err != nil:
			return "", fmt.Errorf("%w: an entry's mode ends with no space (%v)", errBadTree, err)
		case c == ' ' && len(mode) > 0:
			return string(mode), nil
		case c < '0' || c > '7' || len(mode) == maxEntryMode:
			return "", fmt.Errorf("%w: an entry's mode %q is no octal mode git reads", errBadTree,
				append(mode, c))
		}
		mode = append(mode, c)
	}
}

// readName reads an entry's name up to its NUL, bounded by maxEntryName.
func readName(br *bufio.Reader) (string, error) {
	var name []byte
	for {
		chunk, err := br.ReadSlice(0)
		name = append(name, chunk...)
		switch {
		case len(name) > maxEntryName+1:
			return "", fmt.Errorf("%w: an entry's name past %d bytes", errBadTree, maxEntryName)
		case err == nil:
			name = name[:len(name)-1]
			if len(name) == 0 || bytes.IndexByte(name, '/') >= 0 {
				return "", fmt.Errorf("%w: an entry named %q", errBadTree, name)
			}
			return string(name), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return "", fmt.Errorf("%w: an entry's name ends with no NUL (%v)", errBadTree, err)
		}
	}
}

// canon is the entry's mode as git reads it (tree-walk.c's canon_mode, the one
// every reader of a tree and every index applies): a regular file 100644 or,
// with its owner's execute bit, 100755; a link 120000; a directory 40000;
// anything else a submodule. A build writes a directory it passes through in
// these modes — as a checkout and a commit of the same change would, since
// the index holds nothing else — so the legacy group-writable 100664 an old
// tree may carry, which `index-pack --strict` refuses in a NEW tree
// (badFilemode), is written 100644 as git's own commit would write it.
func (e treeEntry) canon() string {
	m, err := strconv.ParseUint(e.mode, 8, 64)
	if err != nil {
		return modeSubmodule // unreachable: readMode admits octal digits alone
	}
	switch uint32(m) & 0o170000 {
	case 0o100000:
		if m&0o100 != 0 {
			return code.ModeExecutable
		}
		return code.ModeFile
	case 0o120000:
		return code.ModeLink
	case 0o040000:
		return modeTree
	}
	return modeSubmodule
}

// encodeTree is entries in git's raw tree format: the inverse of parseTree,
// for the parser's round trip (FuzzParseTree).
func encodeTree(entries []treeEntry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		b.WriteString(e.mode)
		b.WriteByte(' ')
		b.WriteString(e.name)
		b.WriteByte(0)
		b.Write(e.id[:])
	}
	return b.Bytes()
}

// typeOf is the object type a canonical mode names, as mktree is told it.
func typeOf(mode string) string {
	switch mode {
	case modeTree:
		return "tree"
	case modeSubmodule:
		return "commit"
	}
	return "blob"
}

// mktreeLine is one entry as `mktree -z` reads it: `<mode> SP <type> SP
// <hex id> TAB <name> NUL` — every name as it is, since -z quotes nothing.
func mktreeLine(w io.Writer, mode, id, name string) error {
	_, err := fmt.Fprintf(w, "%s %s %s\t%s\x00", mode, typeOf(mode), id, name)
	return err
}

// idHex is a tree entry's id in hex.
func (e treeEntry) idHex() string { return hex.EncodeToString(e.id[:]) }

// editBinaryHead is how far into a file an edit looks for a NUL before it
// calls the file binary: git's own heuristic (xdiff-interface.c's
// buffer_is_binary, FIRST_FEW_BYTES), so a file git diffs as text an edit
// rewrites, and one git calls binary it refuses.
const editBinaryHead = 8000

// editStream applies ONE EDIT to a file's bytes as they stream through it:
// Old must occur exactly once — an occurrence overlapping another counts, so
// "aa" in "aaa" is two — and is replaced by New as the bytes are written on to
// out. It holds len(Old) bytes and the chunk it is handed, never the file.
//
// A second occurrence ends it (edit_ambiguous, both lines named); none by the
// end is edit_not_found; a NUL in the first editBinaryHead bytes is
// edit_binary; a carriage return anywhere in a file while Old or New holds a
// line break is edit_line_endings — a multi-line edit of a CRLF file would
// splice LF lines into CRLF ones. Each refusal is the edit's verdict, read
// after Close, and the bytes written to out are then the caller's to throw
// away.
type editStream struct {
	old, new []byte
	out      io.Writer
	spans    bool // Old or New holds a line break

	buf   []byte // stream bytes from base on, not yet both scanned and handled
	base  int64  // the stream offset of buf[0]
	scan  int64  // the next offset an occurrence may begin at
	emit  int64  // the offset up to which the input is written on, or skipped as Old
	lines int64  // line breaks in the stream before base
	read  int64  // bytes taken

	match         int64 // the occurrence's offset, -1 for none yet
	matchLine     int64
	refusal       code.WriteRefusal
	first, second int64 // the two lines of an ambiguous edit
	sawCR         bool
}

// newEditStream is an edit of old into new, written on to out.
func newEditStream(old, new string, out io.Writer) *editStream {
	return &editStream{old: []byte(old), new: []byte(new), out: out, match: -1,
		spans: strings.Contains(old, "\n") || strings.Contains(new, "\n")}
}

// Write takes the next bytes of the file. It never fails for a refusal — the
// verdict waits for Close, so the whole file is read and cat-file's batch goes
// on — only for out's own failure.
func (e *editStream) Write(p []byte) (int, error) {
	n := len(p)
	if e.refusal != "" {
		e.read += int64(n)
		return n, nil
	}
	if e.read < editBinaryHead {
		head := p[:min(int64(len(p)), editBinaryHead-e.read)]
		if bytes.IndexByte(head, 0) >= 0 {
			e.refusal = code.WriteEditBinary
		}
	}
	if !e.sawCR && bytes.IndexByte(p, '\r') >= 0 {
		e.sawCR = true
		if e.spans {
			e.refusal = code.WriteEditLineEndings
		}
	}
	e.read += int64(n)
	if e.refusal != "" {
		return n, nil
	}
	e.buf = append(e.buf, p...)
	if err := e.step(false); err != nil {
		return 0, err
	}
	return n, nil
}

// step finds every occurrence that has begun in what buf holds and writes on
// what no occurrence can still reach — all of it, at the end.
func (e *editStream) step(end bool) error {
	last := e.base + int64(len(e.buf)) - int64(len(e.old)) // the last offset an occurrence fits at
	for e.scan <= last {
		i := bytes.Index(e.buf[e.scan-e.base:], e.old)
		if i < 0 {
			break
		}
		at := e.scan + int64(i)
		line := e.lines + int64(bytes.Count(e.buf[:at-e.base], []byte{'\n'})) + 1
		if e.match >= 0 {
			e.refusal, e.first, e.second = code.WriteEditAmbiguous, e.matchLine, line
			return nil
		}
		e.match, e.matchLine = at, line
		e.scan = at + 1
	}
	if e.scan <= last {
		e.scan = last + 1
	}
	if end {
		e.scan = e.base + int64(len(e.buf))
	}
	// Write on what is settled — every byte before scan, which no occurrence
	// still to be found can reach — Old's bytes skipped for New at the
	// occurrence. The occurrence's end may lie past scan, its last bytes
	// still to be scanned for an occurrence overlapping it: emit then runs
	// ahead of scan until scan passes it.
	for e.emit < e.scan {
		if e.match >= e.emit && e.match < e.scan {
			if _, err := e.out.Write(e.buf[e.emit-e.base : e.match-e.base]); err != nil {
				return err
			}
			if _, err := e.out.Write(e.new); err != nil {
				return err
			}
			e.emit = e.match + int64(len(e.old))
			continue
		}
		if _, err := e.out.Write(e.buf[e.emit-e.base : e.scan-e.base]); err != nil {
			return err
		}
		e.emit = e.scan
	}
	// Drop what is both handled and scanned past, counting its line breaks.
	keep := min(e.emit, e.scan)
	if drop := keep - e.base; drop > 0 {
		e.lines += int64(bytes.Count(e.buf[:drop], []byte{'\n'}))
		e.buf = append(e.buf[:0], e.buf[drop:]...)
		e.base = keep
	}
	return nil
}

// Close writes on the rest and answers the edit's verdict: "" for an edit
// made, else its refusal and the sentence that names it for path.
func (e *editStream) Close(path string) (code.WriteRefusal, string, error) {
	if e.refusal == "" {
		if err := e.step(true); err != nil {
			return "", "", err
		}
	}
	switch {
	case e.refusal == code.WriteEditBinary:
		return e.refusal, fmt.Sprintf("%s is binary (a NUL in its first %d bytes): put it whole, or change it in a "+
			"coding box (run_sandbox)", path, editBinaryHead), nil
	case e.refusal == code.WriteEditLineEndings:
		return e.refusal, fmt.Sprintf("%s holds carriage returns, and an edit spanning a line would mix its "+
			"line endings: edit it a line at a time, or put it whole", path), nil
	case e.refusal == code.WriteEditAmbiguous:
		return e.refusal, fmt.Sprintf("the old text occurs more than once in %s — at line %d and at line %d at "+
			"least: give more of the text around it, so it occurs once", path, e.first, e.second), nil
	case e.match < 0:
		return code.WriteEditNotFound, fmt.Sprintf("the old text occurs nowhere in %s: read the file again and "+
			"give text it holds", path), nil
	}
	return "", "", nil
}

// batchSink is where one answer of a `cat-file --batch -Z` goes: an object's
// contents, as they stream, or the request's absence.
type batchSink struct {
	// object is handed each object's header and answers the writer its
	// contents go to.
	object func(i int, oid, kind string, size int64) (io.Writer, error)
	// missing is told a request the copy holds no object for.
	missing func(i int) error
}

// batchReader is a Cmd.Stdout reading `cat-file --batch -Z`'s answers as they
// stream — `<oid> SP <type> SP <size> NUL <contents> NUL` for an object,
// `<name> SP missing NUL` for none — handing each object's contents to its
// sink as they arrive, so no object is held whole. NUL-ended both ways (-Z),
// so a request naming a path git holds with a line feed in it is one request.
type batchReader struct {
	sink  batchSink
	names []string // the requests, in order: what a missing answer echoes

	i      int    // the request being answered
	header []byte // a header being read
	out    io.Writer
	left   int64 // contents still to come
	tail   bool  // the NUL after the contents is next
	err    error
}

// newBatchReader reads the answers to names.
func newBatchReader(names []string, sink batchSink) *batchReader {
	return &batchReader{names: names, sink: sink}
}

func (b *batchReader) Write(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n := len(p)
	for len(p) > 0 {
		switch {
		case b.left > 0:
			take := min(int64(len(p)), b.left)
			if _, err := b.out.Write(p[:take]); err != nil {
				b.err = err
				return 0, err
			}
			b.left -= take
			p = p[take:]
			b.tail = b.left == 0
		case b.tail:
			if p[0] != 0 {
				b.err = fmt.Errorf("host: cat-file's answer to %q ends with %q, not a NUL", b.names[b.i], p[0])
				return 0, b.err
			}
			p = p[1:]
			b.tail = false
			b.i++
		default:
			j := bytes.IndexByte(p, 0)
			if j < 0 {
				if len(b.header)+len(p) > gitproc.MaxLine {
					b.err = gitproc.ErrLineTooLong
					return 0, b.err
				}
				b.header = append(b.header, p...)
				p = p[len(p):]
				continue
			}
			line := append(b.header, p[:j]...)
			b.header = b.header[:0]
			p = p[j+1:]
			if err := b.answer(line); err != nil {
				b.err = err
				return 0, err
			}
		}
	}
	return n, nil
}

// answer reads one header.
func (b *batchReader) answer(line []byte) error {
	if b.i >= len(b.names) {
		return fmt.Errorf("host: cat-file answered %q past the %d requests it was given", line, len(b.names))
	}
	if string(line) == b.names[b.i]+" missing" {
		if err := b.sink.missing(b.i); err != nil {
			return err
		}
		b.i++
		return nil
	}
	fields := strings.Split(string(line), " ")
	if len(fields) != 3 {
		return fmt.Errorf("host: cat-file answered %q for %q, which is no object", line, b.names[b.i])
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("host: cat-file answered the size %q for %q", fields[2], b.names[b.i])
	}
	out, err := b.sink.object(b.i, fields[0], fields[1], size)
	if err != nil {
		return err
	}
	b.out, b.left = out, size
	if size == 0 {
		b.tail = true
	}
	return nil
}

// Close reports an answer cut short: every request answered whole, or the
// reason it was not.
func (b *batchReader) Close() error {
	switch {
	case b.err != nil:
		return b.err
	case b.left > 0 || b.tail || len(b.header) > 0:
		return fmt.Errorf("host: cat-file's answer to %q ended inside it", b.names[min(b.i, len(b.names)-1)])
	case b.i != len(b.names):
		return fmt.Errorf("host: cat-file answered %d of the %d requests it was given", b.i, len(b.names))
	}
	return nil
}

// records is a Cmd.Stdout handing each NUL-ended record to fn as it arrives —
// cat-file's answers under -Z, merge-tree's under -z — each at most
// gitproc.MaxLine; close reports output that ended inside one.
type records struct {
	fn      func([]byte) error
	pending []byte
	err     error
}

// newRecords hands each record to fn, valid only during the call.
func newRecords(fn func([]byte) error) *records { return &records{fn: fn} }

func (r *records) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, 0)
		if i < 0 {
			if len(r.pending)+len(p) > gitproc.MaxLine {
				r.err = gitproc.ErrLineTooLong
				return 0, r.err
			}
			r.pending = append(r.pending, p...)
			break
		}
		rec := p[:i]
		if len(r.pending) > 0 {
			r.pending = append(r.pending, rec...)
			rec = r.pending
		}
		if len(rec) > gitproc.MaxLine {
			r.err = gitproc.ErrLineTooLong
			return 0, r.err
		}
		if err := r.fn(rec); err != nil {
			r.err = err
			return 0, err
		}
		r.pending = r.pending[:0]
		p = p[i+1:]
	}
	return n, nil
}

// close is the output's end: an error where it ended inside a record.
func (r *records) close() error {
	if r.err == nil && len(r.pending) > 0 {
		r.err = gitproc.ErrPartialLine
	}
	return r.err
}

// prefixWriter keeps the first n bytes written to it and counts the rest: a
// conflicted file's start, as a write answers it.
type prefixWriter struct {
	n     int
	kept  []byte
	total int64
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	if room := w.n - len(w.kept); room > 0 {
		w.kept = append(w.kept, p[:min(room, len(p))]...)
	}
	w.total += int64(len(p))
	return len(p), nil
}

// text is what the writer kept as a conflict answers it: "" for a file git
// reads as binary — a NUL in what was kept — and otherwise the bytes kept,
// less a character the cut split.
func (w *prefixWriter) text() string {
	if bytes.IndexByte(w.kept, 0) >= 0 {
		return ""
	}
	kept := w.kept
	if w.total > int64(len(kept)) {
		j := len(kept) - 1
		for j > 0 && j > len(kept)-utf8.UTFMax && !utf8.RuneStart(kept[j]) {
			j--
		}
		if j >= 0 && !utf8.FullRune(kept[j:]) {
			kept = kept[:j]
		}
	}
	return string(kept)
}

// truncated reports a file longer than what the writer kept.
func (w *prefixWriter) truncated() bool { return w.total > int64(len(w.kept)) }
