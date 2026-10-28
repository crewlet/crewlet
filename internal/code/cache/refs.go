package cache

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/hostbox"
)

// THE REFS: packed-refs and HEAD, written after every pack they need is
// installed, so no reader ever sees a ref before its objects.
//
// THERE ARE NEVER LOOSE REFS — a loose ref outranks packed-refs for every
// reader — so packed-refs is the whole ref store, and it is PEELED: every ref
// under refs/tags/ that names an annotated tag is followed by the commit (or
// tree, or blob) the tag points at, under the header's `peeled` trait, which
// is exactly what git trusts for refs/tags/. Without it git reads every tag
// object to peel it on every advertisement: 5 000 annotated tags advertise
// in 7 ms peeled and 28 ms not. The peel is ONE `cat-file --batch-check`
// over the tags a round changed, run once every pack is installed.
//
// A DELTA IS A STREAM MERGE of the file on disk with the rows that moved —
// one sequential read and one sequential write, IN PLACE, never a parse into
// a map: a file of code.MaxRefsPerRepo refs is about 10 MB, and every node
// rewrites it after every push that moves a ref, so a line carried is checked
// and copied straight from the read buffer and only a moved ref's line is
// made. The file read is hashed as it is read; a file that is not the one this
// cache wrote (gone, unreadable, its digest, or a line outside the grammar) is
// not merged onto, because a merge would carry the damage forward — the round
// reads the repository whole instead.

// packedRefsHeader is packed-refs' first line, and the whole of an empty one.
const packedRefsHeader = "# pack-refs with: peeled sorted \n"

// errRefsNotOurs is a packed-refs this cache did not write: its digest is not
// the incarnation's, or it holds a line outside the grammar.
var errRefsNotOurs = errors.New("cache: packed-refs is not the file this cache wrote")

// headFor is HEAD naming branch.
func headFor(branch string) []byte { return []byte("ref: refs/heads/" + branch + "\n") }

// headValid reports whether a HEAD's bytes are a symbolic ref to a branch —
// the one shape git reads a repository by.
func headValid(raw []byte) bool {
	branch, ok := bytes.CutPrefix(raw, []byte("ref: refs/heads/"))
	if !ok {
		return false
	}
	branch, ok = bytes.CutSuffix(branch, []byte("\n"))
	return ok && gitref.ValidBranch(string(branch))
}

