package executor

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// THE FIRST READING SPOOLS WHAT IT WRITES, AND THE SECOND COPIES IT: every
// unit's output, as kept, goes in order into one gzipped spool on the node's
// own disk, and the second reading writes the tar from it, reading the box no
// more. The package doc's "The scrub holds no value whole" has the argument.

// unitSpoolGzipLevel is the level the spool is written at: the kept archive's
// ([keptGzipLevel]). The take's room holds the spool to the sealed bound
// ([spoolPlace]), so it must come out no larger than the archive kept from
// it: at one to six flate stores base64 of compressed media whole, where
// seven entropy-codes it (keptGzipLevel says by how much), and a spool written
// faster would refuse a session the seal keeps.
const unitSpoolGzipLevel = keptGzipLevel

// unitSpoolPrefix names a take's spool in its process's spool directory.
const unitSpoolPrefix = "unit-"

// errUnitSpoolPastRoom is a spool that would write past the room its take
// holds for it: the session is not kept.
var errUnitSpoolPastRoom = errors.New("executor: the scrub's spool would pass the room its take holds for it")

// spoolPlace is where a take's scrub spools and in what room: a directory of
// its process's own, and the take's room, which take debits before a byte is
// written and give credits as one leaves the disk, both refused past or
// counted in the room the take holds ([sandbox.SessionSpool]); spooled is told
// once the first reading has written all it will.
//
// A ZERO DIRECTORY is the system's temporary one, a nil take no bound, and a
// nil give or spooled tells nobody: a scrub a test runs. A take always names
// them ([Runner.TakeSession] refuses one that does not).
type spoolPlace struct {
	dir     string
	take    func(int64) error
	give    func(int64)
	spooled func()
}

// spooling is b spooling at place.
func (b scrubBounds) spooling(place spoolPlace) scrubBounds {
	b.spool = place
	return b
}

// unitSpool is a take's one spool: gzip members holding what its first reading
// wrote, in the order it wrote it, held to the room its take holds, the bytes
// on disk counted as they are written. Units share the member open until one
// outgrows its way's room ([candidate]), which opens a member of its own, so
// the spool can be cut back to where that unit began ([unitSpool.cutBack]).
type unitSpool struct {
	place spoolPlace
	disk  *spoolDisk
	zw    *gzip.Writer
	bw    *bufio.Writer
	// at is how many bytes the readings have written to it, before its gzip:
	// where the next lands, which a plan's marks and cuts name.
	at int64
	// memberFile and memberAt are where the member open began: on the disk,
	// and in what the readings wrote.
	memberFile, memberAt int64
	// taken is what the scrub took out of what the spool holds ([tallier]),
	// memberTaken of what it held as the member open began: cut back with
	// the bytes it was taken from.
	taken, memberTaken int64
}

// spoolDisk is what the spool's gzip writes, in files of
// executorkeep.SpoolSegmentBytes each, so THE SECOND READING DRAINS IT: each
// file it has read to its end is removed and given back to the take's room
// at once, which the take's sealed spools then write into (the package doc's
// "The scrub holds no value whole"). A gzip member may span files. Every byte
// is taken from the room before it is written, a write refused whole past it,
// so no byte past the room reaches the disk.
type spoolDisk struct {
	place spoolPlace
	// name is the first file's; file i past it is name-NNNN.
	name string
	// files are the ones on disk, files[0] being number first.
	files []*os.File
	first int
	// n is every byte written, a global offset; held is what the files on
	// disk hold, taken from the room and not yet given back; off is how far
	// the reading is into files[0].
	n, held, off int64
}

// segmentBytes is one file's size.
const segmentBytes = executorkeep.SpoolSegmentBytes

