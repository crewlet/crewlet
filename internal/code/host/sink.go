package host

import (
	"net/http"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/paced"
)

// sink is the response a git process writes into: nothing reaches the client
// until the first byte git writes — then the status 200, the headers, the
// service prefix (for an advertisement) and the byte — so a process that fails
// before writing can still be answered with a status: a 503, or a retry on a
// rebuilt cache. Writes go through paced.Writer, which holds the client to the
// per-mebibyte pace and FLUSHES EVERY WRITE: a git process's keepalives and
// progress lines must reach the client when git writes them, or a balancer's
// idle timeout cuts a transfer that is merely working.
//
// A FETCH'S SINK HOLDS MORE (holdPack): the answer's first byte is upload-pack's
// negotiation, written before its pack-objects runs, and pack-objects meets a
// damaged cache only after it (band.go). So until the pack begins, what
// upload-pack writes is held and read for the pack's beginning rather than
// written, and the response starts only with the pack — or with git's own
// failure, when the run's answer stands (release) — so a run that met damage
// before its pack has shown the client nothing and is run again over the
// repaired cache.
type sink struct {
	w           http.ResponseWriter
	contentType string
	prefix      []byte
	now         func() time.Time
	// onStart, when set, is called once, as the first byte is written.
	onStart func()

	mu  sync.Mutex
	out *paced.Writer // nil until the first byte
	// hold, while a fetch's answer has not started, is what it holds of
	// it (holdPack); nil for any other answer, and once it has started.
	hold *preamble
}

// preamble is what a fetch's sink holds of an upload-pack answer before its
// pack begins, and its reading of it.
type preamble struct {
	scan answerScan
	held []byte
}

// preambleHoldBytes is the most of an upload-pack answer a sink holds before
// its pack begins: everything upload-pack writes ahead of pack-objects' first
// word — the acknowledgments of the request's last negotiation round, an ACK
// line of 49 bytes for each common commit it named, its shallow lines and
// section headers — and pack-objects' first lines. 64 KiB, one largest
// packet's worth, holds 1 300 acknowledgments, which a round of git's
// fetch-pack reaches only when nearly every commit it offers is common; past
// it the answer starts as every answer did before the hold, and damage named
// after that is answered in git's own words. A fetch holds it in its transfer
// slot, so a node holds at most 64 KiB a transfer slot for every fetch at once.
const preambleHoldBytes = 64 << 10

// newSink is a sink answering w with contentType; an advertisement sets its
// service prefix before the first write.
func newSink(w http.ResponseWriter, contentType string, now func() time.Time) *sink {
	return &sink{w: w, contentType: contentType, now: now}
}

// holdPack has the sink hold its answer until the pack begins: a fetch's,
// whose upload-pack may run pack-objects. Called before the first run.
func (s *sink) holdPack() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = &preamble{}
}

// again readies a held answer for another run over the same request: what the
// last run's answer held, and its reading, are dropped. Only a run whose
// answer never started is run again, so nothing of it reached the client.
func (s *sink) again() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil && s.hold != nil {
		s.hold = &preamble{}
	}
}

// Write starts the response at its first byte and writes p — for a held
// answer, at its pack's first byte (or at what it holds outgrowing
// preambleHoldBytes), with everything held before it.
func (s *sink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
		if h := s.hold; h != nil {
			h.scan.scan(p)
			if !h.scan.begun && len(h.held)+len(p) <= preambleHoldBytes {
				h.held = append(h.held, p...)
				return len(p), nil
			}
		}
		if err := s.startLocked(); err != nil {
			return 0, err
		}
	}
	return s.out.Write(p)
}

// release starts a held answer with what it holds: the run's answer stands —
// it succeeded, or failed in git's own words, which the client must read as
// git wrote them. Nothing held, nothing is started, so a run that wrote
// nothing is still answered with a status. Called once the last run has
// ended.
func (s *sink) release() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out != nil || s.hold == nil || len(s.hold.held) == 0 {
		return nil
	}
	return s.startLocked()
}

// startLocked writes the status and the headers, then the service prefix and
// whatever the sink held; s.mu is held. Nothing is read of the answer past
// it.
func (s *sink) startLocked() error {
	h := s.w.Header()
	h.Set("Content-Type", s.contentType)
	h.Set("Cache-Control", "no-cache")
	s.w.WriteHeader(http.StatusOK)
	s.out = paced.NewWriter(s.w, s.now)
	if s.onStart != nil {
		s.onStart()
	}
	if len(s.prefix) > 0 {
		if _, err := s.out.Write(s.prefix); err != nil {
			return err
		}
	}
	if s.hold != nil {
		held := s.hold.held
		s.hold = nil
		if len(held) > 0 {
			if _, err := s.out.Write(held); err != nil {
				return err
			}
		}
	}
	return nil
}

// Started reports whether any byte has been written: past it a failure can no
// longer be answered with a status, only by cutting the connection.
func (s *sink) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out != nil
}

// End ends a started response under what is left of its pace, so the
// terminator net/http writes after the handler returns is charged to the
// client's mebibyte rather than to the handler's own work (paced.Writer.End).
// A sink that never started has nothing to end.
func (s *sink) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out != nil {
		s.out.End()
	}
}
