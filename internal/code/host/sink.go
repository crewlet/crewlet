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
type sink struct {
	w           http.ResponseWriter
	contentType string
	prefix      []byte
	now         func() time.Time
	// onStart, when set, is called once, as the first byte is written.
	onStart func()

	mu  sync.Mutex
	out *paced.Writer // nil until the first byte
}

func newSink(w http.ResponseWriter, contentType string, prefix []byte, now func() time.Time) *sink {
	return &sink{w: w, contentType: contentType, prefix: prefix, now: now}
}

// Write starts the response at its first byte and writes p.
func (s *sink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
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
				return 0, err
			}
		}
	}
	return s.out.Write(p)
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