// writeRefs makes inc's HEAD and packed-refs the snapshot's, and reports
// whether it wrote either.
//
// HEAD is written when the incarnation has no state, when the default branch
// changed, or — on a full round — when its bytes are not the target's.
// packed-refs is written when the round is full or the delta names a ref:
// staged, then the sync.refs fault point, then both renamed in under the
// entry's mu with the digest that describes the new file — so Verify reads
// the file and its digest together — and the incarnation synced once, before
// the state that claims the position may be renamed in.
//
// A FULL ROUND WHOSE packed-refs IS ALREADY, BYTE FOR BYTE, THE ONE IT STAGED
// leaves it in place and reports nothing written: the file on disk is hashed
// against the staged file's digest, so damage or an edit is still put right,
// while a copy holding exactly its image is not written into. Reported as
// written, every full round "wrote into" its copy whatever it changed, so the
// in-place round a failed request asks first (Cache.Rebuild) made every copy
// a rebuild had just read look moved, and the rule that refuses a rebuild of
// what the last one read (rebuildRefusal's unchanged) never held.
//
// by is the round's reader of inc (roundReader), which the peel names.
func (c *Cache) writeRefs(ctx context.Context, e *entry, inc *incarnation, snap snapshot,
	by gitproc.Repairer) (bool, error) {
	e.mu.Lock()
	noState, branchMoved := inc.state == nil, inc.repo.DefaultBranch != snap.repo.DefaultBranch
	digest := inc.refsDigest
	e.mu.Unlock()

	head := headFor(snap.repo.DefaultBranch)
	writeHead := noState || branchMoved
	if !writeHead && snap.full {
		on, err := os.ReadFile(filepath.Join(inc.dir, "HEAD"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("cache: sync %s: refs: %w", e.id, err)
		}
		writeHead = !bytes.Equal(on, head)
	}
	writePacked := snap.full || len(snap.refs) > 0
	if !writeHead && !writePacked {
		return false, nil
	}

	var staged []placement
	// refsMove: a new packed-refs is staged to be renamed in.
	refsMove := false
	defer func() {
		for _, s := range staged {
			if s.staged != "" {
				_ = removeStaged(s.staged, nil) // a round that failed: tmp/ is tidied
			}
		}
	}()
	var written refsFile
	if writePacked {
		peeled, err := c.peel(ctx, e.id, inc.dir, snap, by)
		if err != nil {
			return false, err
		}
		path, out, err := c.stagePackedRefs(inc, snap, peeled, digest)
		if err != nil {
			return false, fmt.Errorf("cache: sync %s: refs: %w", e.id, err)
		}
		written = out
		if snap.full && holdsDigest(filepath.Join(inc.dir, "packed-refs"), out.digest) {
			// Already the file it would write: nothing to rename in.
			if err := removeStaged(path, nil); err != nil {
				return false, fmt.Errorf("cache: sync %s: refs: %w", e.id, err)
			}
		} else {
			staged = append(staged, placement{staged: path, dst: filepath.Join(inc.dir, "packed-refs")})
			refsMove = true
		}
	}
	if writeHead {
		path, err := c.stage(hostbox.FileMode, func(f *os.File) error {
			_, werr := f.Write(head)
			return werr
		})
		if err != nil {
			return false, fmt.Errorf("cache: sync %s: refs: %w", e.id, err)
		}
		staged = append(staged, placement{staged: path, dst: filepath.Join(inc.dir, "HEAD")})
	}
	if c.faults != nil {
		if err := c.faults.Hit(ctx, codefault.SyncRefs, 0); err != nil {
			return false, fmt.Errorf("cache: sync %s: refs: %w", e.id, err)
		}
	}
	if refsMove {
		// The refs moved: what a declaration says of that, before the new
		// packed-refs is renamed in (dropFor's order rule). No declaration
		// drops for moved refs today — a bitmap names commits it selected,
		// and stays sound whichever refs point at them.
		if _, err := c.dropFor(e, inc, EventRefsMoved, eventDetail{}); err != nil {
			return false, fmt.Errorf("cache: sync %s: refs: %w", e.id, err)
		}
	}

	e.mu.Lock()
	placedRefs := false
	for i := range staged {
		if err := c.place(staged[i].staged, staged[i].dst); err != nil {
			e.mu.Unlock()
			staged[i].staged = ""
			return false, fmt.Errorf("cache: sync %s: refs: %w", e.id, err)
		}
		staged[i].staged = ""
		placedRefs = placedRefs || filepath.Base(staged[i].dst) == "packed-refs"
	}
	if writePacked {
		inc.refsDigest, inc.refs = written.digest, written.refs
	}
	if placedRefs {
		c.setSize(inc, inc.diskBytes+written.bytes-written.was)
	}
	e.mu.Unlock()
	if len(staged) == 0 {
		return false, nil
	}
	if err := syncDir(inc.dir); err != nil {
		return false, fmt.Errorf("cache: sync %s: refs: %w", e.id, err)
	}
	return true, nil
}

// holdsDigest reports whether the file at path hashes to digest (SHA-256,
// hex) — false for any file that cannot be read whole.
func holdsDigest(path, digest string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }() // read-only: what was read is the answer
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return false
	}
	return hex.EncodeToString(sum.Sum(nil)) == digest
}

// placement is a staged file and where it goes; staged is "" once placed.
type placement struct{ staged, dst string }

// refsFile is a packed-refs as written: its digest, its refs, its size, and
// the size of the file it replaces.
type refsFile struct {
	digest     string
	refs       int
	bytes, was int64
}

