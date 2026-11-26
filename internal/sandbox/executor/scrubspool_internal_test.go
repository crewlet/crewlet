package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// unboundSpool is a take's spool in dir whose room never refuses.
func unboundSpool(dir string) sandbox.SessionSpool {
	return sandbox.SessionSpool{Dir: dir, Take: func(int64) error { return nil }, Give: func(int64) {},
		Spooled: func() {}}
}

// spoolLedger is a take's room as a test keeps it: what the spool took and gave
// back, refused past room (zero, no bound), and what it
// held as its first reading ended, by the room's count and on disk.
type spoolLedger struct {
	mu                     sync.Mutex
	room                   int64
	used, taken, freed     int64
	spooled, spooledOnDisk int64
	dir                    string
	// drained is each give after the first reading, in order.
	drained []int64
}

// place is the ledger's spool in dir.
func (l *spoolLedger) place(dir string) spoolPlace {
	l.dir, l.spooled = dir, -1
	return spoolPlace{dir: dir,
		take: func(n int64) error {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.room > 0 && l.used+n > l.room {
				return errors.New("the test's room is full")
			}
			l.used, l.taken = l.used+n, l.taken+n
			return nil
		},
		give: func(n int64) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.used, l.freed = l.used-n, l.freed+n
			if l.spooled >= 0 {
				l.drained = append(l.drained, n)
			}
		},
		spooled: func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.spooled, l.spooledOnDisk = l.used, onDisk(l.dir)
		},
	}
}

// onDisk is what the scrub's spool files in dir hold, -1 where it cannot be read.
func onDisk(dir string) int64 {
	files, err := filepath.Glob(filepath.Join(dir, unitSpoolPrefix+"*"))
	if err != nil {
		return -1
	}
	var n int64
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			return -1
		}
		n += info.Size()
	}
	return n
}

// spoolsLeft is the scrub's spools left in dir.
func spoolsLeft(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	return left
}

// THE SECOND READING COPIES THE SPOOL, NEVER THE BOX: a tar is written from
// what its first reading spooled, the box's archive opened again only where
// a first reading goes on from a unit (here a torn line), never for the
// second; so an archive the box rewrote once the first reading was done
// changes no byte kept, and what a reading that went on passed over in the
// spool is passed over in the copy.
//
// Mutation: copy the spool's edits' bytes, or write the second reading from
// the box again, and the kept transcript or the count of readings goes red.
func TestTheSecondReadingCopiesTheSpoolNeverTheBox(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{longValue})
	torn := `{"text":"` + longValue + ` ` + randomBase64(21, textWholeBytes+512)
	files := map[string]string{
		"projects/p/s.jsonl": `{"a":"` + longValue + `"}` + "\n" + torn + "\n" + `{"b":1}` + "\n",
		"tasks/s/1.json":     `{"k":"` + longValue + `"}`,
		"notes.txt":          "a note naming " + longValue + "\n",
	}
	archive := tarOf(t, files)
	rewritten := tarOf(t, map[string]string{"projects/p/s.jsonl": `{"z":1}` + "\n", "notes.txt": "other\n"})
	opened := 0
	reopen := func() (io.ReadCloser, error) {
		opened++
		if opened > 1 {
			return io.NopCloser(bytes.NewReader(rewritten)), nil
		}
		return io.NopCloser(bytes.NewReader(archive)), nil
	}
	var out bytes.Buffer
	if err := scrubTar(bytes.NewReader(archive), reopen, &out, s, testBounds); err != nil {
		t.Fatal(err)
	}
	if opened != 1 {
		t.Errorf("the box's archive was opened again %d times, want once, for the reading that went on", opened)
	}
	kept := untarOf(t, out.Bytes())
	wantLines := rewriteLine(t, `{"a":"`+longValue+`"}`, s) + "\n" + keptText(s, torn) + "\n" + `{"b":1}` + "\n"
	for name, want := range map[string]string{
		"projects/p/s.jsonl": wantLines,
		"tasks/s/1.json":     rewriteLine(t, files["tasks/s/1.json"], s),
		"notes.txt":          keptText(s, files["notes.txt"]),
	} {
		if kept[name] != want {
			t.Errorf("%s was kept as %.120q, want %.120q", name, kept[name], want)
		}
	}
}

