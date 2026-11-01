package checkrun_test

import (
	"context"
	"crypto/sha256"
	"io"
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
