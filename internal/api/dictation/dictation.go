// Package dictation serves /ws/dictation: a signed-in person's microphone
// audio in, the recognizer's provisional partials and punctuated finals out.
//
// # The socket is /ws/stream's, admitted and kept the same way
//
// [stream.Admit] decides the handshake by the rules every engine socket shares
// (the principal from the session or bearer, a stage that may act, an Origin
// the CSRF rule accepts), and [stream.Service.KeepDecided] decides the
// credential again for as long as the socket is open, closing it 4401 or 4403.
// The relay acts only as that person: it writes nothing on their behalf. The
// words land in their composer, and they send them through the existing write.
//
// # The protocol is the recognizer's, not a vendor's
//
// The client sends `{"type":"start","lang":"en","rate":16000,"encoding":"pcm16"}`,
// then binary frames of 16-bit mono PCM (20 to 100 ms each), then
// `{"type":"finish"}` or `{"type":"cancel"}`. The engine answers `preparing
// {progress}` while the recognizer is readied, `ready`, `partial {seq, text}`
// (which replaces the provisional segment `seq`), `final {seq, text}` (which
// appends it), and ends with `done {reason}`, `busy {reason}` or `error
// {code}`. Audio sent before `ready` is held, up to [holdLimit].
//
// # Limits
//
// One live dictation per person per node ([Limits]); a per-message read limit
// sized to the largest frame; a session length and a matching audio cap; an
// idle cut when no audio arrives. Past the recognizer's capacity the answer is
// `busy`, and a screen falls back to the browser's own recognition.
//
// # Nothing is stored or logged
//
// Audio and text live in this session's memory alone. The one log line a
// session writes carries the person's id, its duration, the audio's size, how
// it ended and an error code; never a word.
package dictation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/speech"
)

var log = logging.Get("api.dictation")

// Path is the dictation socket.
const Path = "/ws/dictation"

// readLimit bounds one client message: 100 ms of audio is 3,200 bytes, so
// 8 KiB takes the largest frame a client sends with room to spare.
const readLimit = 8 << 10

// holdLimit is how much audio sent before `ready` is held for the recognizer:
// ten seconds keeps the first words of a dictation started while a warm
// recognizer loads (about two seconds), and ends one that would wait out a
// first download.
const holdLimit = 10 * speech.BytesPerSecond

// startTimeout is how long a socket may stay open without its `start`.
const startTimeout = 10 * time.Second

// finishTimeout bounds the recognizer's last finals after `finish`: decoding
// what it holds takes well under a second.
const finishTimeout = 10 * time.Second

// writeTimeout bounds one message to a client that stopped reading.
const writeTimeout = 10 * time.Second

// Limits are a node's `dictation` session limits.
type Limits struct {
	// Session is the longest dictation, by the clock and by its audio.
	Session time.Duration

	// Idle ends a dictation that sent no audio for this long.
	Idle time.Duration
}

// Handler serves [Path].
type Handler struct {
	guard   *auth.Guard
	origins stream.CrossSite
	sockets *stream.Service
	speech  speech.Streamer
	limits  Limits

	mu   sync.Mutex
	held map[string]bool
}

// New builds the dictation socket over the recognizer.
func New(guard *auth.Guard, origins stream.CrossSite, sockets *stream.Service,
	recognizer speech.Streamer, limits Limits) *Handler {
	return &Handler{guard: guard, origins: origins, sockets: sockets, speech: recognizer,
		limits: limits, held: map[string]bool{}}
}

// ServeHTTP admits the handshake, refuses it 503 while dictation is
// unavailable here, and runs one dictation.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	admitted, ok := stream.Admit(h.guard, h.origins, w, r) //nolint:contextcheck // decided on the request's own context
	if !ok {
		return
	}
	if a := h.speech.Availability(); !a.Available {
		httpjson.FailWithFields(w, http.StatusServiceUnavailable, httpjson.CodeDictationUnavailable,
			httpjson.Detail{"reason": string(a.Reason)})
		return
	}
	conn, err := admitted.Accept(w)
	if err != nil {
		log.Debug("dictation_accept_failed", "error", err)
		return
	}
	conn.SetReadLimit(readLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var deciding sync.WaitGroup
	deciding.Go(func() { h.sockets.KeepDecided(ctx, conn, admitted) })
	defer func() {
		cancel()
		deciding.Wait()
	}()

	principal := admitted.Principal()
	who := personOf(principal)
	if !h.hold(who) {
		log.Info("dictation_refused", "person", who, "reason", "session")
		s := &session{conn: conn}
		s.send(ctx, out{Type: "busy", Reason: "session"})
		_ = conn.Close(websocket.StatusTryAgainLater, "dictation is open elsewhere")
		return
	}
	// RELEASED BEFORE THE CLOSE FRAME, so the tab that reads it may dictate
	// again at once.
	release := sync.OnceFunc(func() { h.release(who) })
	defer release()
	s := &session{conn: conn, limits: h.limits, speech: h.speech, began: time.Now(), release: release}
	s.run(ctx)
	log.Info("dictation_session_ended", "person", who,
		"duration_ms", time.Since(s.began).Milliseconds(),
		"audio_ms", int64(s.bytes)*1000/speech.BytesPerSecond, "bytes", s.bytes,
		"reason", s.reason, "code", s.code)
}

