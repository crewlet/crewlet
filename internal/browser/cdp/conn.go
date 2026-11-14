package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
)

// ReadLimit is the largest message a connection reads whole, 32 MiB. A larger
// one ends nothing but what it is for. It is read through and let go of as it
// streams past, kept only as far as routing it needs — the id or the method
// Chromium writes first, and the session it writes last — and then an answer
// that large fails its own call with [ErrMessageTooBig], an event that large
// ends its own session with it when a listener of that session asked for it,
// and any other is dropped, as an event no listener asked for always is. The
// connection and its other sessions go on: a page can provoke such a message
// at will — a console line or a call of a binding, once its session has
// enabled Runtime, or a document whose accessibility tree is long — and so
// what it provokes ends at most its own session. The browser's own session
// ([Conn.Browser]), which no target owns, ends only with its connection, so an
// event past ReadLimit that one of its listeners asked for ends the
// connection. A page can provoke one there too: a request paused by a gate on
// the browser's session carries the request's body, so a POST of tens of
// megabytes passes ReadLimit (on Chromium 157, measured on 2026-11-11, a POST
// of 40 MiB was paused as an event of 97.9 MB), and ends its whole browser. A
// caller that must not let a page do that listens for such events on the
// page's own session rather than the browser's.
//
// 32 MiB is what the largest answer the browser asks for by design needs: a
// screenshot, which its screenshot tool holds to 2,000 pixels on its long edge
// at its source (crewlet/crewlet#804), is at worst a 16 MB PNG, about 21.3 MiB
// once base64 has carried it, and 32 MiB is that and half again. Nothing holds
// every other answer under it: an accessibility tree has no bound at its
// source, since Accessibility.getFullAXTree's depth bounds how deep the tree
// goes and never how many nodes come back (on Chromium 157, measured on
// 2026-11-11, a table of 4,000 rows of six cells answered 26.8 MiB and one of
// 6,000 rows passed this limit), so a caller that asks for one, or listens for
// an event a page can make as large, holds it below ReadLimit itself or meets
// ErrMessageTooBig. The websocket library's own limit, 32 KiB by default, is
// lifted: a message past ReadLimit is read through, never refused.
const ReadLimit = 32 << 20

// envelopeBytes is how much of a message past ReadLimit the reader keeps to
// route it, of its start and of its end each: Chromium writes an answer's id
// and an event's method first and a child session's id last, each a few dozen
// bytes (a method's name is under a hundred, a session's id 32 hex digits),
// and a kibibyte leaves room for an intermediary that writes a small member
// before them.
const envelopeBytes = 1 << 10

// MaxBacklog is the most one session holds for its listeners, in bytes of
// events queued and not yet taken: [ReadLimit], so the largest message the
// connection reads whole can always be queued for a session that holds
// nothing, and a session never holds more than one such message's worth. A
// page that outruns its reader by more ends its own session with [ErrBacklog]
// instead of growing the engine's heap, and nothing else: its connection and
// every other session go on, as at a message past ReadLimit. A heavy page's
// paused requests are a few megabytes. An event no listener asked for is
// never queued and counts nothing, and an event several listeners of a session
// asked for shares one copy among them and counts once, until the last of them
// takes it or lets it go.
const MaxBacklog = ReadLimit

// MaxConnBacklog is the most a connection holds for every session's listeners
// together, its backstop: twice [MaxBacklog], room for one session holding the
// most any one may and as much again for all the rest together, so only a page
// flooding several of its targets at once comes near it. An event that would
// pass it ends the session holding the most — which lets go of what its
// listeners held, so the room is there at once — until the event fits, or ends
// the event's own session when that one holds the most; a session that has
// ended already lets go of what its listeners still held the same way, and the
// browser's own session, which ends only with its connection, ends the
// connection. So the heap one connection holds is bounded however many
// sessions it carries, and what passes the bound ends the session that holds
// the most, never whichever one an event happens to be for.
const MaxConnBacklog = 2 * MaxBacklog

