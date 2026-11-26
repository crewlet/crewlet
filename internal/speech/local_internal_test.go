package speech

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedLocal is a [Local] whose library and model are nothing and whose
// worker is this test binary as a scripted worker of mode: the real process,
// wire and lifecycle, with no native code. spawns counts started workers.
func scriptedLocal(t *testing.T, capacity int, mode string) (*Local, *atomic.Int32) {
	t.Helper()
	l := NewLocal(LocalConfig{Capacity: capacity})
	var spawns atomic.Int32
	l.native = nativePin{}
	l.library = func(context.Context, func(float64)) (string, error) { return "scripted", nil }
	l.locate = func(context.Context, func(float64)) (string, error) { return t.TempDir(), nil }
	l.command = func(string, string) (*exec.Cmd, error) {
		spawns.Add(1)
		return exec.Command(os.Args[0], scriptedWorkerCommand, mode), nil //nolint:gosec // the test binary
	}
	t.Cleanup(l.Close)
	return l, &spawns
}

func open(t *testing.T, l *Local) Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	s, err := l.Open(ctx, Options{Lang: Lang})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func drain(t *testing.T, s Stream) []Result {
	t.Helper()
	var out []Result
	timeout := time.After(waitTimeout) //fixedwait:liveness
	for {
		select {
		case r, ok := <-s.Results():
			if !ok {
				return out
			}
			out = append(out, r)
		case <-timeout:
			t.Fatal("the stream's results never closed")
		}
	}
}

