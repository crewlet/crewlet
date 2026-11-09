package cdp

// Session is one session on a connection: the browser-level one ([Conn.Browser])
// or one attached to a target, flat on the same connection. It ends when its
// target detaches it ([DetachedError]), when the target crashes
// ([CrashedError]), when an event one of its listeners asked for is past
// [ReadLimit] ([ErrMessageTooBig]), when what its listeners hold passes
// [MaxBacklog] or is the most when its connection's would pass
// [MaxConnBacklog] ([ErrBacklog]), or when its connection ends; its calls in
// flight fail with that end and every later call fails at once, while calls
// on the connection's other sessions go on. The browser's own session ends
// only with its connection: what would end it ends the connection.
type Session struct {
	c    *Conn
	id   SessionID
	done chan struct{}

	// Guarded by c.mu.
	target    TargetInfo
	err       error // set once, before done closes
	listeners map[*Listener]struct{}
	backlog   int // bytes of the events its listeners hold, each once
}

func newSession(c *Conn, id SessionID, target TargetInfo) *Session {
	return &Session{c: c, id: id, target: target, done: make(chan struct{}), listeners: map[*Listener]struct{}{}}
}

// ID is the session's id: empty for the browser-level session.
func (s *Session) ID() SessionID { return s.id }

// Target is what Chromium said of the session's target when it attached it
// (Target.attachedToTarget's targetInfo): empty for the browser-level session,
// and for an id [Conn.Session] found no session of.
func (s *Session) Target() TargetInfo {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return s.target
}

// Done is closed once the session has ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err is why the session ended — a [*DetachedError], a [*CrashedError],
// [ErrMessageTooBig] for an event past ReadLimit one of its listeners asked
// for, [ErrBacklog] for what its listeners held, or its connection's end
// ([Conn.Err]) — and nil while it is open.
func (s *Session) Err() error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return s.err
}

// Conn is the connection the session is on.
func (s *Session) Conn() *Conn { return s.c }
