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

// A READ PAST ITS CLIENT'S WAIT ENDS AT THE ENGINE, AND FREES ITS SLOTS: a
// transcript page whose read is still working when the wait its frame names
// (`wait_ms`) passes, one value that never ends, is ended there by the
// engine's own read, with no answer sent; the read closes its archive, so
// nothing of it runs on; and every one of the node's transcript slots
// ([executor.TranscriptReadsAtOnce]) is free, each taken by the reads asked
// next, which a `query_cancel` ends at once in turn.
//
// Mutation: ask a question under the socket's context alone, or stream a value
// past its read's context, and the reads run on past their wait to the read
// budget, answered; keep a slot past a read its wait ended, and the reads asked
// next never read.
func TestAReadPastItsClientsWaitEndsAtTheEngineAndFreesItsSlots(t *testing.T) {
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

	for i := range executor.TranscriptReadsAtOnce {
		write(t, conn, map[string]any{"kind": "query", "id": i, "what": "executor_session",
			"params": map[string]any{"archive": "endless"}, "wait_ms": 1000})
	}
	for range executor.TranscriptReadsAtOnce {
		e := awaited(t, ends, "a read past its wait had not ended")
		if !errors.Is(e.err, context.DeadlineExceeded) || !e.opened {
			t.Fatalf("a read past its wait ended %v (its archive opened: %t), want its wait's deadline, "+
				"read once it had begun", e.err, e.opened)
		}
		awaited(t, e.closed, "a read past its wait had not closed its archive")
	}

	// EVERY SLOT FREE: each read asked next reads its archive, which it could
	// only do holding one of the slots the reads past their wait held.
	for i := range executor.TranscriptReadsAtOnce {
		write(t, conn, map[string]any{"kind": "query", "id": 10 + i, "what": "executor_session",
			"params": map[string]any{"archive": "stalled"}, "wait_ms": 600_000})
	}
	for range executor.TranscriptReadsAtOnce {
		awaited(t, inside, "a read asked next had not read its archive: a slot a read past its wait held "+
			"is kept")
	}
	for i := range executor.TranscriptReadsAtOnce {
		write(t, conn, map[string]any{"kind": "query_cancel", "id": 10 + i})
	}
	for range executor.TranscriptReadsAtOnce {
		e := awaited(t, ends, "a read its client cancelled had not ended")
		if !errors.Is(e.err, context.Canceled) {
			t.Fatalf("a read its client cancelled ended %v, want cancelled", e.err)
		}
		awaited(t, e.closed, "a read its client cancelled had not closed its archive")
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
