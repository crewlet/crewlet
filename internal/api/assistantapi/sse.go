package assistantapi

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// sseWriter is one live stream as the orchestrator writes to it: the headers
// once, then each frame in the protocol's one encoding (wire.Encode), flushed
// as it is written so a frame reaches the browser when it happens rather than
// when a buffer fills.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
	// started is set once the stream's headers went out: past it a failure
	// can only end the stream, never be answered as a refusal.
	started bool
}

var _ assistant.FrameWriter = (*sseWriter)(nil)

// begin writes the stream's headers, once.
//
// NO CACHE AND NO PROXY BUFFERING, because a buffered stream is a stream that
// arrives all at once at its end: `X-Accel-Buffering: no` is the header the
// common reverse proxies read to pass an event stream through as it is
// written.
func (s *sseWriter) begin() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
}

// Frame writes one frame and flushes it.
func (s *sseWriter) Frame(f wire.Frame) error {
	s.begin()
	if err := wire.Encode(s.w, f); err != nil {
		return err
	}
	return s.rc.Flush()
}

// Keepalive writes the protocol's comment line, so a proxy never closes a
// quiet stream.
func (s *sseWriter) Keepalive() error {
	s.begin()
	if _, err := s.w.Write([]byte(wire.KeepaliveComment)); err != nil {
		return err
	}
	return s.rc.Flush()
}
