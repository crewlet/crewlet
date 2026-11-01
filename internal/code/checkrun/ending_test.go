package checkrun_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/checkrun"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// digestUploader is an object store that keeps nothing: the bytes and digest
// of what it was handed.
type digestUploader struct {
	n   int64
	sum [sha256.Size]byte
}

func (u *digestUploader) Put(_ context.Context, r io.Reader, _ int64, _ objstore.PutMeta) (objstore.Object, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return objstore.Object{}, err
	}
	u.n = n
	copy(u.sum[:], h.Sum(nil))
	return objstore.Object{Size: n}, nil
}

// A KEPT LOG READS ONLY THE BYTES IT KEEPS FROM THE BOX: a run whose output is
// a MiB past code.MaxCheckLogBytes keeps the output's last MaxCheckLogBytes,
// byte for byte, and the box hands out exactly those — never the MiB before
// them, which a remote box would send over the wire for nothing.
//
// Mutation: open the output from its start and read the earlier bytes away —
// the box hands out the whole output, red.
func TestAKeptLogReadsOnlyItsKeptBytesFromTheBox(t *testing.T) {
	t.Parallel()
	output := make([]byte, code.MaxCheckLogBytes+1<<20)
	for i := range output {
		output[i] = byte(i % 251)
	}
	box := sandbox.NewFakeSandbox("box-1")
	box.Put(checkrun.OutputFile, string(output))
	logs := &digestUploader{}
	log, why := checkrun.KeepLog(t.Context(), box, logs)
	if log == nil || why != "" {
		t.Fatalf("KeepLog = %+v, %q; want the log kept", log, why)
	}
	if logs.n != code.MaxCheckLogBytes || logs.sum != sha256.Sum256(output[1<<20:]) {
		t.Errorf("the kept log is %d bytes; want the output's last %d, byte for byte", logs.n,
			code.MaxCheckLogBytes)
	}
	if read := box.Delivered(); read != code.MaxCheckLogBytes {
		t.Errorf("keeping the log read %d bytes from the box; want the %d kept", read, code.MaxCheckLogBytes)
	}
}

// stallingUploader is an object store whose every put hangs until its caller
// gives up.
type stallingUploader struct{}

func (stallingUploader) Put(ctx context.Context, _ io.Reader, _ int64, _ objstore.PutMeta) (objstore.Object,
	error) {
	<-ctx.Done()
	return objstore.Object{}, ctx.Err()
}

// A RUN'S ENDING IS RECORDED WHILE ITS LOG STALLS: an object store that never
// answers costs a failed run its log, its summary saying so, within the
// ending's budget (Timing.End) — never its ending, which the home's sweep
// would otherwise take for a lost runner's and queue again beside the box.
//
// Mutation: read the ending under the run's own context, unbounded — the
// attempt never ends, red.
func TestARunsEndingIsRecordedWhileItsLogStalls(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Logs = stallingUploader{}
	o.Timing.End = 200 * time.Millisecond
	f.runner(o)
	key := f.queue(oidOf("a"), def("unit", time.Minute))
	box := f.container.box(t)
	f.container.end(box, 1, "FAIL web/cmd\n")
	row := f.ended(key)
	if row.State != code.CheckStateFailed || !strings.Contains(row.Summary, "its log could not be stored") {
		t.Fatalf("a failed run whose log stalled = %+v, want failed, its summary saying its log was not kept", row)
	}
}

// THE SWEEP WAITS OUT A RUN'S ENDING: the patience the home's sweep gives a
// running attempt past its timeout and its setup's grace covers the poll that
// finds its command ended, the reading of its ending, the asking for it to be
// recorded and a poll more — so a runner still reading or uploading what its
// run left is never taken for a lost one.
//
// Mutation: SweepPolls of four, with no time for the ending's reads, red.
func TestTheSweepWaitsOutARunsEnding(t *testing.T) {
	t.Parallel()
	ending := checkrun.CheckPoll + checkrun.Production().End + checkrun.FinishPolls*checkrun.CheckPoll +
		checkrun.CheckPoll
	if patience := checkrun.SweepPolls * checkrun.CheckPoll; patience < ending {
		t.Fatalf("the sweep waits %s past a run's timeout and grace; its ending may take %s", patience, ending)
	}
}

// wholeReportRefused is a box whose reports refuse a whole read: a reader of
// them that does not stream is caught.
type wholeReportRefused struct{ *sandbox.FakeSandbox }

func (b wholeReportRefused) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if p == checkrun.JUnitFile || p == checkrun.SARIFFile {
		return nil, errors.New("a report was read whole")
	}
	return b.FakeSandbox.ReadFile(ctx, p)
}

// junitPadded is a JUnit report of one passing case, exactly size bytes.
func junitPadded(size int) string {
	head, tail := `<testsuite tests="1" failures="0"><testcase classname="a" name="b"/>`, `</testsuite>`
	return head + strings.Repeat(" ", size-len(head)-len(tail)) + tail
}