// A STREAM ON THE WORKER CARRIES PARTIALS, THEN ITS FINAL AT THE FINISH, and
// closes clean: the worker protocol end to end through a real child process.
func TestAStreamCarriesPartialsThenItsFinal(t *testing.T) {
	t.Parallel()
	l, _ := scriptedLocal(t, 4, scriptedOK)
	s := open(t, l)
	for range 2 {
		if err := s.Send(FakeSpeech(100 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	got := drain(t, s)
	want := []Result{{Text: "word1"}, {Text: "word1 word2"}, {Final: true, Text: "Said 2 words."}}
	if len(got) != len(want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := s.Err(); err != nil {
		t.Errorf("a clean finish reported %v", err)
	}
}

// PAST CAPACITY, OPEN IS BUSY, and a closed stream gives its place back.
func TestPastCapacityOpenIsBusy(t *testing.T) {
	t.Parallel()
	l, spawns := scriptedLocal(t, 2, scriptedOK)
	first, second := open(t, l), open(t, l)
	if _, err := l.Open(t.Context(), Options{Lang: Lang}); !errors.Is(err, ErrBusy) {
		t.Fatalf("a third stream at capacity 2 opened with %v, want ErrBusy", err)
	}
	first.Close()
	third := open(t, l)
	second.Close()
	third.Close()
	if n := spawns.Load(); n != 1 {
		t.Errorf("%d workers started for streams that overlapped, want one shared", n)
	}
}

// A WORKER THAT DIES FAILS ITS STREAMS WITH ErrLost, and a dictation opened
// the moment one of them learns it starts a new worker: a crash in native
// code ends dictations, never the engine. Each crash is a fresh chance at the
// window between a stream's end and the recognizer letting go of the dead
// worker. Mutations: end the streams with a nil error, and the first stream
// reads as a clean finish; end them before the recognizer lets go, and an
// open lands on the dead worker.
func TestAWorkerThatDiesFailsItsStreamsAndIsReplaced(t *testing.T) {
	t.Parallel()
	const crashes = 50
	l, spawns := scriptedLocal(t, 4, scriptedCrash)
	for i := range crashes {
		s := open(t, l)
		for range 2 {
			_ = s.Send(FakeSpeech(100 * time.Millisecond))
		}
		drain(t, s)
		if err := s.Err(); !errors.Is(err, ErrLost) {
			t.Fatalf("crash %d: a stream whose worker died ended with %v, want ErrLost", i+1, err)
		}
		s.Close()
	}
	again := open(t, l)
	defer again.Close()
	if n := spawns.Load(); n != crashes+1 {
		t.Errorf("%d workers started, want one per crash and one after the last", n)
	}
	if a := l.Availability(); !a.Available {
		t.Errorf("after a crash the recognizer reads %+v, want available", a)
	}
}

// A WORKER THAT CANNOT LOAD IS UNAVAILABLE WITH THE REASON, and the answer is
// kept for RetryAfter rather than a worker started per press.
func TestAWorkerThatCannotLoadIsAnsweredUntilRetryAfter(t *testing.T) {
	t.Parallel()
	l, spawns := scriptedLocal(t, 4, scriptedFailLoad)
	var unavailable *UnavailableError
	_, err := l.Open(t.Context(), Options{Lang: Lang})
	if !errors.As(err, &unavailable) || unavailable.Reason != ReasonFailed {
		t.Fatalf("open = %v, want unavailable, failed", err)
	}
	if a := l.Availability(); a.Available || a.Reason != ReasonFailed {
		t.Errorf("availability = %+v, want unavailable, failed", a)
	}
	if _, err := l.Open(t.Context(), Options{Lang: Lang}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a second open answered %v, want the kept refusal", err)
	}
	if n := spawns.Load(); n != 1 {
		t.Errorf("%d workers started inside RetryAfter, want 1", n)
	}
	l.mu.Lock()
	l.failedAt = l.failedAt.Add(-RetryAfter)
	l.mu.Unlock()
	_, _ = l.Open(t.Context(), Options{Lang: Lang})
	if n := spawns.Load(); n != 2 {
		t.Errorf("%d workers started after RetryAfter, want a second try", n)
	}
}

// A MODEL THAT CANNOT BE HAD IS model_unavailable, and the recognizer says so.
func TestAModelThatCannotBeHadIsModelUnavailable(t *testing.T) {
	t.Parallel()
	l, spawns := scriptedLocal(t, 4, scriptedOK)
	l.locate = func(context.Context, func(float64)) (string, error) {
		return "", errors.New("no route to the release host")
	}
	var unavailable *UnavailableError
	if _, err := l.Open(t.Context(), Options{Lang: Lang}); !errors.As(err, &unavailable) ||
		unavailable.Reason != ReasonModelUnavailable {
		t.Fatalf("open = %v, want unavailable, model_unavailable", err)
	}
	if n := spawns.Load(); n != 0 {
		t.Errorf("a worker started with no model")
	}
}

// PREPARATION IS SHARED AND HEARD: two dictations opened while the model is
// fetched both hear its progress and both ride the one worker it starts.
func TestPreparationIsSharedAndItsProgressHeard(t *testing.T) {
	t.Parallel()
	l, spawns := scriptedLocal(t, 4, scriptedOK)
	release := make(chan struct{})
	reported := make(chan struct{})
	l.locate = func(ctx context.Context, progress func(float64)) (string, error) {
		progress(0.5)
		close(reported)
		<-release
		return t.TempDir(), nil
	}
	// EACH WAITER SIGNALS when it hears the half the fetch reported (0.95 of
	// it: the last part of preparing is the worker's load).
	heard := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var opened sync.WaitGroup
	for i := range 2 {
		var once sync.Once
		opened.Go(func() {
			s, err := l.Open(t.Context(), Options{Lang: Lang, Preparing: func(p float64) {
				if p == 0.475 {
					once.Do(func() { close(heard[i]) })
				}
			}})
			if err != nil {
				t.Errorf("open %d: %v", i, err)
				return
			}
			s.Close()
		})
	}
	<-reported
	for i := range heard {
		select {
		case <-heard[i]:
		case <-time.After(waitTimeout): //fixedwait:liveness
			t.Fatalf("dictation %d never heard the preparation's progress", i)
		}
	}
	close(release)
	opened.Wait()
	if n := spawns.Load(); n != 1 {
		t.Errorf("%d workers started for two dictations prepared together, want 1", n)
	}
}

// AN IDLE WORKER IS UNLOADED: with no live stream for the unload period it
// exits, handing back the model's memory, and the next dictation starts
// another.
func TestAnIdleWorkerIsUnloaded(t *testing.T) {
	t.Parallel()
	l, spawns := scriptedLocal(t, 4, scriptedOK)
	l.unloadAfter = 50 * time.Millisecond
	s := open(t, l)
	l.mu.Lock()
	w := l.worker
	l.mu.Unlock()
	s.Close()
	select {
	case <-w.exited:
	case <-time.After(waitTimeout): //fixedwait:liveness
		t.Fatal("an idle worker never exited")
	}
	open(t, l).Close()
	if n := spawns.Load(); n != 2 {
		t.Errorf("%d workers started, want a new one after the unload", n)
	}
}

// A WORKER THAT NEVER LOADS IS KILLED when the dictation waiting on it gives
// up and the engine stops, rather than left running.
func TestAWorkerThatNeverLoadsIsStopped(t *testing.T) {
	t.Parallel()
	l, _ := scriptedLocal(t, 4, scriptedHang)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := l.Open(ctx, Options{Lang: Lang}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("open = %v, want the dictation's own deadline", err)
	}
	l.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.worker != nil {
		t.Error("a worker survived Close")
	}
}

// AN OPEN AFTER CLOSE IS REFUSED: a stopping engine starts no worker.
func TestOpenAfterCloseIsRefused(t *testing.T) {
	t.Parallel()
	l, spawns := scriptedLocal(t, 4, scriptedOK)
	l.Close()
	if _, err := l.Open(t.Context(), Options{Lang: Lang}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("open after close = %v, want unavailable", err)
	}
	if spawns.Load() != 0 {
		t.Error("a closed recognizer started a worker")
	}
}

// A LANGUAGE THE MODEL DOES NOT SPEAK, OR ODD PCM, IS REFUSED ErrUnsupported.
func TestUnsupportedInputIsRefused(t *testing.T) {
	t.Parallel()
	l, _ := scriptedLocal(t, 4, scriptedOK)
	if _, err := l.Open(t.Context(), Options{Lang: "de"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a German stream opened with %v", err)
	}
	s := open(t, l)
	defer s.Close()
	if err := s.Send([]byte{1, 2, 3}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("three bytes of PCM sent with %v", err)
	}
}
