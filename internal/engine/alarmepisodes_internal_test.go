package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
)

// episodeLog is an advisor log's writes that keep the alarm episodes written,
// refusing the first refusals writes.
type episodeLog struct {
	estate.AdvisorWriter
	mu       sync.Mutex
	refusals int
	raised   []advisordomain.AlarmRaise
	cleared  []string
	ops      []string
	done     chan struct{}
}

func (l *episodeLog) RaiseAlarm(_ context.Context, c advisordomain.Call, a advisordomain.AlarmRaise) (statelog.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, c.OpID)
	if l.refusals > 0 {
		l.refusals--
		return statelog.Result{}, errors.New("no data node answered")
	}
	l.raised = append(l.raised, a)
	return statelog.Result{}, nil
}

func (l *episodeLog) ClearAlarm(_ context.Context, c advisordomain.Call, node, kind, instance string,
	a advisordomain.AlarmClear) (statelog.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, c.OpID)
	l.cleared = append(l.cleared, node+"/"+kind+"/"+instance+"@"+a.ClearedAt.Format(time.RFC3339))
	close(l.done)
	return statelog.Result{}, nil
}

// EVERY ALARM TRANSITION IS KEPT AS AN EPISODE where the node makes it: a fire
// raises its instance's series at the instant it was raised, a clear clears it
// at the instant it cleared, a refusal is retried under the same operation,
// and on a node with no broker the fire is told back so the tracker keeps it
// across a restart. Mutation: write a fire under a fresh operation on each
// attempt, or never tell it back, and a check fails.
func TestEveryAlarmTransitionIsKeptAsAnEpisode(t *testing.T) {
	t.Parallel()
	since := time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)
	cleared := since.Add(7 * time.Minute)
	log := &episodeLog{refusals: 1, done: make(chan struct{})}
	a := newAlarmEpisodes(func() (estate.AdvisorWriter, bool) { return log, true })
	a.wait = 0
	var told []string
	a.taken = func(_ context.Context, id string) { told = append(told, id) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.run(ctx)
	fire := statelog.AlarmTransition{ID: "tr-1", Node: "data-1", Kind: statelog.KindPoolStarved,
		Change: statelog.TransitionFired, Since: since, At: since}
	clear := fire
	clear.ID, clear.Change, clear.At = "tr-2", statelog.TransitionCleared, cleared
	a.Add(fire)
	a.Add(clear)
	// A BOUND ON THE WAIT, never its verdict: the loop writes both at once.
	select {
	case <-log.done:
	case <-time.After(time.Minute):
		t.Fatal("the clear was never written")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.raised) != 1 || log.raised[0].Node != "data-1" || log.raised[0].Kind != "pool_starved" ||
		log.raised[0].Instance != "pool_starved" || !log.raised[0].RaisedAt.Equal(since) {
		t.Errorf("raised %+v, want pool_starved on data-1 since %s", log.raised, since)
	}
	if want := "data-1/pool_starved/pool_starved@" + cleared.Format(time.RFC3339); len(log.cleared) != 1 || log.cleared[0] != want {
		t.Errorf("cleared %v, want %s", log.cleared, want)
	}
	if len(log.ops) != 3 || log.ops[0] != log.ops[1] || log.ops[1] == log.ops[2] {
		t.Errorf("operations %v, want the fire's retry under its first operation", log.ops)
	}
	if len(told) != 1 || told[0] != "tr-1" {
		t.Errorf("told %v, want the fire alone", told)
	}
}
