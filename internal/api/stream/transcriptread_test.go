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
// come, is ended there by the engine's own read, taking nothing after the
// cancel but what it had asked for, with no answer sent, closing its archive;
// and every transcript slot ([executor.TranscriptReadsAtOnce]) is free, each
// taken by the reads asked next. Each is given up on once inside what it reads
// (#1904); a frame's wait ending a question is TestAQueryItsClientStoppedWaitingForStopsAndFreesItsSlot's.
//
// Mutation: stream a value past its read's context, and a read takes more than
// [streamedPast] after its cancel, still ending cancelled with no answer; keep
// a slot past a read its client left, and the reads asked next never read.
func TestAReadItsClientLeavesEndsAtTheEngineAndFreesItsSlots(t *testing.T) {
	t.Parallel()
	type ended struct {
		err         error
		opened      bool
		closed      <-chan struct{}
		afterCancel *int
	}
	ends := make(chan ended, executor.TranscriptReadsAtOnce)
	inside := make(chan struct{}, executor.TranscriptReadsAtOnce)
	f := newSocket(t, nil, func(ctx context.Context, what string, params map[string]any) (any, error) {
		if what != "executor_session" {
			return "answered", nil
		}
		closed := make(chan struct{})
		opened, afterCancel := false, 0
		reading := sync.OnceFunc(func() { inside <- struct{}{} })
		open := func(ctx context.Context) (io.ReadCloser, error) {
			opened = true
			if params["archive"] == "endless" {
				return endlessArchive(ctx, reading, &afterCancel, closed), nil
			}
			return stalledArchive{ctx: ctx, inside: reading,
				closing: sync.OnceFunc(func() { close(closed) })}, nil
		}
		got, err := executor.ReadTranscript(ctx, executor.OpenCodeName, executor.OpenCode{}.ArchiveFormat(),
			open, 0)
		ends <- ended{err: err, opened: opened, closed: closed, afterCancel: &afterCancel}
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
			awaited(t, inside, "a read had not begun reading its "+archive+" archive: a slot a read its "+
				"client left held is kept")
		}
		for i := range executor.TranscriptReadsAtOnce {
			write(t, conn, map[string]any{"kind": "query_cancel", "id": first + i})
		}
		for range executor.TranscriptReadsAtOnce {
			e := awaited(t, ends, "a read its client left had not ended")
			if !errors.Is(e.err, context.Canceled) || !e.opened {
				t.Fatalf("a read of the %s archive, left by its client, ended %v (its archive opened: %t), "+
					"want cancelled, mid-read", archive, e.err, e.opened)
			}
			awaited(t, e.closed, "a read its client left had not closed its "+archive+" archive")
			if *e.afterCancel > streamedPast {
				t.Fatalf("a read of the %s archive, left by its client, took %d bytes of it after its cancel, "+
					"want at most %d: it streamed a value past its context", archive, *e.afterCancel,
					streamedPast)
			}
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
// gzipped as it is read. Each piece is flushed into a pipe, which hands it
// over only as the read asks, so once the read has taken [streamedPast] of the
// text its walk is streaming the value, where only the read's own ctx can end
// it: inside is told then, and nothing more is written until ctx is done.
// afterCancel counts the pieces the read takes whole after that, and the text
// ends once it passes streamedPast, so a read that streams on past its context
// fails on what it took, not on the time its read budget takes to spend. done
// closes once nothing writes the archive any more, afterCancel then final.
func endlessArchive(ctx context.Context, inside func(), afterCancel *int, done chan<- struct{}) io.ReadCloser {
	r, w := io.Pipe()
	go func() {
		defer close(done)
		zw := gzip.NewWriter(w)
		_, err := io.WriteString(zw, `{"info":{"id":"ses_0123456789abcdef0123456789abcdef"},"messages":[`+
			`{"id":"msg_0","type":"user","text":"`)
		words := strings.Repeat("words a box wrote, and wrote again ", 1<<10)
		piece := func() error {
			if _, err := io.WriteString(zw, words); err != nil {
				return err
			}
			return zw.Flush()
		}
		for taken := 0; err == nil && taken < streamedPast; taken += len(words) {
			err = piece()
		}
		if err == nil {
			inside()
			<-ctx.Done()
		}
		for err == nil && *afterCancel <= streamedPast {
			if err = piece(); err == nil {
				*afterCancel += len(words)
			}
		}
		_ = w.CloseWithError(err)
	}()
	return r
}

// streamedPast is how much of an endless text a read has taken once its walk
// is past the text's opening: a mebibyte, ten times all a read buffers ahead
// of its walk (gzip's 4 KiB reader and 32 KiB window, the scanner's 64 KiB).
// Told at the open, a cancel can land before the walk reaches the value, where
// the walk's own check between messages ends it and no value streams past it.
// It bounds what a read takes after its cancel too: one that stops takes at
// most the piece its read in flight asked for, and one that streams on would
// take its whole read budget ([executor.TranscriptReadBytes], 256 MiB).
const streamedPast = 1 << 20

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