// CommandTimeout bounds every command: its deadline is the earlier of the
// caller's context and CommandTimeout from its sending. No command waits on a
// page — waiting for a page is built on events, under its own bound — so a
// command still unanswered after a minute is one Chromium will not answer: a
// hung renderer, or a target gone without a detach. A minute is twice the
// longest wait the browser's tools are to offer (30 s, crewlet/crewlet#804),
// and the idle timeout every common HTTP front gives a response's first byte
// (nginx's proxy_read_timeout, a cloud load balancer's), which bounds a
// navigation's commit. A liveness bound under the caller's own deadline, never
// a budget. It bounds each write too: a browser that takes no frame for a
// minute is one that will not answer the command it carries.
const CommandTimeout = 60 * time.Second

// transport is one way to carry messages to and from a browser: a websocket
// ([Dial]) or a pair of pipes ([NewPipe]).
type transport interface {
	// read returns the next message — whole when it is at most ReadLimit
	// bytes, and past that only what routing it needs (gather) — or the
	// error that ended the transport.
	read(ctx context.Context) (message, error)
	// write sends one whole message, under the connection's own context,
	// which never ends: only close frees a write the peer never takes, and
	// the connection's writer closes it at CommandTimeout, once it has said
	// why (writeLoop).
	write(ctx context.Context, msg []byte) error
	// close ends the transport, unblocking a read or a write in progress.
	// Only its first call does anything.
	close() error
}

// message is one message a transport read: whole, or for one past ReadLimit
// only what routing it needs.
type message struct {
	body []byte
	big  *oversized // set, and body nil, for a message past ReadLimit
}

// oversized is a message past ReadLimit as its transport kept it: its first
// and its last envelopeBytes, and its size.
type oversized struct {
	head, tail []byte
	size       int64
}

// gather collects one message a slice at a time, as a transport reads it:
// whole while it is at most ReadLimit bytes, and once it is longer only its
// start, its end and its size, the rest let go of as it streams past — so no
// message holds more than ReadLimit and two envelopes, whatever its length.
type gather struct {
	body []byte
	big  *oversized
}

func (g *gather) add(p []byte) {
	switch {
	case g.big != nil:
		g.big.size += int64(len(p))
		g.big.tail = lastBytes(g.big.tail, p)
	case len(g.body)+len(p) <= ReadLimit:
		g.body = append(g.body, p...)
	default:
		g.big = &oversized{
			head: firstBytes(g.body, p),
			tail: lastBytes(g.body, p),
			size: int64(len(g.body) + len(p)),
		}
		g.body = nil
	}
}

// message is what g gathered.
func (g *gather) message() message {
	if g.big != nil {
		return message{big: g.big}
	}
	return message{body: g.body}
}

// firstBytes is a copy of the first envelopeBytes of a and b, read as one.
func firstBytes(a, b []byte) []byte {
	out := make([]byte, 0, envelopeBytes)
	out = append(out, a[:min(len(a), envelopeBytes)]...)
	return append(out, b[:min(len(b), envelopeBytes-len(out))]...)
}

// lastBytes is a copy of the last envelopeBytes of a and b, read as one.
func lastBytes(a, b []byte) []byte {
	if len(b) >= envelopeBytes {
		return bytes.Clone(b[len(b)-envelopeBytes:])
	}
	out := make([]byte, 0, envelopeBytes)
	out = append(out, a[max(0, len(a)-(envelopeBytes-len(b))):]...)
	return append(out, b...)
}

// errNoFrameTaken ends a connection whose browser took no frame for
// CommandTimeout, whichever its transport.
var errNoFrameTaken = fmt.Errorf("the browser took no frame for %s", CommandTimeout)

// Conn is one connection to a browser: one transport, one goroutine reading
// it and one writing it, both owned by the Conn and ended by its end; a table
// of the calls in flight, keyed by one command id across every session; and
// the table of its sessions, each attached to one target, all flat on this
// connection.
type Conn struct {
	t   transport
	log *slog.Logger
	ids atomic.Int64
	// writes hands a frame to the writer, unbuffered: a frame waits with its
	// caller until the writer takes it, so a caller that gives up first — at
	// its context, its deadline or its session's end — never sends it. A
	// queue would only move that wait from the frame to its answer, which the
	// caller waits for either way, and would send a command its caller had
	// already called off.
	writes chan []byte
	done   chan struct{}
	wg     sync.WaitGroup

	mu       sync.Mutex
	err      error // why it ended; set once, before done closes
	pending  map[int64]*pending
	sessions map[SessionID]*Session
	browser  *Session
	// backlog is the bytes every session's listeners hold, each event once
	// (MaxConnBacklog); holders is every session whose listeners hold any,
	// open or ended, which is where the one holding the most is looked for.
	backlog int
	holders map[*Session]struct{}
}

