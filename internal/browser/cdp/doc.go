// Package cdp is the engine's Chrome DevTools Protocol client: a thin, typed
// client over one connection to a browser, a websocket ([Dial]) or a pair of
// pipes ([NewPipe]), with every session flat on that connection.
//
// # Thin, rather than chromedp or go-rod
//
// The engine calls about seventy commands and listens for about thirty events
// across fifteen domains. chromedp and go-rod each bring a generated surface of
// the whole protocol and a session model of their own — chromedp's actions on
// a context it owns, go-rod's pages and its own waits — and the browser's
// rules (a target held until it is set up, a request decided before it
// leaves, a crash that fails the calls waiting on it) would be written against
// their model and around it. So this package declares only what the engine
// sends and reads, each type holding exactly the fields its consumer reads or
// sends, by the protocol's own JSON names.
//
// # Sessions, answers and events
//
// One connection carries every session: the browser's own ([Conn.Browser])
// and one per attached target, each named by its sessionId (flat mode). A
// command's answer is matched to it by its id, one counter across every
// session; an event goes to its session's listeners that asked for it, in
// the order it arrived ([Session.Listen], [Listener.Next]). A child's
// Target.attachedToTarget registers it before it is delivered, so whatever
// Chromium sends the child next has a session to reach, and [Conn.Session]
// hands it back from then until Chromium detaches it.
//
// A session ends when Chromium detaches it ([*DetachedError]), when its
// target crashes ([*CrashedError], which Chromium itself would answer with
// nothing at all), when an event one of its listeners asked for is past
// [ReadLimit] ([ErrMessageTooBig]), when what its listeners hold passes its
// bound ([ErrBacklog]), or when its connection ends ([ErrClosed]). Its calls in
// flight fail with that end and every later call fails at once; its listeners
// hand over what they hold, then report it — but for a backlog's end, which
// lets go of what they held, and says so — and the connection's other sessions
// go on. The browser's own session ends only with its connection, since a
// connection without it could create, attach and resume nothing: what would
// end it ends the connection. A session that ended other than by Chromium's
// detach is still held, ended, until Chromium detaches it, so the children it
// announced, whose detaches arrive on it, still end. What a listener holds
// counts against its session's backlog and its connection's until it is taken
// or the listener closed, its session's end included, so a reader that stops
// closes its listener.
//
// # Holding new targets until they are set up
//
// [AutoAttach] turns auto-attach on, Chromium told to hold each new target at
// its start, sets each one up (the caller's Setup: a request gate, a binding,
// a script, each sent with [Send] rather than called), turns auto-attach on
// for it in turn — an out-of-process frame's frames, a page's or a worker's
// workers — and only then resumes it, behind what its Setup sent. So no
// target of the kinds measured that Chromium starts once auto-attach is on —
// a popup, with an opener or without, of its opener's site or another, a tab
// opened on about:blank, an out-of-process frame, a worker of any kind —
// runs a line before the rules the caller put on it, however long its Setup
// takes, as long as no tab still on about:blank opens a window. A resume of a
// page also releases every popup it holds, so the attacher resumes only a
// target Chromium holds, never one already running, and the two commands that
// hold and release a target, Target.setAutoAttach and
// Runtime.runIfWaitingForDebugger, are this package's alone. [AutoAttach]
// says what was measured, that any other kind of target is not known to be
// held, which two kinds Chromium does not hold — a target already running
// when auto-attach reached it, which Setup is told of (a pool's open tabs,
// the browser's first tab once it has started), and a tab created with an
// address — and why a tab still on about:blank opens no window: the resume of
// a held tab sharing its renderer can release the popup before the popup's
// own setup.
//
// # The bounds
//
// Every command has its own deadline, [CommandTimeout] from its sending under
// the caller's own context. Every message is read whole up to [ReadLimit], by
// this package's own reader on either transport; a larger one is read through
// and ends only what it is for — the call it answers, or the session whose
// listener asked for it — so a page that provokes one ends at most its own
// session. The events queued for a session's listeners are held to
// [MaxBacklog], each counted once however many of them hold it, so a page that
// outruns its reader ends its own session rather than grow the engine's heap;
// and every session's together to [MaxConnBacklog], past which the session
// holding the most ends, so the heap a connection holds is bounded however
// many sessions it carries. What the browser's own session meets — an event
// past ReadLimit, a backlog past a bound — ends the connection, so what a page
// can make arrive there, a request gate's paused requests among it, can end
// its whole browser, and a caller that must not let it listens for that on
// the page's own session instead. Each bound's reason is at its definition.
//
// # The floor
//
// [MinMajor] is the oldest Chromium major the engine drives, and
// [CheckVersion] refuses an older browser naming both versions. The value is
// measured, and floor.go says how; the suite holds it on every run against
// two pinned Chrome for Testing builds, the newest and the floor's own
// (scripts/chromium-pin.env), reading each one's own description of the
// protocol against every command, event and field declared here.
//
// That proof is complete by construction rather than by review: [Call] and
// [Send] send, and [Session.Listen] and [Event.Of] read, only an entry of the
// package's inventory with the very types it is declared with there, so a
// descriptor built by any other spelling — a type alias of Method, a name set
// on a zero one, a struct of its shape converted — sends and reads nothing;
// and [Method] and [Event] keep their names unexported, so only this package
// can declare one, and each of their instantiations is a type of its own, so
// no declared one converts to other parameters. A test holds the inventory
// to the package's declarations both ways; the commands the package's own
// tests send beside it are registered by a test file, and by no other. Every
// command anything in the engine can send is one the floor was measured
// against.
//
// # What is never logged
//
// A command's parameters can carry a password being typed and a result can
// carry the cookies of a kept sign-in, so neither is ever logged or put in an
// error: [*ProtocolError] names the method, Chromium's code and its message,
// and never its data, which may echo the parameters. The connection logs at
// DEBUG alone, and only three lines: cdp_answer_unmatched, an answer whose
// call had gone (at its deadline, or given up by its caller), by the answer's
// id and session, since the method went with the call; cdp_event_unrouted, an
// event for a session the connection holds no record of, by its method and
// session; and cdp_message_too_big, a message past [ReadLimit] read through,
// by its id or its method, its session and its size. Every end is the
// caller's to report, through Err, because only the caller knows whose
// browser it was.
//
// # What it does not decide
//
// Which targets to refuse, what a crash means for a seat, which requests a
// page may make, and how a page's words are fenced before a model reads them
// are the browser's to decide, in internal/browser as crewlet/crewlet#804
// builds it on this package: these protocol types carry no provenance tags,
// and the browser's own answer types are to declare which of their fields a
// page controls. The caller's HTTP client is the caller's too
// ([DialOptions]): this package refuses to dial without one, and never follows
// a redirect with it.
package cdp
