package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ALARMS OF A NODE WITHOUT `data`: it runs no state log, so no retention
// heartbeat evaluates a table for it, yet it measures conditions no other node
// can see (its objective meter's burns, its store's volume, write-ahead log
// and pool). It evaluates those kinds ([statelog.LocalKinds]) on a loop of its
// own, through the same tracker, wakes and advisor episodes a data node's
// table feeds, so its alarms reach every surface a data node's do: the gauge,
// the log line, its presence marks, `/fleet`, the notices. The package doc's
// "The alarms of a node without data" says why it judges them itself rather
// than a data node raising them from its status record.

// nodeMeasures is what a node measures of itself alone: its store's files and
// the volumes holding them, its recorder's windowed pool waits and its
// objective meter's burns. ONE IMPLEMENTATION for a data node's reading
// ([retention.measures]) and a node without `data`'s ([localAlarms]), so the
// two never judge one condition two ways.
type nodeMeasures struct {
	db         *store.DB
	metrics    *metrics.Recorder
	objectives *objectives.Meter

	// volumes identifies the volume a directory is on and measures what it
	// has free ([measureVolume] where nil); a parameter for the tests.
	volumes func(dir string) (volume string, free int64, err error)
}

// window is the recorder's rolling window, nil where the node has none.
func (m nodeMeasures) window() []metrics.Snapshot {
	if m.metrics == nil {
		return nil
	}
	return m.metrics.ReadWindow()
}

// read fills the reading's fields the node measures of itself, at now, the
// pool's waits from window ([nodeMeasures.window]).
func (m nodeMeasures) read(out *statelog.Reading, now time.Time, window []metrics.Snapshot) {
	m.space(out, now)
	for _, snapshot := range window {
		if snapshot.Name == metrics.StorePoolWait {
			// THE WORST FILE, not the sum of them. A caller queues on
			// ONE pool, and two estates each half-starved is not the
			// same node as one estate fully starved.
			out.PoolWaitP95 = max(out.PoolWaitP95, p95Duration(snapshot))
		}
	}
	// THE OBJECTIVES' OWN JUDGEMENT, read whole: the meter owns the targets,
	// the window pairs and their factors, and the alarm restates none of
	// them (ADR-0015). A nil meter judges nothing burning.
	out.ObjectiveBurns = m.objectives.Burning()
}

// localAlarms is a node without `data`'s alarm table: its tracker, the wakes
// and the episodes it hands each transition to, and the loop evaluating it.
type localAlarms struct {
	measures nodeMeasures
	tracker  *statelog.Tracker
	wakes    *alarmOutbox
	episodes *alarmEpisodes
	now      func() time.Time

	// beating holds a pass whole, so a pass the loop runs and one a case
	// drives never interleave.
	beating sync.Mutex

	stop context.CancelFunc
	done sync.WaitGroup
}

// startLocalAlarms starts the alarm loop of a node without `data`, kept until
// [Engine.stopLocalAlarms]. A data node evaluates the whole table on its
// retention heartbeat ([Engine.startRetention]) and starts none.
func (e *Engine) startLocalAlarms(ctx context.Context) {
	if HoldsEstate(e.boot) || e.backends == nil {
		return
	}
	measures := nodeMeasures{db: e.backends.Store, metrics: e.metrics, objectives: e.objectives}
	e.newLocalAlarms(measures, e.AdvisorWrites).start(ctx, e)
}

// newLocalAlarms is the table over measures, its episodes kept through
// writes, its loop not yet started.
func (e *Engine) newLocalAlarms(measures nodeMeasures,
	writes func() (estate.AdvisorWriter, bool)) *localAlarms {
	now := func() time.Time { return e.now().UTC() }
	l := &localAlarms{measures: measures, now: now, tracker: statelog.NewTracker(e.metrics, now)}
	// AS THE RETENTION HEARTBEAT HANDS THEM ON ([Engine.startRetention]): the
	// wakes told of a raise where a broker takes it, the episodes otherwise.
	// No Remember: this node's store is scratch, deleted at every boot.
	var handOn []func(statelog.AlarmTransition)
	if e.backends != nil && e.backends.Queue != nil {
		l.wakes = newAlarmOutbox(e.backends.Queue, e.metrics)
		l.wakes.taken = l.tracker.Told
		handOn = append(handOn, l.wakes.Add)
	}
	l.episodes = newAlarmEpisodes(writes)
	if l.wakes == nil {
		l.episodes.taken = l.tracker.Told
	}
	handOn = append(handOn, l.episodes.Add)
	l.tracker.OnTransition(e.nodeID(), func(t statelog.AlarmTransition) {
		for _, add := range handOn {
			add(t)
		}
	})
	return l
}

// start publishes the tracker for the health envelope and the presence beat,
// then runs the loop, the wakes and the episodes until [Engine.stopLocalAlarms].
// DETACHED from the caller's context, as the retention loops are.
func (l *localAlarms) start(ctx context.Context, e *Engine) {
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	l.stop = stop
	e.alarms.Store(l.tracker)
	e.localAlarms.Store(l)
	l.done.Go(func() { l.run(loop) })
	if l.wakes != nil {
		l.done.Go(func() { l.wakes.run(loop) })
	}
	l.done.Go(func() { l.episodes.run(loop) })
}

// stopLocalAlarms ends the loop, its wakes and its episodes, waiting for a
// pass in flight. What the tracker last found stands for the health envelope.
func (e *Engine) stopLocalAlarms() {
	l := e.localAlarms.Swap(nil)
	if l == nil {
		return
	}
	l.stop()
	l.done.Wait()
}

// run evaluates the table at once and then every [statelog.AlarmInterval], the
// cadence a data node's heartbeat evaluates its own at, until ctx ends. A pass
// costs about 0.45 ms and 129 KB in 2,950 allocations on a 2-core EPYC-Rome
// (BenchmarkALocalAlarmPass): under a ten-thousandth of a core at that cadence.
func (l *localAlarms) run(ctx context.Context) {
	ticker := time.NewTicker(statelog.AlarmInterval)
	defer ticker.Stop()
	for {
		l.beat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// beat is one evaluation: the node's own measures, judged by the local rules
// alone ([statelog.EvaluateLocal]), observed by the tracker.
func (l *localAlarms) beat(ctx context.Context) {
	l.beating.Lock()
	defer l.beating.Unlock()
	var reading statelog.Reading
	// WHAT IS UP NOW, so an alarm that clears past its raise is judged
	// against the wider bound while it stands ([statelog.Reading.Up]).
	reading.Up, _ = l.tracker.Standing()
	l.measures.read(&reading, l.now(), l.measures.window())
	l.tracker.Observe(ctx, statelog.EvaluateLocal(reading))
}