// A REPORT IS STREAMED FROM THE BOX AND HELD TO THE CAP: a run's JUnit and
// SARIF reports are read as streams — never whole — into its failures; a
// report of exactly sandbox.MaxFileBytes is read; and one a byte past it is
// refused, saying so, as a whole read refuses it.
//
// Mutation: read the reports whole — every report unread, red; let the
// stream run past the cap — the report past it read, red.
func TestAReportIsStreamedFromTheBoxAndHeldToTheCap(t *testing.T) {
	t.Parallel()
	d := def("unit", time.Minute)
	d.Report.JUnit, d.Report.SARIF = "out/junit.xml", "out/lint.sarif"
	box := sandbox.NewFakeSandbox("box-1")
	box.Put(checkrun.JUnitFile, junitOf(2, 5, 3))
	box.Put(checkrun.SARIFFile, `{"runs":[{"results":[{"ruleId":"SA1000","message":{"text":"bad"}}]}]}`)
	got, unread := checkrun.Reports(t.Context(), wholeReportRefused{box}, d, "web")
	if len(unread) > 0 || len(got.Kept) != 3 || got.Failed != 2 || got.Results != 1 {
		t.Fatalf("the reports read %+v, unread %q; want two JUnit failures and one SARIF result", got, unread)
	}

	junitOnly := def("unit", time.Minute)
	junitOnly.Report.JUnit = "out/junit.xml"
	box.Put(checkrun.JUnitFile, junitPadded(sandbox.MaxFileBytes))
	if got, unread := checkrun.Reports(t.Context(), wholeReportRefused{box}, junitOnly, "web"); len(unread) > 0 ||
		got.Tests != 1 {
		t.Errorf("a report of exactly the cap read %+v, unread %q; want it read", got, unread)
	}
	box.Put(checkrun.JUnitFile, junitPadded(sandbox.MaxFileBytes+1))
	_, unread = checkrun.Reports(t.Context(), wholeReportRefused{box}, junitOnly, "web")
	if len(unread) != 1 || !strings.Contains(unread[0], fmt.Sprintf("past %d MiB", sandbox.MaxFileBytes>>20)) {
		t.Errorf("a report a byte past the cap: unread %q; want it refused for its size", unread)
	}
}

// sarifStream is a SARIF log of n failing results, made as it is read, so no
// copy of it is held but what its reader holds.
type sarifStream struct {
	n, next int
	buf     bytes.Buffer
	done    bool
	// produced counts the bytes handed out, and heap is the most heap in use
	// sampled every sampleEvery of them.
	produced, sampled int
	heap              uint64
}

// sampleEvery is how often the stream samples the heap: 4 MiB, a dozen
// samples of the case's log — each a stop of the world.
const sampleEvery = 4 << 20

func (s *sarifStream) Read(p []byte) (int, error) {
	for s.buf.Len() < len(p) && !s.done {
		if s.next == 0 {
			s.buf.WriteString(`{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"lint"}},"results":[`)
		}
		if s.next == s.n {
			s.buf.WriteString(`]}]}`)
			s.done = true
			continue
		}
		if s.next > 0 {
			s.buf.WriteString(",")
		}
		fmt.Fprintf(&s.buf, `{"ruleId":"R%d","level":"error","message":{"text":"%s"},"locations":[`+
			`{"physicalLocation":{"artifactLocation":{"uri":"web/pkg/f%d.go"},"region":{"startLine":%d}}}]}`,
			s.next, strings.Repeat("m", 200), s.next, s.next+1)
		s.next++
	}
	n, err := s.buf.Read(p)
	s.produced += n
	if s.produced-s.sampled >= sampleEvery {
		s.sampled = s.produced
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		s.heap = max(s.heap, m.HeapAlloc)
	}
	if err == io.EOF && !s.done {
		err = nil
	}
	return n, err
}

// A SARIF LOG IS READ A RESULT AT A TIME: a log of some 48 MiB, a hundred and
// fifty thousand failing results, is read with the heap growing by a fraction
// of it — the failures an ending keeps and the result in hand, never the log
// or every result decoded — and its results all counted.
//
// Not parallel, so the heap it samples is its own.
//
// Mutation: decode the log whole — the heap grows past the log's size, red.
func TestASARIFLogIsReadAResultAtATime(t *testing.T) {
	const results = 150_000
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	log := &sarifStream{n: results}
	got, err := checkrun.ReadSARIF(log, "web")
	if err != nil {
		t.Fatalf("ReadSARIF: %v", err)
	}
	if got.Results != results || len(got.Kept) == 0 || got.Kept[0].Path != "pkg/f0.go" {
		t.Fatalf("the log read %d results, kept %d (first %+v); want all %d counted", got.Results,
			len(got.Kept), got.Kept, results)
	}
	if log.produced < 40<<20 {
		t.Fatalf("the log is %d bytes; the case is about one past 40 MiB", log.produced)
	}
	if grew := int64(log.heap) - int64(before.HeapAlloc); grew > 16<<20 {
		t.Errorf("reading a %d MiB log grew the heap by %d MiB; want under 16 MiB", log.produced>>20, grew>>20)
	}
}