func (d *spoolDisk) Write(p []byte) (int, error) {
	if d.place.take != nil {
		if err := d.place.take(int64(len(p))); err != nil {
			return 0, fmt.Errorf("%w: %w", errUnitSpoolPastRoom, err)
		}
	}
	d.held += int64(len(p))
	written := 0
	for written < len(p) {
		f, err := d.writing()
		if err != nil {
			d.giveBack(int64(len(p) - written))
			return written, err
		}
		chunk := p[written:min(len(p), written+int(segmentBytes-d.n%segmentBytes))]
		n, err := f.Write(chunk)
		d.n += int64(n)
		written += n
		if err != nil {
			d.giveBack(int64(len(p) - written))
			return written, err
		}
	}
	return written, nil
}

// writing is the file the next byte lands in, opened where the spool's last
// is full.
func (d *spoolDisk) writing() (*os.File, error) {
	i := int(d.n / segmentBytes)
	if i < len(d.files) {
		return d.files[i], nil
	}
	f, err := os.OpenFile(fmt.Sprintf("%s-%04d", d.name, i), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("executor: a file of the scrub's spool on this node's disk: %w", err)
	}
	d.files = append(d.files, f)
	return f, nil
}

// giveBack gives n bytes of what the files held back to the take's room.
func (d *spoolDisk) giveBack(n int64) {
	d.held -= n
	if d.place.give != nil && n > 0 {
		d.place.give(n)
	}
}

// truncate cuts the spool back to its first at bytes, while it is written:
// the file holding at is cut there and every later one removed, and what they
// held given back.
func (d *spoolDisk) truncate(at int64) error {
	i := int(at / segmentBytes)
	for len(d.files) > i+1 {
		last := d.files[len(d.files)-1]
		d.files = d.files[:len(d.files)-1]
		_ = last.Close()
		_ = os.Remove(last.Name())
	}
	removed := d.n - at
	d.n = at
	defer d.giveBack(removed)
	if i == len(d.files) {
		return nil
	}
	f, cut := d.files[i], at-int64(i)*segmentBytes
	if err := f.Truncate(cut); err != nil {
		return err
	}
	_, err := f.Seek(cut, io.SeekStart)
	return err
}

// Read reads the spool from its start, removing each file it has read to its
// end and giving its bytes back to the take's room before the next is read.
func (d *spoolDisk) Read(p []byte) (int, error) {
	for len(d.files) > 0 {
		f := d.files[0]
		size := min(segmentBytes, d.n-int64(d.first)*segmentBytes)
		if d.off < size {
			n, err := f.ReadAt(p[:min(int64(len(p)), size-d.off)], d.off)
			d.off += int64(n)
			if err == io.EOF {
				err = nil
			}
			return n, err
		}
		d.files, d.first, d.off = d.files[1:], d.first+1, 0
		_ = f.Close()
		_ = os.Remove(f.Name())
		d.giveBack(size)
	}
	return 0, io.EOF
}