// A UNIT WHOSE TEXT WAS LET GO IS READ AGAIN AS TEXT ALONE: from the first
// unit that is no JSON a reading reads each both ways, holding each way to
// [scrubPieceBaseBytes] until the unit's way is known; a long unit that is
// JSON throughout is kept as JSON with no reading more, and one that is no
// JSON past where its text was let go is read once more, as text alone, and
// kept as its text.
//
// Mutation: keep a JSON way spilled into the spool where its text is kept, or
// keep a text way that was let go, and a case keeps other bytes.
func TestAUnitWhoseTextWasLetGoIsReadAgainAsTextAlone(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{longValue})
	// PAST WHAT THE TEXT'S REDACTION HOLDS BACK, so its text way writes past
	// its room while its JSON still holds.
	long := randomBase64(22, 8<<20)
	for name, tc := range map[string]struct {
		line     string
		readings int
	}{
		"a long line that is JSON":            {`{"text":"` + longValue + ` ` + long + `"}`, 2},
		"a long line that is no JSON at last": {`{"text":"` + longValue + ` ` + long, 3},
	} {
		transcript := "no JSON\n" + tc.line + "\n"
		archive := tarOf(t, map[string]string{"projects/p/s.jsonl": transcript})
		kept, readings, err := scrubTake(archiveTar, archive, s, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := keptText(s, "no JSON") + "\n"
		if strings.HasSuffix(tc.line, `"}`) {
			want += rewriteLine(t, tc.line, s) + "\n"
		} else {
			want += keptText(s, tc.line) + "\n"
		}
		if got := untarOf(t, kept)["projects/p/s.jsonl"]; got != want || readings != tc.readings {
			t.Errorf("%s: kept as %.120q… in %d readings, want %.120q… in %d", name, got, readings, want,
				tc.readings)
		}
	}
}

// A SCRUB LEAVES NO SPOOL, HOWEVER IT ENDS: one that keeps the session, one
// whose box's archive fails in the middle of a unit, as a take's closed
// reader ends it, and one that panics there leave nothing in the spool's
// directory.
//
// Mutation: remove the spool only where the scrub keeps the session, or not
// on a panic, and a case leaves it.
func TestAScrubLeavesNoSpoolHoweverItEnds(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{longValue})
	archive := tarOf(t, map[string]string{"projects/p/s.jsonl": pdfLine(23, 600<<10) + "\n"})
	half := len(archive) / 2
	for name, src := range map[string]func() io.Reader{
		"kept":               func() io.Reader { return bytes.NewReader(archive) },
		"its archive failed": func() io.Reader { return &failAfter{r: bytes.NewReader(archive), n: half} },
		"it panicked":        func() io.Reader { return &failAfter{r: bytes.NewReader(archive), n: half, panics: true} },
	} {
		dir := t.TempDir()
		err := scrubContained(t.Context(), func() error {
			return scrubTar(src(), reopenOf(archive), io.Discard, s, testBounds.spooling(spoolPlace{dir: dir}))
		})
		if (err == nil) != (name == "kept") {
			t.Errorf("%s: the scrub ended %v", name, err)
		}
		if left := spoolsLeft(t, dir); len(left) != 0 {
			t.Errorf("%s: the scrub left %v in its spool's directory", name, left)
		}
	}
}

// failAfter reads r for n bytes, then fails, or panics where panics is set.
type failAfter struct {
	r      io.Reader
	n      int
	panics bool
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.n <= 0 {
		if f.panics {
			panic("the box's archive failed")
		}
		return 0, errors.New("the take's reader was closed")
	}
	n, err := f.r.Read(p[:min(len(p), f.n)])
	f.n -= n
	return n, err
}