// stagePackedRefs stages the packed-refs snap leaves: the snapshot's refs on a
// full round, and on a delta the file on disk merged with the refs that
// moved. digest is what this cache last wrote; a file on disk that is not it
// is errRefsNotOurs.
func (c *Cache) stagePackedRefs(inc *incarnation, snap snapshot, peeled map[string]string,
	digest string) (string, refsFile, error) {

	var out refsFile
	path, err := c.stage(hostbox.FileMode, func(f *os.File) error {
		sum := sha256.New()
		w := bufio.NewWriterSize(io.MultiWriter(f, sum), copyBuffer)
		n := &countingWriter{w: w}
		if _, err := io.WriteString(n, packedRefsHeader); err != nil {
			return err
		}
		var err error
		if snap.full {
			err = writeFull(n, snap.refs, peeled, &out)
		} else {
			err = mergeDelta(inc.dir, n, snap.refs, peeled, digest, &out)
		}
		if err != nil {
			return err
		}
		if err = w.Flush(); err != nil {
			return err
		}
		out.digest, out.bytes = hex.EncodeToString(sum.Sum(nil)), n.n
		return nil
	})
	return path, out, err
}

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// writeRef writes one ref's line and, for a tag that peels, its peel line.
func writeRef(w io.Writer, ref, oid string, peeled map[string]string) error {
	line := oid + " " + ref + "\n"
	if p, ok := peeled[oid]; ok && isTag(ref) {
		line += "^" + p + "\n"
	}
	_, err := io.WriteString(w, line)
	return err
}

// isTag reports a ref git peels in a `peeled` packed-refs.
func isTag(ref string) bool { return strings.HasPrefix(ref, "refs/tags/") }

// writeFull writes a full snapshot's refs, already sorted by ref.
func writeFull(w io.Writer, refs []code.RefRow, peeled map[string]string, out *refsFile) error {
	for _, r := range refs {
		if err := writeRef(w, r.Ref, r.OID, peeled); err != nil {
			return err
		}
		out.refs++
	}
	return nil
}

// mergeDelta writes the packed-refs in dir merged with a delta's refs: a ref
// the delta names takes its new object (an empty OID deletes it, and a
// deletion of a ref the file does not hold is nothing), and every other line —
// its peel line included — is carried byte for byte, written straight from
// the read buffer. digest is what this cache last wrote there.
//
// IN PLACE, because every git node runs it after every push that moves a ref,
// over a file of up to code.MaxRefsPerRepo lines: a line carried is checked
// on the bytes the reader holds and copied out, and only a moved ref's line is
// made — so the merge allocates for what moved and never for the file.
//
// A file it cannot read whole AS THE ONE THIS CACHE WROTE is errRefsNotOurs —
// missing, not a file, failing a read, its digest or a line outside the
// grammar alike — and the round reads the repository whole and writes the
// file afresh: an operator's rm and a disk fault are exactly what that
// repairs, where failing would fail every delta that moves a ref until the
// process restarts. A failure to WRITE the staged file is the round's own.
func mergeDelta(dir string, w io.Writer, delta []code.RefRow, peeled map[string]string,
	digest string, out *refsFile) error {

	moves := slices.Clone(delta)
	slices.SortFunc(moves, func(a, b code.RefRow) int { return strings.Compare(a.Ref, b.Ref) })
	f, err := os.Open(filepath.Join(dir, "packed-refs"))
	if err != nil {
		return fmt.Errorf("%w: %w", errRefsNotOurs, err)
	}
	defer func() { _ = f.Close() }() // read-only
	sum := sha256.New()
	lines := &refsReader{r: bufio.NewReaderSize(io.TeeReader(f, sum), copyBuffer)}
	if err = lines.header(); err != nil {
		return err
	}

	// create writes every move ordered before ref — every one left, at the
	// end: a ref the file does not hold, created, or a deletion of nothing.
	create := func(ref []byte, end bool) error {
		for len(moves) > 0 && (end || moves[0].Ref < string(ref)) {
			if moves[0].OID != "" {
				if werr := writeRef(w, moves[0].Ref, moves[0].OID, peeled); werr != nil {
					return werr
				}
				out.refs++
			}
			moves = moves[1:]
		}
		return nil
	}
	carried := false // the last ref line was carried, so its peel line is too
	for {
		var line refsLine
		line, err = lines.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if line.peel {
			if carried {
				if _, err = w.Write(line.raw); err != nil {
					return err
				}
			}
			continue
		}
		if err = create(line.ref, false); err != nil {
			return err
		}
		if len(moves) > 0 && moves[0].Ref == string(line.ref) {
			// Moved: its new line (and peel) replaces the old one's.
			if moves[0].OID != "" {
				if err = writeRef(w, moves[0].Ref, moves[0].OID, peeled); err != nil {
					return err
				}
				out.refs++
			}
			moves, carried = moves[1:], false
			continue
		}
		if _, err = w.Write(line.raw); err != nil {
			return err
		}
		out.refs++
		carried = true
	}
	if err = create(nil, true); err != nil {
		return err
	}
	out.was = lines.n
	if got := hex.EncodeToString(sum.Sum(nil)); got != digest {
		return fmt.Errorf("%w: it hashes to %s, and this cache wrote %s", errRefsNotOurs, got, digest)
	}
	return nil
}

