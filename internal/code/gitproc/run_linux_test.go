package gitproc_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A RUN THE NODE ENDS COUNTS WHAT ITS WHOLE GROUP SPENT: an upload-pack whose
// client went away half way through a clone is killed with its group, and
// dies without reaping its pack-objects — which spent every bit of the
// counting, the delta search and half the writing the clone had cost by then
// — so the reap's own figure is upload-pack alone and would read a clone
// abandoned at the end of its work as free. The figure is read from the
// group before the signal: at least half what the same pack-objects spends
// run to its end by the pool, since it had written at least half the pack
// when its client stopped reading, and had done all of the rest of its work
// before its first byte.
//
// Half the pack is still to be written when the client stops — more than the
// pipes between it and the client hold — so pack-objects is still running
// when the group is signalled, the child the leader never reaps.
//
// Mutation: drop the reading before the signal (Result.CPU the reap's alone)
// and this fails — measured, 3.8 ms counted against 527 ms run to its end.
//
// Linux alone, as the reading is: XNU keeps a process's CPU where only
// libproc reads it, and the engine builds without cgo, so procgroup.GroupCPU
// is errors.ErrUnsupported on darwin and a killed run there counts the reap's
// figure alone — upload-pack without the pack-objects it never reaped, which
// is what this case refuses (the macOS job read 6.33 ms against 402 ms).
func TestARunTheNodeEndsCountsWhatItsGroupSpent(t *testing.T) {
	t.Parallel()
	dir, tip := looseHistory(t)
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	p := realPool(t, gitproc.Options{Metrics: rec})
	var pack countingWriter
	direct, err := p.Run(t.Context(), gitproc.Cmd{
		Dir: dir, Args: []string{"pack-objects", "--revs", "--stdout", "--delta-base-offset", "-q"},
		Stdin: strings.NewReader(tip + "\n"), Stdout: &pack, Class: gitproc.ClassRead, Deadline: time.Minute,
	})
	if err != nil {
		t.Fatalf("pack-objects of the history: %v", err)
	}
	if direct.CPU < 100*time.Millisecond {
		t.Fatalf("pack-objects of the history spent %v of CPU, under the tenth of a second this case needs "+
			"to tell a group from its leader: the fixture is too small", direct.CPU)
	}

	client := &haltingWriter{after: pack.n / 2, halted: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := pkt("command=fetch\n") + "0001" + pkt("ofs-delta\n") + pkt("no-progress\n") +
		pkt("want "+tip+"\n") + pkt("done\n") + "0000"
	done := start(ctx, p, gitproc.Cmd{
		Dir: dir, Args: []string{"upload-pack", "--stateless-rpc", dir}, Protocol: "version=2",
		Stdin: strings.NewReader(request), Stdout: client, Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: time.Minute,
	})
	select {
	case <-client.halted:
	case o := <-done:
		t.Fatalf("upload-pack ended (%v) before its client had read half the pack", o.err)
	case <-time.After(hang):
		t.Fatalf("upload-pack had not sent half the pack's %d bytes after %v", pack.n, hang)
	}
	// The client goes away, its Write still blocked: the leader is reaped —
	// its group signalled — while the client holds the run open, as a
	// client's connection does.
	cancel()
	awaitGauge(t, rec, metrics.CodeGitProcesses, "transfer", 0)
	close(client.release)
	o := await(t, done, "an upload-pack whose client went away")
	if !errors.Is(o.err, context.Canceled) {
		t.Fatalf("an upload-pack whose client went away: %v, want context.Canceled", o.err)
	}
	t.Logf("pack-objects run to its end spent %v of CPU; upload-pack killed with half the pack read, %v",
		direct.CPU, o.res.CPU)
	if o.res.CPU < direct.CPU/2 {
		t.Errorf("upload-pack killed half way through a clone counts %v of CPU, and the same pack-objects run "+
			"to its end %v: what its group spent before the signal is not counted", o.res.CPU, direct.CPU)
	}
}

// haltingWriter is a client that reads a stream until it has `after` bytes,
// closes halted, and then takes nothing more until release closes.
type haltingWriter struct {
	after   int64
	n       int64
	halted  chan struct{}
	release chan struct{}
}

func (w *haltingWriter) Write(p []byte) (int, error) {
	if w.n >= w.after {
		<-w.release
		return len(p), nil
	}
	w.n += int64(len(p))
	if w.n >= w.after {
		close(w.halted)
	}
	return len(p), nil
}
