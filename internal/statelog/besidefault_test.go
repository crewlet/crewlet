package statelog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// lineLog is a JSON log a running loop writes and a case reads, under one lock.
type lineLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// count is how many lines carry msg.
func (l *lineLog) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range bytes.Split(l.buf.Bytes(), []byte("\n")) {
		var record struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(line, &record) == nil && record.Msg == msg {
			n++
		}
	}
	return n
}

// A COMMIT BESIDE THE LOOP IS NO RETRY OF A FAILED FETCH.
//
// A barrier is handed to the checkpoint transaction beside the loop and held at
// its commit; the broker then fails two fetches in a row, and the commit returns
// in the pause after the first. A run of failures is written as its start and
// its end: one `statelog_apply_retrying` for the two fetches and one
// `statelog_apply_recovered` once a fetch is answered. Before, the commit
// between them was taken for the retry — a recovery written naming the fetch's
// error, the fault's clock and its back-off reset — and the second failed fetch
// began a run of its own.
func TestACommitBesideTheLoopIsNoRetryOfAFailedFetch(t *testing.T) {
	t.Parallel()
	lines := &lineLog{}
	hold := storetest.HoldCommits(t)
	h := newApplyHarnessOn(t, probeDomain{}, store.Options{WrapDriver: hold.Wrap},
		func(d *statelog.RunnerDeps) { d.Logger = slog.New(slog.NewJSONHandler(lines, nil)) })
	h.fetch.offer(1, env(1, "edit", "o1", "op-1", 1))
	loop := h.looping(t)
	until(t, "the write before the hold", func() bool { return h.runner.Committed().Seq >= 1 })

	hold.Arm()
	defer hold.Release()
	h.fetch.offer(2, barrierEnv())
	if err := hold.AwaitHeld(t.Context(), 1); err != nil {
		t.Fatalf("the barrier's checkpoint never reached its commit: %v", err)
	}
	h.fetch.fail(2)
	until(t, "the first of two failed fetches", func() bool { return h.fetch.failing() == 1 })
	// THE COMMIT RETURNS in the pause the first failure began.
	hold.Release()
	until(t, "a fetch answered after the two that failed", func() bool {
		return h.fetch.failing() == 0 && lines.count("statelog_apply_recovered") > 0
	})
	if err := loop.stop(); err != nil {
		t.Fatalf("the applier stopped with %v", err)
	}
	if got := h.runner.Committed().Seq; got != 2 {
		t.Fatalf("the checkpoint is at %d, want the barrier's 2 — the premise is a "+
			"commit beside the loop between the two failed fetches", got)
	}
	if got := lines.count("statelog_apply_retrying"); got != 1 {
		t.Errorf("%d statelog_apply_retrying lines for two failed fetches in a row, "+
			"want one — the commit beside the loop between them ended the run", got)
	}
	if got := lines.count("statelog_apply_recovered"); got != 1 {
		t.Errorf("%d statelog_apply_recovered lines, want the one an answered fetch "+
			"wrote", got)
	}
}