// The prefixes a line of packed-refs is held to, as bytes.
var (
	refsPrefix = []byte("refs/")
	tagsPrefix = []byte("refs/tags/")
)

// refsReader reads packed-refs line by line, holding every line to the
// grammar this cache writes: the header, then `<oid> refs/…` lines in
// strictly ascending ref order, each under refs/tags/ optionally followed by
// one `^<oid>` line. It reads in place: a line is the reader's own buffer
// until the next read, and the one thing kept across lines — the previous
// ref, for the order — is copied into a buffer of its own, reused.
type refsReader struct {
	r *bufio.Reader
	// n is the bytes read.
	n int64
	// last is the previous ref line's ref.
	last []byte
	// peelable: the previous line is a ref under refs/tags/, so a peel line
	// may follow it.
	peelable bool
}

// refsLine is one line of the file.
type refsLine struct {
	// raw is the line, its newline included: the reader's buffer, valid
	// until the next read.
	raw []byte
	// ref is a ref line's ref, within raw; nil for a peel line.
	ref  []byte
	peel bool
}

// line is the next line, its newline included, or io.EOF at the file's end.
// Every failure to read it is errRefsNotOurs: a file this cache cannot read is
// not one a merge can carry forward.
func (l *refsReader) line() ([]byte, error) {
	b, err := l.r.ReadSlice('\n')
	l.n += int64(len(b))
	switch {
	case errors.Is(err, io.EOF) && len(b) == 0:
		return nil, io.EOF
	case errors.Is(err, io.EOF):
		return nil, fmt.Errorf("%w: its last line does not end", errRefsNotOurs)
	case errors.Is(err, bufio.ErrBufferFull):
		return nil, fmt.Errorf("%w: a line longer than %d bytes", errRefsNotOurs, l.r.Size())
	case err != nil:
		return nil, fmt.Errorf("%w: reading it: %w", errRefsNotOurs, err)
	}
	return b, nil
}

func (l *refsReader) header() error {
	b, err := l.line()
	switch {
	case errors.Is(err, io.EOF):
		return fmt.Errorf("%w: it is empty", errRefsNotOurs)
	case err != nil:
		return err
	case string(b) != packedRefsHeader:
		return fmt.Errorf("%w: it does not begin %q", errRefsNotOurs, packedRefsHeader)
	}
	return nil
}