// personOf names the principal in a log line: their person id, or a token's
// login when the credential is bound to nobody.
func personOf(p iam.Principal) string {
	if p.ID != uuid.Nil {
		return p.ID.String()
	}
	return p.Login
}

// hold claims the person's one dictation on this node.
func (h *Handler) hold(who string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held[who] {
		return false
	}
	h.held[who] = true
	return true
}

func (h *Handler) release(who string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.held, who)
}

// in is a client's text message.
type in struct {
	Type     string `json:"type"`
	Lang     string `json:"lang"`
	Rate     int    `json:"rate"`
	Encoding string `json:"encoding"`
}

// out is an engine message; only the fields its type uses are set.
type out struct {
	Type     string   `json:"type"`
	Progress *float64 `json:"progress,omitempty"`
	Seq      *int     `json:"seq,omitempty"`
	Text     string   `json:"text,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Code     string   `json:"code,omitempty"`
}

// How a dictation ends, as `done`'s reason.
const (
	doneFinish    = "finish"
	doneCancel    = "cancel"
	doneLength    = "length"
	doneIdle      = "idle"
	donePreparing = "preparing"
)

// message is one read off the socket, or why reading stopped.
type message struct {
	binary bool
	data   []byte
	err    error
}

// session is one dictation.
type session struct {
	conn   *websocket.Conn
	limits Limits
	speech speech.Streamer
	began  time.Time

	// release gives back the person's one dictation on this node.
	release func()

	// What the log line reports.
	bytes  int
	reason string
	code   string
}

func (s *session) send(ctx context.Context, m out) {
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	_ = s.conn.Write(ctx, websocket.MessageText, raw)
}

// end sends the closing message and closes the socket.
func (s *session) end(ctx context.Context, m out, status websocket.StatusCode) {
	s.send(ctx, m)
	switch m.Type {
	case "done":
		s.reason = m.Reason
	case "busy":
		s.reason = "busy_" + m.Reason
	case "error":
		s.reason, s.code = "error", m.Code
	}
	if s.release != nil {
		s.release()
	}
	_ = s.conn.Close(status, m.Type)
}

func (s *session) fail(ctx context.Context, code httpjson.Code) {
	status := websocket.StatusInternalError
	if code == httpjson.CodeBadParams {
		status = websocket.StatusPolicyViolation
	}
	s.end(ctx, out{Type: "error", Code: string(code)}, status)
}

// run reads the start, opens the recognizer while holding early audio, and
// relays until the dictation ends.
func (s *session) run(ctx context.Context) {
	frames := make(chan message)
	go func() {
		for {
			kind, data, err := s.conn.Read(ctx)
			select {
			case frames <- message{binary: kind == websocket.MessageBinary, data: data, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	s.reason = "closed"

	start, ok := s.readStart(ctx, frames)
	if !ok {
		return
	}
	stream, held, ok := s.open(ctx, frames, start)
	if !ok {
		return
	}
	defer stream.Close()
	s.send(ctx, out{Type: "ready"})
	for _, pcm := range held {
		if err := stream.Send(pcm); err != nil {
			s.fail(ctx, httpjson.CodeDictationUnavailable)
			return
		}
	}
	s.relay(ctx, frames, stream)
}

// readStart waits for a valid `start`.
func (s *session) readStart(ctx context.Context, frames <-chan message) (in, bool) {
	timer := time.NewTimer(startTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return in{}, false
	case <-timer.C:
		s.fail(ctx, httpjson.CodeBadParams)
		return in{}, false
	case m := <-frames:
		if m.err != nil {
			return in{}, false
		}
		var start in
		if m.binary || json.Unmarshal(m.data, &start) != nil || start.Type != "start" ||
			start.Rate != speech.Rate || start.Encoding != "pcm16" || start.Lang != speech.Lang {
			s.fail(ctx, httpjson.CodeBadParams)
			return in{}, false
		}
		return start, true
	}
}

type opened struct {
	stream speech.Stream
	err    error
}

// open opens the recognizer, relaying its preparation and holding the audio
// that arrives meanwhile.
func (s *session) open(ctx context.Context, frames <-chan message, start in) (speech.Stream, [][]byte, bool) {
	openCtx, cancelOpen := context.WithCancel(ctx)
	defer cancelOpen()
	done := make(chan opened, 1)
	go func() {
		st, err := s.speech.Open(openCtx, speech.Options{Lang: start.Lang, Preparing: func(p float64) {
			s.send(ctx, out{Type: "preparing", Progress: &p})
		}})
		done <- opened{st, err}
	}()
	// Whatever Open answers after this session gave up is closed here.
	abandon := func() {
		cancelOpen()
		go func() {
			if r := <-done; r.stream != nil {
				r.stream.Close()
			}
		}()
	}
	timer := time.NewTimer(s.limits.Session)
	defer timer.Stop()
	var held [][]byte
	heldBytes := 0
	for {
		select {
		case r := <-done:
			if r.err != nil {
				s.refused(ctx, r.err)
				return nil, nil, false
			}
			return r.stream, held, true
		case <-ctx.Done():
			abandon()
			return nil, nil, false
		case <-timer.C:
			abandon()
			s.end(ctx, out{Type: "done", Reason: donePreparing}, websocket.StatusNormalClosure)
			return nil, nil, false
		case m := <-frames:
			switch {
			case m.err != nil:
				abandon()
				return nil, nil, false
			case m.binary:
				if heldBytes+len(m.data) > holdLimit {
					abandon()
					s.end(ctx, out{Type: "done", Reason: donePreparing}, websocket.StatusNormalClosure)
					return nil, nil, false
				}
				heldBytes += len(m.data)
				s.bytes += len(m.data)
				held = append(held, m.data)
			default:
				abandon()
				s.control(ctx, m.data, doneCancel)
				return nil, nil, false
			}
		}
	}
}

// refused answers an Open the recognizer refused.
func (s *session) refused(ctx context.Context, err error) {
	switch {
	case errors.Is(err, speech.ErrBusy):
		log.Info("dictation_refused", "reason", "capacity")
		s.end(ctx, out{Type: "busy", Reason: "capacity"}, websocket.StatusTryAgainLater)
	case errors.Is(err, speech.ErrUnsupported):
		s.fail(ctx, httpjson.CodeBadParams)
	case errors.Is(err, context.Canceled):
	default:
		log.Warn("dictation_unavailable", "error", err.Error())
		s.fail(ctx, httpjson.CodeDictationUnavailable)
	}
}

// control ends a dictation on a client's `finish` or `cancel` sent before
// there was anything to finish; anything else is refused.
func (s *session) control(ctx context.Context, data []byte, reason string) {
	var m in
	if json.Unmarshal(data, &m) != nil || (m.Type != "finish" && m.Type != "cancel") {
		s.fail(ctx, httpjson.CodeBadParams)
		return
	}
	if m.Type == "finish" {
		reason = doneFinish
	}
	s.end(ctx, out{Type: "done", Reason: reason}, websocket.StatusNormalClosure)
}

// relay feeds audio to the recognizer and its results to the client until
// the dictation ends.
func (s *session) relay(ctx context.Context, frames <-chan message, st speech.Stream) {
	length := time.NewTimer(s.limits.Session)
	defer length.Stop()
	idle := time.NewTimer(s.limits.Idle)
	defer idle.Stop()
	var finishing <-chan time.Time
	reason := ""
	finish := func(why string) {
		if reason != "" {
			return
		}
		reason = why
		_ = st.Finish()
		finishing = time.After(finishTimeout)
		idle.Stop()
		length.Stop()
	}
	maxBytes := int(s.limits.Session.Seconds() * speech.BytesPerSecond)
	seq := 0
	results := st.Results()
	for {
		select {
		case <-ctx.Done():
			return
		case <-length.C:
			finish(doneLength)
		case <-idle.C:
			finish(doneIdle)
		case <-finishing:
			log.Warn("dictation_finish_stalled", "waited", finishTimeout.String())
			s.fail(ctx, httpjson.CodeDictationUnavailable)
			return
		case r, open := <-results:
			if !open {
				if err := st.Err(); err != nil {
					if errors.Is(err, speech.ErrTooLong) {
						s.end(ctx, out{Type: "done", Reason: doneLength}, websocket.StatusNormalClosure)
						return
					}
					log.Warn("dictation_stream_lost", "error", err.Error())
					s.fail(ctx, httpjson.CodeDictationUnavailable)
					return
				}
				if reason == "" {
					reason = doneFinish
				}
				s.end(ctx, out{Type: "done", Reason: reason}, websocket.StatusNormalClosure)
				return
			}
			n := seq
			m := out{Type: "partial", Seq: &n, Text: r.Text}
			if r.Final {
				m.Type = "final"
				seq++
			}
			s.send(ctx, m)
		case m := <-frames:
			switch {
			case m.err != nil:
				return
			case reason != "":
				// Audio after a finish is not recognised.
			case m.binary:
				if err := st.Send(m.data); err != nil {
					if errors.Is(err, speech.ErrUnsupported) {
						s.fail(ctx, httpjson.CodeBadParams)
						return
					}
					continue
				}
				s.bytes += len(m.data)
				idle.Reset(s.limits.Idle)
				if s.bytes >= maxBytes {
					finish(doneLength)
				}
			default:
				var c in
				switch {
				case json.Unmarshal(m.data, &c) != nil:
					s.fail(ctx, httpjson.CodeBadParams)
					return
				case c.Type == "finish":
					finish(doneFinish)
				case c.Type == "cancel":
					s.end(ctx, out{Type: "done", Reason: doneCancel}, websocket.StatusNormalClosure)
					return
				default:
					s.fail(ctx, httpjson.CodeBadParams)
					return
				}
			}
		}
	}
}
