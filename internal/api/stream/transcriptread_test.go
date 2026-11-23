package stream_test

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A READ ITS CLIENT LEAVES ENDS AT THE ENGINE, AND FREES ITS SLOTS: a
// transcript page whose read is still working when its client gives it up
// (`query_cancel`), one value that never ends or an archive whose bytes never
// come, is ended there by the engine's own read, with no answer sent, closing
// its archive; and every transcript slot ([executor.TranscriptReadsAtOnce]) is
// free, each taken by the reads asked next. Each is given up on once it opened its archive, so it
// ends mid-read at any runner's pace (#1904); a frame's wait ending a question
// is TestAQueryItsClientStoppedWaitingForStopsAndFreesItsSlot's.
//
// Mutation: stream a value past its read's context, and the reads run on to
// the read budget, answered; keep a slot past a read its client left, and the
// reads asked next never read.
func TestAReadItsClientLeavesEndsAtTheEngineAndFreesItsSlots(t *testing.T) {
	t.Parallel()
	type ended struct {
		err    error
		opened bool
		closed <-chan struct{}
	}
	ends := make(chan ended, executor.TranscriptReadsAtOnce)
	inside := make(chan struct{}, executor.TranscriptReadsAtOnce)
	f := newSocket(t, nil, func(ctx context.Context, what string, params map[string]any) (any, error) {
		if what != "executor_session" {
			return "answered", nil
		}
		closed := make(chan struct{})
		opened := false
		open := func(ctx context.Context) (io.ReadCloser, error) {
			opened = true
			if params["archive"] == "endless" {
				inside <- struct{}{}
				return endlessArchive(closed), nil
			}
			return stalledArchive{ctx: ctx, inside: sync.OnceFunc(func() { inside <- struct{}{} }),
				closing: sync.OnceFunc(func() { close(closed) })}, nil
		}
		got, err := executor.ReadTranscript(ctx, executor.OpenCodeName, executor.OpenCode{}.ArchiveFormat(),
			open, 0)
		ends <- ended{err: err, opened: opened, closed: closed}
		return got, err
	})
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)

	// Each round's reads take every slot, so a read asked in the second
	// opens only once a read of the first has let its slot go.
	for round, archive := range []string{"endless", "stalled"} {
		first := 10 * round
		for i := range executor.TranscriptReadsAtOnce {
			write(t, conn, map[string]any{"kind": "query", "id": first + i, "what": "executor_session",
				"params": map[string]any{"archive": archive}, "wait_ms": 600_000})
		}
		for range executor.TranscriptReadsAtOnce {
			awaited(t, inside, "a read had not opened its "+archive+" archive: a slot a read its client "+
				"left held is kept")
		}
		for i := range executor.TranscriptReadsAtOnce {
			write(t, conn, map[string]any{"kind": "query_cancel", "id": first + i})
		}
		for range executor.TranscriptReadsAtOnce {
			e := awaited(t, ends, "a read its client left had not ended")
			if !errors.Is(e.err, context.Canceled) || !e.opened {
				t.Fatalf("a read of a %s archive its client left ended %v (its archive opened: %t), "+
					"want cancelled, mid-read", archive, e.err, e.opened)
			}
			awaited(t, e.closed, "a read its client left had not closed its "+archive+" archive")
		}
	}
	write(t, conn, map[string]any{"kind": "query", "id": 100, "what": "events"})
	if got := next(t, conn); kindOf(got) != stream.KindResult || got["id"] != float64(100) {
		t.Fatalf("the frame after the reads that ended = %v, want the next question's answer and none of "+
			"theirs", got)
	}
}

// awaited is what c hands over, or the test's end naming what never came, so a
// read that runs on or a slot kept fails saying which rather than hanging the
// package to its timeout.
func awaited[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(time.Minute): //fixedwait:liveness
		t.Fatalf("a minute on, %s", what)
	}
	var none T
	return none
}

// endlessArchive is an OpenCode session whose one message's text never ends,
// gzipped as it is read: a read of it runs until its caller leaves, or to its
// read budget. done closes once nothing writes it any more, its reader closed.
func endlessArchive(done chan<- struct{}) io.ReadCloser {
	r, w := io.Pipe()
	go func() {
		defer close(done)
		zw := gzip.NewWriter(w)
		_, err := io.WriteString(zw, `{"info":{"id":"ses_0123456789abcdef0123456789abcdef"},"messages":[`+
			`{"id":"msg_0","type":"user","text":"`)
		words := strings.Repeat("words a box wrote, and wrote again ", 1<<10)
		for err == nil {
			_, err = io.WriteString(zw, words)
		}
		_ = w.CloseWithError(err)
	}()
	return r
}

// stalledArchive is an archive whose bytes never come: a read of it says so on
// inside and waits for its caller to leave, and closing says it was closed.
type stalledArchive struct {
	ctx     context.Context
	inside  func()
	closing func()
}

func (a stalledArchive) Read([]byte) (int, error) {
	a.inside()
	<-a.ctx.Done()
	return 0, a.ctx.Err()
}

func (a stalledArchive) Close() error {
	a.closing()
	return nil
}
