package host

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/paced"
)

// sink is the response a git process writes into. Nothing reaches the client
// before git's first byte, so a process that fails before writing can still be
// answered with a status (a 503, or a retry on a rebuilt cache). Writes go
// through paced.Writer, which holds the client to the per-mebibyte pace and
// FLUSHES EVERY WRITE: git's keepalives and progress must reach the client as
// written, or a balancer's idle timeout cuts a transfer that is merely working.
//
// A fetch's sink holds upload-pack's negotiation until the pack begins
// (holdPack): pack-objects meets a damaged cache only after it (band.go), and a
// run that has shown the client nothing can run again over the repaired cache.
// A download's sink (download.go) holds its opening for its headers and refuses
// any byte past its bound.
type sink struct {
	w           http.ResponseWriter
	contentType string
	prefix      []byte
	now         func() time.Time
	// onStart, when set, is called once, as the first byte is written.
	onStart func()
	// headers, when set, writes a download's headers from its opening in
	// place of contentType and no-cache.
	headers func(h http.Header, opening []byte)
	// opening is how much a download's hold keeps before the answer starts.
	opening int
	// bounded has the sink refuse a write past limit bytes of body; taken is
	// what git has written so far, held or sent.
	bounded      bool
	limit, taken int64

	mu  sync.Mutex
	out *paced.Writer // nil until the first byte
	// hold, while a fetch's or a download's answer has not started, is what
	// it holds of it (holdPack, holdOpening); nil for any other answer, and
	// once it has started.
	hold *preamble
	// tap reads a fetch's answer for its pack's beginning (band.go): the hold's
	// release rule while held, then, past preambleHoldBytes, whether the answer
	// carried a pack ([sink.PackSent]), which the fetch audit asks (fetch.go).
	// It keeps nothing and delays no write. Nil for any other answer.
	tap *answerScan
	// sent is every byte the paced writer took, prefix and released hold
	// included: the repository's heat's bytes out (Server.served). What a run
	// done again held and dropped never counts.
	sent int64
}

// preamble is what a fetch's sink holds of an upload-pack answer before its
// pack begins.
type preamble struct {
	held []byte
}

// preambleHoldBytes is the most of an upload-pack answer a sink holds before
// its pack begins: the last negotiation round's acknowledgments (a 49-byte ACK
// a common commit), shallow lines, section headers and pack-objects' first
// lines. 64 KiB, one largest packet, holds 1 300 ACKs, which a fetch-pack round
// reaches only when nearly every commit it offers is common; past it the answer
// starts unheld, and damage found later is answered in git's own words. Held
// in a fetch's transfer slot: at most 64 KiB a slot.
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
	s.hold, s.tap = &preamble{}, &answerScan{}
}

// holdOpening has a download's sink hold up to n bytes of its answer before it
// starts, so its headers can read them. Called before the first run.
func (s *sink) holdOpening(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold, s.opening = &preamble{}, n
}

// bound has the sink refuse any write past limit bytes of body (errPastBound),
// writing nothing of it. Called before the first run.
func (s *sink) bound(limit int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bounded, s.limit = true, limit
}

// errPastBound is a git process writing past its download's bound.
var errPastBound = errors.New("host: git wrote past the download's bound")

// again readies a held answer for another run over the same request: what the
// last run's answer held, and its reading, are dropped. Only a run whose
// answer never started is run again, so nothing of it reached the client.
func (s *sink) again() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
		s.taken = 0
		if s.hold != nil {
			s.hold = &preamble{}
		}
		if s.tap != nil {
			s.tap = &answerScan{}
		}
	}
}

// PackSent reports whether a fetch's answer, as far as written, began a pack
// (band.go): false for a negotiation round of acknowledgments alone, and for
// any answer but a fetch's.
func (s *sink) PackSent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tap != nil && s.tap.begun
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
	if s.bounded {
		if s.taken+int64(len(p)) > s.limit {
			return 0, errPastBound
		}
		s.taken += int64(len(p))
	}
	// READ ON PAST THE HOLD until the pack begins, so acknowledgments that
	// outgrew it still tell a fetch from a round that sent no object.
	if t := s.tap; t != nil && !t.begun {
		t.scan(p)
	}
	if s.out == nil {
		if h := s.hold; h != nil {
			if s.tap != nil && !s.tap.begun && len(h.held)+len(p) <= preambleHoldBytes {
				h.held = append(h.held, p...)
				return len(p), nil
			}
			if s.tap == nil && len(h.held)+len(p) < s.opening {
				h.held = append(h.held, p...)
				return len(p), nil
			}
			if s.tap == nil {
				// THE OPENING READ WHOLE: what is held and p start the answer together.
				h.held = append(h.held, p...)
				if err := s.startLocked(); err != nil {
					return 0, err
				}
				return len(p), nil
			}
		}
		if err := s.startLocked(); err != nil {
			return 0, err
		}
	}
	return s.emit(p)
}

// emit writes p to the client and counts what the paced writer took of it;
// s.mu is held and the response has started.
func (s *sink) emit(p []byte) (int, error) {
	n, err := s.out.Write(p)
	s.sent += int64(n)
	return n, err
}

// release starts a held answer with what it holds, once the last run has
// ended: its answer stands, success or git's own words of failure. A fetch
// holding nothing starts nothing, so it can still be answered with a status; a
// download starts even empty.
func (s *sink) release() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out != nil || s.hold == nil || (s.tap != nil && len(s.hold.held) == 0) {
		return nil
	}
	return s.startLocked()
}

// startLocked writes the status, the headers, the service prefix and whatever
// the sink held; s.mu is held.
func (s *sink) startLocked() error {
	h := s.w.Header()
	if s.headers != nil {
		var opening []byte
		if s.hold != nil {
			opening = s.hold.held
		}
		s.headers(h, opening)
	} else {
		h.Set("Content-Type", s.contentType)
		h.Set("Cache-Control", "no-cache")
	}
	s.w.WriteHeader(http.StatusOK)
	s.out = paced.NewWriter(s.w, s.now)
	if s.onStart != nil {
		s.onStart()
	}
	if len(s.prefix) > 0 {
		if _, err := s.emit(s.prefix); err != nil {
			return err
		}
	}
	if s.hold != nil {
		held := s.hold.held
		s.hold = nil
		if len(held) > 0 {
			if _, err := s.emit(held); err != nil {
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

// Sent is how many bytes of the answer the client has been written.
func (s *sink) Sent() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

// End ends a started response under what is left of its pace, so the
// terminator net/http writes after the handler returns is charged to the
// client's mebibyte, not the handler's work (paced.Writer.End).
func (s *sink) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out != nil {
		s.out.End()
	}
}