// THE SPOOL IS HELD TO ITS ROOM: a session the spool's room holds as gzip
// writes input it cannot shrink ([executorkeep.GzipMostBytes]) is kept, and
// one whose spool would pass its room is refused before a byte past it is
// written, with nothing reaching the take's writer and no spool left.
//
// Mutation: drop the room's count, count it past the write, or bound gzip's
// worst at a stored block of 64 KiB, and a case goes red.
func TestTheSpoolIsHeldToItsRoom(t *testing.T) {
	t.Parallel()
	raw := make([]byte, 6<<20)
	_, _ = rand.Read(raw)
	body := string(raw)
	archive := tarOf(t, map[string]string{"blob": body})
	s := newScrubber(nil)
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	h, err := tar.NewReader(zr).Next()
	if err != nil {
		t.Fatal(err)
	}
	var rec bytes.Buffer
	if err = writeMemberHeader(&rec, memberName(s, h.Name), h.Mode&0o777, h.ModTime); err != nil {
		t.Fatal(err)
	}
	kept := keptText(s, body)
	spooled := int64(rec.Len() + len(kept))
	for name, tc := range map[string]struct {
		room int64
		kept bool
	}{
		"room for gzip at its worst": {executorkeep.GzipMostBytes(spooled), true},
		"room for the input alone":   {spooled, false},
	} {
		dir := t.TempDir()
		dst := &counted{}
		ledger := &spoolLedger{room: tc.room}
		err := scrubTar(bytes.NewReader(archive), reopenOf(archive), dst, s,
			testBounds.spooling(ledger.place(dir)))
		switch {
		case tc.kept && err != nil:
			t.Errorf("%s: the session was refused: %v", name, err)
		case !tc.kept && (!errors.Is(err, errUnitSpoolPastRoom) || dst.n != 0):
			t.Errorf("%s: the scrub ended %v having written %d bytes, want it refused for its room before "+
				"writing", name, err, dst.n)
		}
		if left := spoolsLeft(t, dir); len(left) != 0 || ledger.used != 0 {
			t.Errorf("%s: the scrub left %v, %d bytes of its room taken", name, left, ledger.used)
		}
	}
}

// THE CUT TAKES THE BYTES IT COUNTED, WHEREVER ITS ELEMENTS LIE: a loaded-tools
// line whose renamed definitions are its list's first, middle, last, first and
// last, or every one is copied less those elements and the commas that joined
// them, the commas between the ones kept written again, and the copy's bytes
// are those the first reading counted ([cutFilter]).
//
// Mutation: write again one comma too many or too few, or drop the comma
// after a first element cut, and a case's bytes or its copy go red.
func TestTheCutTakesTheBytesItCountedWhereverItsElementsLie(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{ltServer})
	for name, tc := range map[string]struct{ all, kept []string }{
		"first":          {[]string{ltRenamed, ltEcho, ltSend}, []string{ltEcho, ltSend}},
		"middle":         {[]string{ltEcho, ltRenamed, ltSend}, []string{ltEcho, ltSend}},
		"last":           {[]string{ltEcho, ltSend, ltRenamed}, []string{ltEcho, ltSend}},
		"first and last": {[]string{ltRenamed, ltEcho, ltRenamed}, []string{ltEcho}},
		"every one":      {[]string{ltRenamed, ltRenamed}, nil},
	} {
		plan := spooledPlan(t)
		size, err := measureLines(strings.NewReader(ltRecord("2.1.310", "", tc.all...)+"\n"), s, plan)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := strings.ReplaceAll(ltRecord("2.1.310", "", tc.kept...), ltServer, scrubMarker) + "\n"
		if got := copiedSpool(t, plan, size); got != want {
			t.Errorf("%s: copied as\n%s\nwant\n%s", name, got, want)
		}
	}
}