// next is the next line, held to the grammar, or io.EOF at the end.
func (l *refsReader) next() (refsLine, error) {
	b, err := l.line()
	if err != nil {
		return refsLine{}, err
	}
	body := b[:len(b)-1]
	if len(body) > 0 && body[0] == '^' {
		if !l.peelable || !gitref.ValidOID(body[1:]) {
			return refsLine{}, fmt.Errorf("%w: a peel line %q after %s", errRefsNotOurs, body, l.last)
		}
		l.peelable = false
		return refsLine{raw: b, peel: true}, nil
	}
	sp := bytes.IndexByte(body, ' ')
	if sp < 0 {
		return refsLine{}, fmt.Errorf("%w: a line %q", errRefsNotOurs, body)
	}
	oid, ref := body[:sp], body[sp+1:]
	if !gitref.ValidOID(oid) || !bytes.HasPrefix(ref, refsPrefix) || bytes.ContainsAny(ref, " \t\r") ||
		bytes.Compare(ref, l.last) <= 0 {
		return refsLine{}, fmt.Errorf("%w: a line %q", errRefsNotOurs, body)
	}
	l.last = append(l.last[:0], ref...)
	l.peelable = bytes.HasPrefix(ref, tagsPrefix)
	return refsLine{raw: b, ref: ref}, nil
}

// peel peels every tag the snapshot sets — all of them on a full round, the
// delta's on a delta — with ONE cat-file over the objects they name, and
// answers each object that is an annotated tag with what it points at.
// An object no installed pack holds fails the round with
// gitproc.ErrMissingObject: the rows name what the packs do not hold here.
// The process names the round's reader by (gitproc.Cmd.Repairer), so what it
// reports is held for the round's own repair (roundReader).
func (c *Cache) peel(ctx context.Context, id, dir string, snap snapshot, by gitproc.Repairer) (map[string]string,
	error) {
	var oids []string
	named := map[string]string{}
	for _, r := range snap.refs {
		if r.OID == "" || !isTag(r.Ref) {
			continue
		}
		if _, ok := named[r.OID]; !ok {
			named[r.OID] = r.Ref
			oids = append(oids, r.OID)
		}
	}
	if len(oids) == 0 {
		return nil, nil
	}
	var stdin bytes.Buffer
	for _, oid := range oids {
		stdin.WriteString(oid + "^{}\n")
	}
	if c.knobs != nil && c.knobs.peeling != nil {
		c.knobs.peeling(dir)
	}
	peeled := map[string]string{}
	i := 0
	lines := gitproc.NewLines(func(line []byte) error {
		if i >= len(oids) {
			return fmt.Errorf("cache: cat-file answered more lines than it was asked")
		}
		oid := oids[i]
		i++
		got := string(line)
		switch {
		case got == oid+"^{} missing":
			return fmt.Errorf("%w: %s names %s, which no installed pack of %s holds",
				gitproc.ErrMissingObject, named[oid], oid, id)
		case !gitref.ValidOID(got):
			return fmt.Errorf("cache: cat-file peeled %s as %q", oid, got)
		case got != oid:
			peeled[oid] = got
		}
		return nil
	})
	// --buffer: no tag waits on an answer before it is sent, and without it
	// git writes and flushes every line on its own — 14 ms against 22 ms a
	// thousand ids (gitproc's measured section) — paid for every tag a full
	// round sets, while the round holds the repository's directory.
	// THROUGH THE DROPPING VIEW where the copy holds one (heldView): a tag a
	// push revived onto a dropping tag object before its expunge's begin is
	// one the copy serves, and named missing it would fail every round of the
	// repository until the expunge's commit — which its maintainer's own
	// rescue, waiting on a sync of the repository, could then never make.
	_, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: dir, Dropping: heldView(dir), Args: []string{"cat-file", "--batch-check=%(objectname)", "--buffer"},
		Config: c.ServeConfig(), Stdin: bytes.NewReader(stdin.Bytes()), Stdout: lines, Class: gitproc.ClassRead,
		Repairer: by, Deadline: gitproc.PackWorkDeadline(int64(stdin.Len())),
	})
	if cerr := lines.Close(); err == nil {
		err = cerr
	}
	if err == nil && i != len(oids) {
		err = fmt.Errorf("cache: cat-file answered %d of %d tags", i, len(oids))
	}
	if err != nil {
		return nil, fmt.Errorf("cache: sync %s: refs: peel: %w", id, err)
	}
	return peeled, nil
}