// remove removes every file left and gives back what they held.
func (d *spoolDisk) remove() {
	for _, f := range d.files {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	d.files = nil
	d.giveBack(d.held)
}

// newUnitSpool is an empty spool at place, its files the owner's alone (0600,
// as [os.CreateTemp] makes them).
func newUnitSpool(place spoolPlace) (*unitSpool, error) {
	f, err := os.CreateTemp(place.dir, unitSpoolPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("executor: a spool for the scrub on this node's disk: %w", err)
	}
	s := &unitSpool{place: place, disk: &spoolDisk{place: place, name: f.Name(), files: []*os.File{f}}}
	if s.zw, err = gzip.NewWriterLevel(s.disk, unitSpoolGzipLevel); err != nil {
		s.remove()
		return nil, fmt.Errorf("executor: the scrub's spool at gzip level %d: %w", unitSpoolGzipLevel, err)
	}
	s.bw = bufio.NewWriterSize(s.zw, 64<<10)
	return s, nil
}

func (s *unitSpool) Write(p []byte) (int, error) {
	n, err := s.bw.Write(p)
	s.at += int64(n)
	return n, err
}

// WriteString writes str.
func (s *unitSpool) WriteString(str string) (int, error) {
	n, err := s.bw.WriteString(str)
	s.at += int64(n)
	return n, err
}

// newMember ends the member open and opens another where the spool stands,
// unless the one open holds nothing yet.
func (s *unitSpool) newMember() error {
	if s.at == s.memberAt {
		s.memberTaken = s.taken
		return nil
	}
	if err := s.bw.Flush(); err != nil {
		return err
	}
	if err := s.zw.Close(); err != nil {
		return err
	}
	s.zw.Reset(s.disk)
	s.memberFile, s.memberAt, s.memberTaken = s.disk.n, s.at, s.taken
	return nil
}

func (s *unitSpool) tally(taken int64) { s.taken += taken }

// cutBack drops all the member open holds, the spool cut back to where it
// began, so a unit not kept takes no room ([candidate.discard]).
func (s *unitSpool) cutBack() error {
	s.bw.Reset(s.zw)
	if err := s.disk.truncate(s.memberFile); err != nil {
		return err
	}
	s.at, s.taken = s.memberAt, s.memberTaken
	s.zw.Reset(s.disk)
	return nil
}

// empty begins the spool again, empty: a first reading read again from its
// start ([firstReadings]).
func (s *unitSpool) empty() error {
	s.memberFile, s.memberAt, s.memberTaken = 0, 0, 0
	return s.cutBack()
}

// reader ends the spool's writing, tells its take the first reading is done,
// and reads it back from its start, its members one stream, draining it.
// bufio's read-ahead only removes a file sooner.
func (s *unitSpool) reader() (io.Reader, error) {
	if err := s.bw.Flush(); err != nil {
		return nil, err
	}
	if err := s.zw.Close(); err != nil {
		return nil, err
	}
	s.bw, s.zw = nil, nil
	if s.place.spooled != nil {
		s.place.spooled()
	}
	zr, err := gzip.NewReader(bufio.NewReaderSize(s.disk, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("executor: reading the scrub's spool back: %w", err)
	}
	return zr, nil
}

// remove removes the spool and gives back what it held: on every way out of
// the scrub, a panic's included (the caller defers it).
func (s *unitSpool) remove() {
	if s != nil {
		s.disk.remove()
	}
}

// wayRoomBytes is the most of one way of a unit the first reading holds before
// the unit's way is known ([candidate]): 64 KiB, the buffer a unit's JSON was
// written through before, so its JSON way costs a take at its base nothing
// more (takeBaseHeldBytes), and its text way, held only once a unit is read
// both ways, 64 KiB of the 110 the base tier leaves each of [SessionTakesAtOnce]
// (340.6 MiB over 29 takes of 11.64). A larger way would cost the JSON way
// more than the buffer it reuses.
const wayRoomBytes = 64 << 10

// candidate is one way of a unit: held in memory up to [wayRoomBytes] until
// the unit's way is known, then written into the spool. Past that, the JSON
// way goes on into a gzip member of its own (spilled), which the spool is cut
// back to where the way is not kept, so a way not kept takes no room; the text
// way, while the JSON way still holds, is let go (dropped), and a unit whose
// text was let go is read again as text alone within the first reading
// ([errTextAgain]). The second reading never reads the box.
type candidate struct {
	spool                   *unitSpool
	buf                     []byte
	drops, spilled, dropped bool
	// pending is what the scrub took out of what buf holds ([tallier]),
	// the spool's once the way is kept.
	pending int64
}

// newCandidate is a way of a unit onto spool, dropping past its room where
// drops is set; buf is the room its bytes are held in.
func newCandidate(spool *unitSpool, buf []byte, drops bool) *candidate {
	return &candidate{spool: spool, buf: buf[:0], drops: drops}
}

func (c *candidate) Write(p []byte) (int, error) {
	switch {
	case c.dropped:
		return len(p), nil
	case c.spilled:
		return c.spool.Write(p)
	case len(c.buf)+len(p) <= wayRoomBytes:
		c.buf = append(c.buf, p...)
		return len(p), nil
	case c.drops:
		c.dropped, c.buf, c.pending = true, c.buf[:0], 0
		return len(p), nil
	}
	if err := c.spool.newMember(); err != nil {
		return 0, err
	}
	if err := c.keep(); err != nil {
		return 0, err
	}
	return c.spool.Write(p)
}

// WriteString writes str, as Write does.
func (c *candidate) WriteString(str string) (int, error) {
	switch {
	case c.dropped:
		return len(str), nil
	case c.spilled:
		return c.spool.WriteString(str)
	case len(c.buf)+len(str) <= wayRoomBytes:
		c.buf = append(c.buf, str...)
		return len(str), nil
	}
	return c.Write([]byte(str))
}

// WriteByte writes b, as Write does.
func (c *candidate) WriteByte(b byte) error {
	if !c.dropped && !c.spilled && len(c.buf) < wayRoomBytes {
		c.buf = append(c.buf, b)
		return nil
	}
	_, err := c.Write([]byte{b})
	return err
}

// keep puts the way in the spool, kept, writing on there; never a way that
// dropped, whose unit is read again alone instead ([errTextAgain]).
func (c *candidate) keep() error {
	if c.spilled {
		return nil
	}
	c.spilled = true
	_, err := c.spool.Write(c.buf)
	c.spool.tally(c.pending)
	c.buf, c.pending = c.buf[:0], 0
	return err
}

func (c *candidate) tally(taken int64) {
	switch {
	case c.dropped:
	case c.spilled:
		c.spool.tally(taken)
	default:
		c.pending += taken
	}
}

// discard lets the way go: what it spilled into a member of its own is cut
// back out of the spool.
func (c *candidate) discard() error {
	c.buf, c.pending = c.buf[:0], 0
	if !c.spilled {
		return nil
	}
	c.spilled, c.dropped = false, true
	return c.spool.cutBack()
}

// writeMemberHeader writes what the second reading needs of a member's header
// to the spool, ahead of its body: its name as kept, its mode and its time.
// The kind and the body's size are the plan's ([tarPlan.members]).
func writeMemberHeader(w io.Writer, name string, mode int64, modTime time.Time) error {
	stamp, err := modTime.MarshalBinary()
	if err != nil {
		return fmt.Errorf("executor: the session's archive member %s: its time: %w", memberNamed(name), err)
	}
	rec := binary.AppendUvarint(nil, uint64(len(name)))
	rec = append(rec, name...)
	rec = binary.AppendUvarint(rec, uint64(mode))
	rec = binary.AppendUvarint(rec, uint64(len(stamp)))
	rec = append(rec, stamp...)
	_, err = w.Write(rec)
	return err
}

// errSpoolRead is a spool the second reading reads otherwise than its first
// wrote it: a fault of the scrub's own, never of the box's archive, which it
// reads no more.
var errSpoolRead = errors.New("executor: the scrub's spool read back otherwise than it was written")

// readMemberHeader reads back what [writeMemberHeader] wrote, of kind.
func readMemberHeader(r io.ByteReader, kind byte) (tar.Header, error) {
	text := func() ([]byte, error) {
		n, err := binary.ReadUvarint(r)
		if err != nil || n > uint64(scrubPlainMax) {
			return nil, errSpoolRead
		}
		b := make([]byte, n)
		for i := range b {
			if b[i], err = r.ReadByte(); err != nil {
				return nil, errSpoolRead
			}
		}
		return b, nil
	}
	name, err := text()
	if err != nil {
		return tar.Header{}, err
	}
	mode, err := binary.ReadUvarint(r)
	if err != nil {
		return tar.Header{}, errSpoolRead
	}
	stamp, err := text()
	if err != nil {
		return tar.Header{}, err
	}
	h := tar.Header{Typeflag: kind, Name: string(name), Mode: int64(mode)}
	if err = h.ModTime.UnmarshalBinary(stamp); err != nil {
		return tar.Header{}, errSpoolRead
	}
	return h, nil
}

// spoolCopy reads a spool back as the session is kept: every byte the first
// reading wrote, less the loaded tool definitions a line's cut takes
// ([tarPlan.addCut], [cutFilter]), each payload the plan keeps apart read
// alone, its member opened on split before its first byte ([memberSplit]).
type spoolCopy struct {
	r    *bufio.Reader
	plan *tarPlan
	// at is how many bytes of the spool are read; next is where the plan's
	// next cut's line begins, read off it ahead of its place (pending).
	at      int64
	next    spoolCut
	pending bool
	// cut is the line a cut is being applied to, nil between them.
	cut *cutFilter
	// split is where the copy's payloads open their members; media is the
	// plan's next payload, and mediaEnd where the one being read ends, 0
	// between payloads.
	split    *memberSplit
	media    int
	mediaEnd int64
}

// spoolCut is one cut of the plan: of the line the spool holds from at,
// which takes taken bytes from it.
type spoolCut struct {
	at, taken int64
	cut       *definitionCut
}

// newSpoolCopy reads spool as plan's cuts keep it, each payload it keeps
// apart opening its member on split.
func newSpoolCopy(spool io.Reader, plan *tarPlan, split *memberSplit) *spoolCopy {
	return &spoolCopy{r: bufio.NewReaderSize(spool, 64<<10), plan: plan, split: split}
}

// ReadByte reads the next byte kept.
func (c *spoolCopy) ReadByte() (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func (c *spoolCopy) Read(p []byte) (int, error) {
	for {
		if c.cut != nil {
			n, err := c.cut.read(c, p)
			if n > 0 || err != nil {
				return n, err
			}
			continue
		}
		if !c.pending {
			if err := c.readCut(); err != nil {
				return 0, err
			}
		}
		if c.pending && c.next.at == c.at {
			c.pending = false
			c.cut = &cutFilter{cut: c.next.cut, taken: c.next.taken}
			continue
		}
		want := len(p)
		if c.pending {
			want = int(min(int64(want), c.next.at-c.at))
		}
		bound, err := c.mediaBound()
		if err != nil {
			return 0, err
		}
		if bound >= 0 {
			want = int(min(int64(want), bound))
		}
		n, err := c.r.Read(p[:want])
		c.at += int64(n)
		if errors.Is(err, io.EOF) && c.pending {
			return n, errSpoolRead
		}
		return n, err
	}
}

// mediaBound is how far the copy reads before the plan's next payload begins
// or the one being read ends, -1 for no bound: where a payload begins, its
// member opened first ([memberSplit.media]).
func (c *spoolCopy) mediaBound() (int64, error) {
	if c.at < c.mediaEnd {
		return c.mediaEnd - c.at, nil
	}
	found := c.plan.media.found
	if c.media == len(found) {
		return -1, nil
	}
	next := found[c.media]
	switch {
	case next.at > c.at:
		return next.at - c.at, nil
	case next.at < c.at:
		return 0, errSpoolRead
	}
	if err := c.split.media(c.media); err != nil {
		return 0, err
	}
	c.media++
	c.mediaEnd = next.at + next.n
	return next.n, nil
}

// readCut reads the plan's next cut, none where none is left.
func (c *spoolCopy) readCut() error {
	e := &c.plan.edits
	if e.done() {
		return nil
	}
	gap, placed := e.takeNumber()
	list, listed := e.takeNumber()
	n, counted := e.takeNumber()
	taken, weighed := e.takeNumber()
	if !placed || !listed || !counted || !weighed {
		return errSpoolRead
	}
	c.next = spoolCut{at: c.plan.editRead + int64(gap), taken: int64(taken),
		cut: &definitionCut{list: int(list), n: int(n), bits: e}}
	c.plan.editRead = c.next.at
	c.pending = true
	return nil
}

// done says the spool and its plan's edits are both read to their ends.
func (c *spoolCopy) done() bool {
	if c.pending || c.cut != nil || !c.plan.edits.done() || c.media < len(c.plan.media.found) {
		return false
	}
	_, err := c.r.ReadByte()
	return errors.Is(err, io.EOF)
}

// cutFilter applies a line's cut to the line as the first reading wrote it,
// whole: the rewrite writes a JSON value compactly, so the list the cut
// applies to is the container of its place among the line's ([jsonRewrite]
// counts them as it opens them), and its elements are what lies between its
// own commas. The elements cut are left out, and the commas written again
// between the ones kept, as the rewrite writes a list it cuts as it goes:
// what it writes is what [jsonRewrite] writes of the line with that cut,
// which the plan's count of the bytes it takes holds it to.
type cutFilter struct {
	cut   *definitionCut
	taken int64
	// containers is how many containers of the line are open so far, depth
	// how deep the reading is, listDepth the list's own depth once it is
	// open; inString and escaped track a string's bytes.
	containers, depth, listDepth int
	inString, escaped            bool
	// element says an element of the list is being read; dropping that it
	// is cut, kept how many of its elements are written, removed how many
	// bytes the cut took, and closed that the list has closed.
	element, dropping, closed bool
	kept                      int
	removed                   int64
	// hold is what is read but not yet handed on: a comma written again
	// and the byte it goes before.
	hold [2]byte
	held int
}

// read reads the next bytes of the line through the cut, and ends the cut
// once its list has closed, where the copy reads on as it came.
func (f *cutFilter) read(c *spoolCopy, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if f.held > 0 {
			p[n] = f.hold[0]
			f.hold[0] = f.hold[1]
			f.held--
			n++
			continue
		}
		if f.closed {
			if f.removed != f.taken || f.cut.read != f.cut.n {
				return n, errSpoolRead
			}
			c.cut = nil
			return n, nil
		}
		b, err := c.r.ReadByte()
		if err != nil {
			return n, errSpoolRead
		}
		c.at++
		comma, keep, err := f.step(b)
		if err != nil {
			return n, err
		}
		if comma {
			f.hold[f.held] = ','
			f.held++
		}
		if keep {
			f.hold[f.held] = b
			f.held++
		}
	}
	return n, nil
}

// step reads b, one byte of the line: whether it is kept, and whether a
// comma is written again before it.
func (f *cutFilter) step(b byte) (comma, keep bool, err error) {
	if f.inString {
		switch {
		case f.escaped:
			f.escaped = false
		case b == '\\':
			f.escaped = true
		case b == '"':
			f.inString = false
		}
		return false, f.keepByte(), nil
	}
	if f.listDepth > 0 && f.depth == f.listDepth {
		switch b {
		case ',':
			f.element, f.dropping = false, false
			f.removed++
			return false, false, nil
		case ']':
			// THE COMMAS ARE WRITTEN AGAIN: every one read was taken, and
			// as many as join the elements kept are back.
			f.depth--
			f.closed = true
			f.removed -= int64(max(f.kept-1, 0))
			return false, true, nil
		}
		if !f.element {
			cut, cutErr := f.cut.next()
			if cutErr != nil {
				return false, false, errSpoolRead
			}
			f.element, f.dropping = true, cut
			if !cut {
				f.kept++
				comma = f.kept > 1
			}
		}
	}
	keep = f.keepByte()
	switch b {
	case '"':
		f.inString = true
	case '{', '[':
		f.depth++
		if f.listDepth == 0 {
			if f.containers++; f.containers == f.cut.list {
				if b != '[' {
					return false, false, errSpoolRead
				}
				f.listDepth = f.depth
			}
		}
	case '}', ']':
		f.depth--
	}
	return comma, keep, nil
}

// keepByte says the byte read is kept, counting it taken where it is not.
func (f *cutFilter) keepByte() bool {
	if f.dropping {
		f.removed++
		return false
	}
	return true
}