// A WAY NOT KEPT TAKES NO ROOM: a line that is JSON for a mebibyte of short
// fields and torn at its end has its JSON spooled past its way's room into a
// gzip member of its own, which is cut back out of the spool once the line is
// no JSON, so the spool the second reading reads holds the line's text alone,
// no larger than gzip makes of it. (One long string would never spill: its
// value is held until it closes.)
//
// Mutation: let a way go without cutting the spool back, and the reading that
// goes on from the line finds the spool past where the line began.
func TestAWayNotKeptTakesNoRoom(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{longValue})
	torn := "{" + strings.Repeat(`"k":"v",`, 1<<20/8)
	archive := tarOf(t, map[string]string{"projects/p/s.jsonl": torn + "\n"})
	ledger := &spoolLedger{}
	var out bytes.Buffer
	if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), &out, s,
		testBounds.spooling(ledger.place(t.TempDir()))); err != nil {
		t.Fatal(err)
	}
	spooled := ledger.spooled
	kept := untarOf(t, out.Bytes())["projects/p/s.jsonl"]
	if want := keptText(s, torn) + "\n"; kept != want {
		t.Fatalf("the torn line was kept as %.80q…, want its text", kept)
	}
	if bound := executorkeep.GzipMostBytes(int64(len(kept)) + 1<<10); spooled == 0 || spooled > bound {
		t.Errorf("the spool held %d bytes, past the %d its line's text comes to at most", spooled, bound)
	}
}

// THE SECOND READING GIVES BACK WHAT IT READ: a session whose spool spans
// several files has each removed and given back, in order, as the second
// reading reads past it, so what the spool holds and what the take has
// written beside it never pass the spool's size and one file; and by the
// scrub's end everything it took is back, nothing left on disk. Sampled on
// the take's own writes, so no wait decides it.
//
// Mutation: keep each file until the scrub ends, or give a file back twice,
// and a check goes red.
func TestTheSecondReadingGivesBackWhatItRead(t *testing.T) {
	t.Parallel()
	s := newScrubber(nil)
	archive := tarOf(t, map[string]string{"notes.txt": randomBase64(31, 5<<20) + "\n"})
	dir := t.TempDir()
	ledger := &spoolLedger{}
	worst := int64(0)
	dst := &watchedWriter{wrote: func(out int64) {
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		if ledger.spooled >= 0 {
			worst = max(worst, ledger.used+out-ledger.spooled)
		}
	}}
	if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), dst, s,
		testBounds.spooling(ledger.place(dir))); err != nil {
		t.Fatal(err)
	}
	if ledger.spooled < 3*segmentBytes {
		t.Fatalf("the spool held %d bytes, want at least three files' worth", ledger.spooled)
	}
	// THE TAKE'S OUTPUT IS THE SPOOL'S KEPT TEXT GZIPPED AGAIN, at the
	// spool's level, so what it writes tracks what it read within a block.
	if slack := int64(64 << 10); worst > segmentBytes+slack {
		t.Errorf("the spool and the take's output held %d bytes past the spool's size at once, want at "+
			"most one file (%d) and %d of gzip's slack", worst, segmentBytes, slack)
	}
	files := (ledger.spooled + segmentBytes - 1) / segmentBytes
	if got := int64(len(ledger.drained)); got != files {
		t.Errorf("the second reading gave the room back in %d steps %v, want one for each of its %d files",
			got, ledger.drained, files)
	}
	for i, n := range ledger.drained {
		if want := min(segmentBytes, ledger.spooled-int64(i)*segmentBytes); n != want {
			t.Errorf("give %d was %d bytes, want file %d's %d, in order", i, n, i, want)
		}
	}
	if ledger.used != 0 || ledger.freed != ledger.taken || len(spoolsLeft(t, dir)) != 0 {
		t.Errorf("the scrub ended holding %d bytes (took %d, gave back %d), files %v left", ledger.used,
			ledger.taken, ledger.freed, spoolsLeft(t, dir))
	}
}

// watchedWriter counts what is written to it and tells wrote each time.
type watchedWriter struct {
	n     int64
	wrote func(n int64)
}

func (w *watchedWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	w.wrote(w.n)
	return len(p), nil
}