// pending is a call waiting for its answer.
type pending struct {
	session *Session
	reply   chan reply // buffered: whoever answers never waits
}

// reply is what ends a call: Chromium's result or its error, an answer past
// ReadLimit, or the end of the call's session or connection.
type reply struct {
	result json.RawMessage
	fault  *wireError
	end    error
	tooBig int64 // the size of an answer past ReadLimit, read through
}

// wireError is Chromium's error object. Its data is never read: it may echo
// the command's parameters.
type wireError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// envelope is all the reader decodes of a message: an answer's id, result and
// error, or an event's method, and the session either belongs to. An event's
// parameters stay raw, decoded on the goroutine that takes them, but for the
// few events the connection reads itself (a target attached or detached, a
// session detached, a target crashed), each into a struct of its ids.
type envelope struct {
	ID        int64           `json:"id"`
	Result    json.RawMessage `json:"result"`
	Error     *wireError      `json:"error"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID SessionID       `json:"sessionId"`
}

// newConn starts a connection over t. Its two goroutines take ctx's values (a
// trace, say) and none of its cancellation: the connection outlives the call
// that opened it, and ends only by its own end.
func newConn(ctx context.Context, t transport, log *slog.Logger) *Conn {
	if log == nil {
		log = logging.Get("cdp")
	}
	c := &Conn{
		t:        t,
		log:      log,
		writes:   make(chan []byte),
		done:     make(chan struct{}),
		pending:  map[int64]*pending{},
		sessions: map[SessionID]*Session{},
		holders:  map[*Session]struct{}{},
	}
	c.browser = newSession(c, "", TargetInfo{})
	c.sessions[""] = c.browser
	base := context.WithoutCancel(ctx)
	c.wg.Add(2)
	go c.readLoop(base)
	go c.writeLoop(base)
	return c
}

// Browser is the browser-level session, the one with no session id, over
// which targets are created, attached and watched. It ends only with its
// connection: a connection whose browser session had ended could create,
// attach and resume nothing, and would only carry what was attached already,
// so what would end that session — an event past [ReadLimit] one of its
// listeners asked for, what its listeners hold passing [MaxBacklog], or its
// holding the most when its connection's would pass [MaxConnBacklog] — ends
// the connection instead.
func (c *Conn) Browser() *Session { return c.browser }

// Session is the attached session id names, as the connection holds it: every
// one Chromium announced with Target.attachedToTarget, which the connection
// registers before it delivers the announcement, from that announcement until
// Chromium detaches it. Chromium announces a session before it answers the
// Target.attachToTarget that made it, so the session that answer names is
// held by the time the answer is read. A session that ended otherwise — its
// target crashed, or an event or a backlog of its passed a bound — is handed
// back as it is, ended, until then.
//
// An id the connection holds no session of — one Chromium detached, which the
// connection forgets, or one never attached on it — is a session that has
// ended already, with a [*DetachedError] saying so, and its calls fail at once
// and send nothing; nothing registers it, since nothing would ever end it. On
// a connection that has ended it is a session that ended with the connection.
func (c *Conn) Session(id SessionID) *Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sessions[id]; ok {
		return s
	}
	end := c.err
	if end == nil {
		end = &DetachedError{SessionID: id, Reason: "the connection holds no such session"}
	}
	s := newSession(c, id, TargetInfo{})
	c.endSessionLocked(s, end)
	return s
}

// Close ends the connection: every call in flight and every later one fails
// with [ErrClosed], every listener drains and then reports it, and the
// transport is closed — for a pipe, which makes Chromium exit. It waits for
// the connection's goroutines to end, and is idempotent: a second Close, or
// one after the connection ended by itself, returns nil.
func (c *Conn) Close() error {
	err := c.end(ErrClosed)
	c.wg.Wait()
	return err
}

// Done is closed once the connection has ended.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended: [ErrClosed] (wrapping a transport's own
// error where there was one), or what ended the browser's own session, which
// ends only with its connection — [ErrMessageTooBig] for an event past
// ReadLimit one of its listeners asked for, [ErrBacklog] for a backlog past
// its bound; nil while it is open.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// end ends the connection with err, once, and closes the transport; it reports
// the transport's close error from the end that closed it, and nil from any
// later one.
func (c *Conn) end(err error) error {
	c.mu.Lock()
	first := c.endLocked(err)
	c.mu.Unlock()
	if !first {
		return nil
	}
	return c.t.close()
}

// endLocked records err as the connection's end and ends every session with
// it; it reports whether this was the end that did so. The caller closes the
// transport once it has let go of c.mu.
func (c *Conn) endLocked(err error) bool {
	if c.err != nil {
		return false
	}
	c.err = err
	for _, s := range c.sessions {
		c.endSessionLocked(s, err)
	}
	close(c.done)
	return true
}

// readLoop reads the transport until it ends, routing each message.
func (c *Conn) readLoop(ctx context.Context) {
	defer c.wg.Done()
	for {
		msg, err := c.t.read(ctx)
		if err != nil {
			_ = c.end(fmt.Errorf("%w: %w", ErrClosed, err))
			return
		}
		var routed bool
		if msg.big != nil {
			routed = c.routeOversized(msg.big)
		} else {
			routed = c.route(msg.body)
		}
		if !routed {
			// Ended while routing (what ended the browser's own session, or
			// a message that is no CDP message): the transport closes here,
			// once c.mu is free.
			_ = c.t.close()
			return
		}
	}
}

// writeLoop hands each frame a call gives it to the transport, one at a time,
// until the connection ends.
func (c *Conn) writeLoop(ctx context.Context) {
	defer c.wg.Done()
	for {
		select {
		case frame := <-c.writes:
			// Each write is bounded here, on either transport, and never
			// by a caller's context: a caller that gives up must not take
			// the connection's next frame down with it, and a browser that
			// takes no frame for CommandTimeout will not answer the command
			// it carries. At that bound the connection ends, saying so,
			// before its transport closes and frees the write, so what it
			// reports is never the closed transport's own words, and never
			// whichever of its reader and its writer saw them first.
			stalled := time.AfterFunc(CommandTimeout, func() {
				_ = c.end(fmt.Errorf("%w: %w", ErrClosed, errNoFrameTaken))
			})
			err := c.t.write(ctx, frame)
			stalled.Stop()
			if err != nil {
				_ = c.end(fmt.Errorf("%w: %w", ErrClosed, err))
				return
			}
		case <-c.done:
			return
		}
	}
}

// route hands one message to the call it answers or the session it belongs
// to; it reports false once the connection has ended, which ends the reader.
func (c *Conn) route(msg []byte) bool {
	var env envelope
	if err := json.Unmarshal(msg, &env); err != nil || (env.ID == 0 && env.Method == "") {
		c.mu.Lock()
		c.endLocked(fmt.Errorf("%w: the peer sent a message of %d bytes that is no CDP message", ErrClosed, len(msg)))
		c.mu.Unlock()
		return false
	}
	if env.ID != 0 {
		c.answer(env)
		return true
	}
	return c.event(env)
}

// routeOversized ends what a message past ReadLimit is for, and nothing else:
// the call an answer that large answers fails with ErrMessageTooBig, and the
// session an event that large belongs to ends with it if a listener of that
// session asked for the event — which it can no longer be handed, and which a
// listener must not silently miss — while an event no listener asked for is
// dropped, as one of any size is. The browser's own session ends only with its
// connection (Conn.Browser), so an event of its that a listener asked for ends
// the connection. Each is said at DEBUG by its id or method, its session and
// its size, never by anything of the rest. A message whose start names neither
// an id nor a method is no CDP message, and ends the connection as a whole one
// would; Chromium writes one or the other first in every message it sends.
//
// The events the connection reads itself — a target attached or detached, a
// session detached, a target crashed — carry ids, a reason, a status and a
// target's address and title, never anything near ReadLimit, so none of them
// is ever read through here unapplied.
func (c *Conn) routeOversized(o *oversized) bool {
	h := readHead(o.head)
	session := h.session
	if !h.named {
		session = tailSession(o.tail)
	}
	if h.id == 0 && h.method == "" {
		c.mu.Lock()
		c.endLocked(fmt.Errorf("%w: the peer sent a message of %d bytes that is no CDP message", ErrClosed, o.size))
		c.mu.Unlock()
		return false
	}
	c.log.Debug("cdp_message_too_big", "id", h.id, "method", h.method, "session", session, "bytes", o.size)
	if h.id != 0 {
		c.mu.Lock()
		p := c.pending[h.id]
		delete(c.pending, h.id)
		c.mu.Unlock()
		if p != nil {
			p.reply <- reply{tooBig: o.size}
		}
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false
	}
	s, ok := c.sessions[session]
	if !ok || s.err != nil {
		return true
	}
	for l := range s.listeners {
		if l.events[h.method] {
			return c.endSessionOrConnLocked(s, fmt.Errorf("cdp: %s ended at a %s event of %d bytes: %w",
				endName(s), h.method, o.size, ErrMessageTooBig))
		}
	}
	return true
}

// sessionName is how an error names the session id names.
func sessionName(id SessionID) string {
	if id == "" {
		return "the browser's own session"
	}
	return "session " + string(id)
}

// endName is how an error names what ending s ends: s, and for the browser's
// own session the connection with it.
func endName(s *Session) string {
	if s.id == "" {
		return "the browser's own session, and with it the connection,"
	}
	return sessionName(s.id)
}

// head is what the start of a message past ReadLimit says of it.
type head struct {
	id      int64
	method  string
	session SessionID
	named   bool // a member of the start named the session
}

// readHead reads the members a message's start holds whole, in whatever order
// they come: an answer's id, an event's method, and a session's id where one
// precedes the cut. It stops at the first member the start cuts short.
func readHead(b []byte) head {
	var h head
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return h
	}
	for d.More() {
		t, err := d.Token()
		key, ok := t.(string)
		if err != nil || !ok {
			return h
		}
		switch key {
		case "id":
			var n json.Number
			if d.Decode(&n) != nil {
				return h
			}
			if h.id, err = n.Int64(); err != nil {
				h.id = 0
				return h
			}
		case "method":
			if d.Decode(&h.method) != nil {
				return h
			}
		case "sessionId":
			if d.Decode(&h.session) != nil {
				return h
			}
			h.named = true
		default:
			var skip json.RawMessage
			if d.Decode(&skip) != nil {
				return h
			}
		}
	}
	return h
}

// tailSession is the session a message's end names: Chromium appends a child
// session's id to every message it sends for it, as the object's last member,
// and a message of the browser's own session has none. So a message ending in
// a top-level `"sessionId":"<id>"` member is that session's, and any other
// end — an object or an array closing the last member, as every member of the
// browser's own messages does — is the browser's.
//
// It reads that member back from the end, past any blanks: the object's '}',
// the id's closing quote and its opening one, the ':' before the id, the name
// "sessionId" before the ':', and the ',' or '{' before the name. No quote
// inside a string passes for one of the member's, so no value or name that
// merely ends as the member does is read as it: inside a string a quote is
// escaped, so the byte before it is a backslash, and what is read before each
// of the member's quotes never is — the ':' and the name before the id's
// opening quote, the name's last letter before its closing quote, the ',' or
// '{' before its opening one — while the id's closing quote, the last byte
// but blanks before the '}', ends a string in any message that parses. An id
// holding a backslash would have to be unescaped to name a session, which no
// id Chromium makes needs, so it names none.
func tailSession(b []byte) SessionID {
	i := lastToken(b, len(b))
	if i < 0 || b[i] != '}' {
		return ""
	}
	closing := lastToken(b, i)
	if closing < 0 || b[closing] != '"' {
		return ""
	}
	opening := bytes.LastIndexByte(b[:closing], '"')
	if opening < 0 {
		return ""
	}
	id := b[opening+1 : closing]
	if bytes.IndexByte(id, '\\') >= 0 {
		return ""
	}
	colon := lastToken(b, opening)
	if colon < 0 || b[colon] != ':' {
		return ""
	}
	const name = `"sessionId"`
	end := lastToken(b, colon) + 1
	start := end - len(name)
	if start < 0 || string(b[start:end]) != name {
		return ""
	}
	if before := lastToken(b, start); before < 0 || (b[before] != ',' && b[before] != '{') {
		return ""
	}
	return SessionID(id)
}

// lastToken is the index of the last byte before i that is not JSON's white
// space, or -1.
func lastToken(b []byte, i int) int {
	for i--; i >= 0; i-- {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
		default:
			return i
		}
	}
	return -1
}

// answer hands a call its reply. A call that is gone — at its deadline, or
// because its caller gave up — leaves the answer to nobody, said at DEBUG by
// its ids alone: never its result, which may be a cookie.
func (c *Conn) answer(env envelope) {
	c.mu.Lock()
	p := c.pending[env.ID]
	delete(c.pending, env.ID)
	c.mu.Unlock()
	if p == nil {
		c.log.Debug("cdp_answer_unmatched", "id", env.ID, "session", env.SessionID)
		return
	}
	p.reply <- reply{result: env.Result, fault: env.Error}
}

// event hands an event to the listeners of its session that asked for it,
// and applies what the connection itself reads of it: a target attached, a
// session detached, a target crashed. It reports false once the connection
// has ended.
func (c *Conn) event(env envelope) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false
	}
	s, ok := c.sessions[env.SessionID]
	if !ok {
		// A session this connection holds no record of: one Chromium
		// detached, or one nobody attached here. Said by its method and
		// session alone.
		c.log.Debug("cdp_event_unrouted", "method", env.Method, "session", env.SessionID)
		return true
	}
	// A child is registered BEFORE its announcement is delivered, so the
	// messages Chromium sends it next always find a session — and with the
	// target it is attached to, which a crash of that target names. A second
	// announcement of a session held already changes nothing.
	if env.Method == TargetAttachedToTarget.name {
		var a struct {
			SessionID  SessionID  `json:"sessionId"`
			TargetInfo TargetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(env.Params, &a) == nil && a.SessionID != "" && a.TargetInfo.TargetID != "" {
			if _, held := c.sessions[a.SessionID]; !held {
				c.sessions[a.SessionID] = newSession(c, a.SessionID, a.TargetInfo)
			}
		}
	}
	// A session that has ended hands its listeners nothing more: they have its
	// end to report, after what they hold. Only a child ends while its
	// connection is open — the browser's own session ends only with it — and
	// one that ended here rather than by Chromium's detach (its target
	// crashed, or an event or a backlog of its passed a bound) is held, ended,
	// until Chromium detaches it: the children it announced are detached on
	// it and nowhere else, so what the connection reads of its events, below
	// and above, still applies.
	if s.err == nil && !c.deliverLocked(s, env) {
		return false
	}
	// The ends, applied AFTER delivery, so a listener on the session that
	// ends takes the event that ended it before it takes the end.
	switch env.Method {
	case TargetDetachedFromTarget.name:
		var d struct {
			SessionID SessionID `json:"sessionId"`
		}
		if json.Unmarshal(env.Params, &d) == nil {
			if child, held := c.sessions[d.SessionID]; held && child != c.browser {
				c.endSessionLocked(child, &DetachedError{SessionID: d.SessionID, Reason: "detached from its target"})
				c.forgetLocked(child)
			}
		}
	case InspectorDetached.name:
		if s != c.browser {
			var d InspectorDetachedEvent
			_ = json.Unmarshal(env.Params, &d)
			c.endSessionLocked(s, &DetachedError{SessionID: s.id, Reason: d.Reason})
			c.forgetLocked(s)
		}
	case TargetTargetCrashed.name:
		var crash TargetTargetCrashedEvent
		if json.Unmarshal(env.Params, &crash) == nil && crash.TargetID != "" {
			c.crashLocked(&CrashedError{TargetID: crash.TargetID, Status: crash.Status, ErrorCode: crash.ErrorCode})
		}
	case InspectorTargetCrashed.name:
		if s != c.browser {
			c.crashLocked(&CrashedError{TargetID: s.target.TargetID})
		}
	}
	return true
}

// crashLocked ends every session attached to the crashed target.
func (c *Conn) crashLocked(crash *CrashedError) {
	for _, s := range c.sessions {
		if s != c.browser && s.target.TargetID == crash.TargetID {
			c.endSessionLocked(s, crash)
		}
	}
}

// deliverLocked queues the event on every listener of s that asked for it,
// one copy shared among them and counted once against s's backlog and the
// connection's. An event that would pass s's bound ends s (MaxBacklog), and
// one that would pass the connection's ends the session holding the most
// until it fits (MaxConnBacklog), s itself when that is s; either end lets go
// of what the ended session's listeners held. It reports false when that
// ended the connection: the browser's own session, ending, ends it.
func (c *Conn) deliverLocked(s *Session, env envelope) bool {
	var to []*Listener
	for l := range s.listeners {
		if l.events[env.Method] {
			to = append(to, l)
		}
	}
	if len(to) == 0 {
		return true
	}
	size := messageSize(env.Method, env.Params)
	if s.backlog+size > MaxBacklog {
		err := fmt.Errorf("cdp: %s ended: its listeners held %d bytes of events, and a %s event of %d more "+
			"would pass MaxBacklog: %w", endName(s), s.backlog, env.Method, size, ErrBacklog)
		c.letGoLocked(s)
		return c.endSessionOrConnLocked(s, err)
	}
	for c.backlog+size > MaxConnBacklog {
		most := s
		for h := range c.holders {
			if h.backlog > most.backlog {
				most = h
			}
		}
		err := fmt.Errorf("cdp: %s ended: its listeners held the most, %d of the %d bytes of events its "+
			"connection's held, and a %s event of %d more would pass MaxConnBacklog: %w",
			endName(most), most.backlog, c.backlog, env.Method, size, ErrBacklog)
		c.letGoLocked(most)
		if !c.endSessionOrConnLocked(most, err) {
			return false
		}
		if most == s {
			return true
		}
	}
	sh := &share{size: size, holders: len(to)}
	s.backlog += size
	c.backlog += size
	c.holders[s] = struct{}{}
	for _, l := range to {
		l.queue = append(l.queue, queued{m: Message{Method: env.Method, Params: env.Params}, share: sh})
		l.signal()
	}
	return true
}

// share is one event as the listeners of one session hold it: one copy among
// all of them, counted once against the session's backlog and the
// connection's until the last of them takes it or lets it go.
type share struct {
	size    int
	holders int
}

// queued is one event a listener holds: the event, and the share it is a
// holder of.
type queued struct {
	m     Message
	share *share
}

// dropLocked is one holder of sh, a share of s's, letting go of it; the last
// takes its bytes off s's backlog and the connection's.
func (c *Conn) dropLocked(s *Session, sh *share) {
	sh.holders--
	if sh.holders > 0 {
		return
	}
	s.backlog -= sh.size
	c.backlog -= sh.size
	if s.backlog == 0 {
		delete(c.holders, s)
	}
}

// letGoLocked lets go of every event s's listeners hold, which each of them
// says when it reaches s's end (Listener.Next): it is what a session that
// passes a backlog's bound gives back, so the room is there at once rather
// than once its slow reader has drained it.
func (c *Conn) letGoLocked(s *Session) {
	for l := range s.listeners {
		if len(l.queue) == 0 {
			continue
		}
		for _, h := range l.queue {
			c.dropLocked(s, h.share)
		}
		l.queue = nil
		l.lost = true
		l.signal()
	}
}

// messageSize is what one queued event counts against MaxBacklog and
// MaxConnBacklog: added as it is queued, and taken off as the last listener
// holding it takes it or lets it go, so the one definition is what every side
// of the count reads.
func messageSize(method string, params json.RawMessage) int { return len(method) + len(params) }

// endSessionOrConnLocked ends s with err — or, s being the browser's own
// session, which ends only with its connection, the connection — and reports
// false when the connection ended; the reader closes the transport as it
// returns, once c.mu is free.
func (c *Conn) endSessionOrConnLocked(s *Session, err error) bool {
	if s == c.browser {
		c.endLocked(err)
		return false
	}
	c.endSessionLocked(s, err)
	return true
}

// endSessionLocked ends s with err, once: its calls in flight fail with err,
// and its listeners drain and then report err. The connection still holds s,
// ended, until Chromium detaches it (forgetLocked): the detaches of the
// children s announced arrive on s and nowhere else, so what the connection
// reads of its events goes on applying. The browser's own session is ended
// here only by its connection's end (endLocked).
func (c *Conn) endSessionLocked(s *Session, err error) {
	if s.err != nil {
		return
	}
	s.err = err
	close(s.done)
	for id, p := range c.pending {
		if p.session == s {
			delete(c.pending, id)
			p.reply <- reply{end: err}
		}
	}
	for l := range s.listeners {
		l.signal()
	}
}

// forgetLocked drops s, which Chromium has detached, from the sessions the
// connection holds: a later event for its id is unrouted, and Conn.Session
// hands the id back as a session that has ended.
func (c *Conn) forgetLocked(s *Session) {
	if c.sessions[s.id] == s && s != c.browser {
		delete(c.sessions, s.id)
	}
}

// call sends m with params on s and waits for its answer, its deadline or the
// caller's context.
func (c *Conn) call(ctx context.Context, s *Session, m declared, params any) (json.RawMessage, error) {
	inFlight, err := c.send(ctx, s, m, params)
	if err != nil {
		return nil, err
	}
	return inFlight.wait(ctx)
}

// inFlight is a command the connection's writer has taken: the reply that
// ends it and the deadline that runs from its sending.
type inFlight struct {
	c        *Conn
	id       int64
	method   string
	reply    chan reply
	deadline *time.Timer
}

// send hands m with params on s to the connection's writer and returns once
// the writer has taken it, or why nothing was sent. It builds every frame the
// connection's writer sends, so it holds m to the inventory itself
// (checkDeclared) before anything else: no command reaches the browser that
// the floor was not measured against.
func (c *Conn) send(ctx context.Context, s *Session, m declared, params any) (*inFlight, error) {
	if err := checkDeclared(m); err != nil {
		return nil, err
	}
	method := m.Name()
	// A context that has ended sends nothing, as net/http sends no request
	// under one: offered to the writer, the frame would race the context's
	// end and be sent about one time in two — a click, a key or a navigation
	// its caller had already called off.
	if ctx.Err() != nil {
		return nil, fmt.Errorf("cdp: %s: %w", method, context.Cause(ctx))
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("cdp: %s: its parameters cannot be encoded: %w", method, err)
	}
	id := c.ids.Add(1)
	frame, err := json.Marshal(struct {
		ID        int64           `json:"id"`
		Method    string          `json:"method"`
		Params    json.RawMessage `json:"params"`
		SessionID SessionID       `json:"sessionId,omitempty"`
	}{id, method, body, s.id})
	if err != nil {
		return nil, fmt.Errorf("cdp: %s: %w", method, err)
	}
	r := make(chan reply, 1)
	c.mu.Lock()
	if ended := s.err; ended != nil {
		c.mu.Unlock()
		return nil, ended
	}
	c.pending[id] = &pending{session: s, reply: r}
	c.mu.Unlock()

	// One deadline per command, from its sending: never one per connection,
	// which a command sent later would inherit from one sent earlier. The
	// frame waits here until the writer takes it, so a caller that gives up
	// first sends nothing.
	deadline := time.NewTimer(CommandTimeout)
	select {
	case c.writes <- frame:
		return &inFlight{c: c, id: id, method: method, reply: r, deadline: deadline}, nil
	case got := <-r:
		// Only an end answers a command not yet sent.
		deadline.Stop()
		_, err := got.unwrap(method)
		return nil, err
	case <-deadline.C:
		c.forget(id)
		return nil, unanswered(method)
	case <-ctx.Done():
		deadline.Stop()
		c.forget(id)
		return nil, fmt.Errorf("cdp: %s: %w", method, context.Cause(ctx))
	}
}

// wait is the command's answer, or its failure at its deadline or at ctx's
// end, which gives up on it: an answer Chromium sends after that is
// unmatched.
func (f *inFlight) wait(ctx context.Context) (json.RawMessage, error) {
	defer f.deadline.Stop()
	select {
	case got := <-f.reply:
		return got.unwrap(f.method)
	case <-f.deadline.C:
		f.c.forget(f.id)
		return nil, unanswered(f.method)
	case <-ctx.Done():
		f.c.forget(f.id)
		return nil, fmt.Errorf("cdp: %s: %w", f.method, context.Cause(ctx))
	}
}

// forget drops a call nobody waits for any more; its answer, if Chromium
// sends one, is unmatched.
func (c *Conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// unanswered is a command's failure at CommandTimeout.
func unanswered(method string) error {
	return fmt.Errorf("cdp: %s was not answered within %s: %w", method, CommandTimeout, context.DeadlineExceeded)
}

// unwrap is the reply as a call's result or error.
func (r reply) unwrap(method string) (json.RawMessage, error) {
	switch {
	case r.end != nil:
		return nil, r.end
	case r.tooBig > 0:
		return nil, fmt.Errorf("cdp: %s answered %d bytes: %w", method, r.tooBig, ErrMessageTooBig)
	case r.fault != nil:
		return nil, &ProtocolError{Method: method, Code: r.fault.Code, Message: r.fault.Message}
	}
	return r.result, nil
}
