package harness

import (
	"errors"
	"sync"
	"time"
)

// stretch is the clock a CLI's run is bounded by: timeout_seconds for ONE
// STRETCH of the model's own work — from the start, or from the moment the last
// of the engine's tools in flight returned, to the next call or the end — and
// stopped while any of the engine's tools runs.
//
// NOT THE WHOLE RUN, because a run is no longer one answer. A seat's CLI drives
// its own loop and calls the seat's tools in it, so one run is every model turn
// of a phase: an executor thinking thirty seconds a turn over twenty-four turns
// works twelve minutes, and a wall-clock cap derived for one answer (adaptive
// thinking against the output cap, plus the launch) would kill a healthy phase
// and hand it to the next member of the chain with nothing gained. Stopped
// while a tool runs, because the tool's time is the tool's — each is bounded
// by its own deadline and by the CLI's tool timeout, which the engine answers
// ahead of — and a model blocked on one is not working.
//
// A text function is one answer, and its clock is never stopped: one stretch
// is the whole call, which is what timeout_seconds always bounded.
type stretch struct {
	limit time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	busy    bool
	stopped bool
	// gen counts the stretches armed and ended, so a timer that fires after
	// its stretch was ended is told from the stretch armed after it.
	gen int

	// fired is closed when a stretch ran its limit out.
	fired chan struct{}
	once  sync.Once
}

// errStretch is the cause a run's context is cancelled with when its clock ran
// out, which [run] reports as a timeout.
var errStretch = errors.New("the model worked one stretch past timeout_seconds")

func newStretch(limit time.Duration) *stretch {
	return &stretch{limit: limit, fired: make(chan struct{})}
}

// start arms the clock for the first stretch, when the CLI starts. A call
// already in flight — the clock told busy before the process was up — leaves
// it stopped.
func (s *stretch) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.busy && !s.stopped {
		s.armLocked()
	}
}

// pause stops the clock: one of the engine's tools started running.
func (s *stretch) pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = true
	s.disarmLocked()
}

// resume starts a fresh stretch: the last tool in flight returned, and the
// model is working again — with the whole limit, since what it does next is a
// new answer.
func (s *stretch) resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	if !s.stopped {
		s.armLocked()
	}
}

// stop retires the clock for good: the run is over.
func (s *stretch) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	s.disarmLocked()
}

func (s *stretch) disarmLocked() {
	s.gen++
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

func (s *stretch) armLocked() {
	s.disarmLocked()
	gen := s.gen
	s.timer = time.AfterFunc(s.limit, func() {
		s.mu.Lock()
		// A stretch that was ended — paused, restarted or retired — while
		// this func was already on its way is not the one that ran out.
		live := gen == s.gen
		s.mu.Unlock()
		if live {
			s.once.Do(func() { close(s.fired) })
		}
	})
}

// expired is closed once a stretch ran its limit out.
func (s *stretch) expired() <-chan struct{} { return s.fired }