// A CUT BACK SPANS FILES: a torn line of short fields, its JSON way spilled
// into a member that runs over several of the spool's files, is cut back out
// of the spool once it is no JSON; every file past the cut is removed and its
// bytes given back, so as the first reading ends the files on disk hold
// exactly what the take's room counts, and the line is kept as its text.
//
// Mutation: leave the files past the cut, or give back what the cut file
// held less than it lost, and the count goes red.
func TestACutBackSpansFiles(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{longValue})
	var torn strings.Builder
	torn.WriteString("{")
	for i := 0; torn.Len() < 6<<20; i++ {
		torn.WriteString(`"k":"` + randomBase64(uint64(i), 24) + `",`)
	}
	archive := tarOf(t, map[string]string{"projects/p/s.jsonl": torn.String() + "\n"})
	dir := t.TempDir()
	ledger := &spoolLedger{}
	var out bytes.Buffer
	if err := scrubTar(bytes.NewReader(archive), reopenOf(archive), &out, s,
		testBounds.spooling(ledger.place(dir))); err != nil {
		t.Fatal(err)
	}
	if want := keptText(s, torn.String()) + "\n"; untarOf(t, out.Bytes())["projects/p/s.jsonl"] != want {
		t.Fatal("the torn line was not kept as its text")
	}
	if cut := ledger.taken - ledger.spooled; cut < 2*segmentBytes {
		t.Fatalf("the first reading cut back %d bytes, want a member over at least two files", cut)
	}
	if ledger.spooledOnDisk != ledger.spooled {
		t.Errorf("as the first reading ended its files held %d bytes, want the %d its room counts",
			ledger.spooledOnDisk, ledger.spooled)
	}
	if ledger.used != 0 || len(spoolsLeft(t, dir)) != 0 {
		t.Errorf("the scrub ended holding %d bytes, files %v left", ledger.used, spoolsLeft(t, dir))
	}
}

// AN EARLIER MEMBER REWRITTEN BETWEEN READINGS IS KEPT AS THE FIRST READING READ
// IT: a box that rewrites a member, at the same length, before the one a first
// reading goes on in has the take keep that member as the first reading read
// and redacted it, the rewrite read by nothing ([tarPlan.memberAt]).
//
// Mutation: read a member before the resume point again, and the kept member
// is the rewrite's.
func TestAnEarlierMemberRewrittenBetweenReadingsIsKeptAsTheFirstReadingReadIt(t *testing.T) {
	t.Parallel()
	s := newScrubber([]string{longValue})
	transcript := `{"a":1}` + "\n" + `{"b":` + "\n"
	before := "a note naming " + longValue + "\n"
	after := strings.Repeat("z", len(before)-1) + "\n"
	first := tarOf(t, map[string]string{"a/notes.txt": before, "projects/p/s.jsonl": transcript})
	again := tarOf(t, map[string]string{"a/notes.txt": after, "projects/p/s.jsonl": transcript})
	var out bytes.Buffer
	if err := scrubTar(bytes.NewReader(first), reopenOf(again), &out, s, testBounds); err != nil {
		t.Fatal(err)
	}
	kept := untarOf(t, out.Bytes())["a/notes.txt"]
	if want := keptText(s, before); kept != want || strings.Contains(kept, longValue) {
		t.Errorf("the earlier member was kept as %q, want the first reading's, redacted: %q", kept, want)
	}
}

// THE SPOOL'S MEMBERS ARE COUNTED IN ITS ROOM: the take's room counts a gzip
// member for each [wayRoomBytes] of the plaintext a reading reads, each its
// frame, which an empty member at the spool's level does not pass.
//
// Mutation: count the members by another way's room or another bound, or
// frame them lighter than gzip writes one, and a check goes red.
func TestTheSpoolsMembersAreCountedInItsRoom(t *testing.T) {
	t.Parallel()
	if want := int64((scrubPlainMax/wayRoomBytes + 1) * executorkeep.GzipMemberBytes); executorkeep.SpoolMembersMostBytes != want {
		t.Errorf("the room counts %d bytes of members, want %d", executorkeep.SpoolMembersMostBytes, want)
	}
	var b bytes.Buffer
	zw, err := gzip.NewWriterLevel(&b, unitSpoolGzipLevel)
	if err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	if b.Len() > executorkeep.GzipMemberBytes {
		t.Errorf("an empty member is %d bytes, past the %d counted", b.Len(), executorkeep.GzipMemberBytes)
	}
}
