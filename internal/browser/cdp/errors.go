package cdp

import (
	"errors"
	"fmt"
)

// ErrClosed is how a connection ends when its transport closes — Close, the
// peer going away, a read or a write that failed — and the answer of every
// call in flight on it and every call after. A transport's own error, where
// there is one, is wrapped beside it.
var ErrClosed = errors.New("cdp: the connection is closed")

// ErrMessageTooBig is what a message larger than [ReadLimit] ends, and all it
// ends: the call it answers, or the session it is an event of when a listener
// of that session asked for the event — never any other session, and never
// the connection, but for an event of the browser's own session, which ends
// only with its connection ([Conn.Browser]).
var ErrMessageTooBig = errors.New("cdp: the browser sent a message larger than ReadLimit")

// ErrBacklog ends a session whose listeners' undelivered events passed
// [MaxBacklog], or held the most when its connection's together would pass
// [MaxConnBacklog]: a page that outran its reader ends its own session rather
// than grow the engine's heap, and the session lets go of what its listeners
// held. It ends the connection only when that session is the browser's own,
// which ends only with its connection.
var ErrBacklog = errors.New("cdp: the events waiting for a session's listeners passed their bound")

// ErrNoHTTPClient refuses a [Dial] without DialOptions.HTTPClient, before
// anything is dialled: the websocket library would otherwise fall back to the
// process-global client, which no pool guard can see.
var ErrNoHTTPClient = errors.New("cdp: Dial needs DialOptions.HTTPClient, the caller's own client (internal/httpx)")

// ErrRedirected is a [Dial] whose upgrade was answered with a redirect, which
// is never followed: a CDP endpoint never redirects, and a followed redirect
// would carry DialOptions.Header — a pool's credentials — wherever it points.
var ErrRedirected = errors.New("cdp: the endpoint answered the upgrade with a redirect, which is never followed")

// ErrListenerClosed is a [Listener]'s answer once its own Close has run.
var ErrListenerClosed = errors.New("cdp: the listener is closed")

// DetachedError ends a session whose target detached it — Chromium's
// Target.detachedFromTarget on its parent, or Inspector.detached on the session
// itself — and is the answer of every call in flight on it and every call
// after. [Conn.Session] hands back a session ended with one for an id the
// connection holds no session of.
type DetachedError struct {
	// SessionID is the session that ended.
	SessionID SessionID
	// Reason is Chromium's, where it gave one (Inspector.detached's
	// "target_closed", "Render process gone.", …); "detached from its
	// target" for Target.detachedFromTarget, which carries none; or "the
	// connection holds no such session" for [Conn.Session]'s.
	Reason string
}

func (e *DetachedError) Error() string {
	return fmt.Sprintf("cdp: session %s was detached: %s", e.SessionID, e.Reason)
}

// CrashedError ends every session attached to a target whose renderer
// crashed — Chromium's Target.targetCrashed at the browser level, or
// Inspector.targetCrashed on one of its sessions — and is the answer of every
// call in flight on them, which Chromium would otherwise leave unanswered.
type CrashedError struct {
	// TargetID is the target that crashed.
	TargetID TargetID
	// Status and ErrorCode are Target.targetCrashed's, the renderer's
	// termination status and exit code; Inspector.targetCrashed carries
	// neither, and leaves them zero.
	Status    string
	ErrorCode int64
}

func (e *CrashedError) Error() string {
	if e.Status == "" {
		return fmt.Sprintf("cdp: target %s crashed", e.TargetID)
	}
	return fmt.Sprintf("cdp: target %s crashed (%s, %d)", e.TargetID, e.Status, e.ErrorCode)
}

// ProtocolError is Chromium's refusal of one command: the method, and
// Chromium's own code and message. It never carries the command's parameters
// or Chromium's error data, which may echo them: a parameter can be a password
// being typed or a cookie being restored.
type ProtocolError struct {
	Method  string
	Code    int64
	Message string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("cdp: %s: %s (%d)", e.Method, e.Message, e.Code)
}

// MethodNotFound is JSON-RPC's code for a method the receiver does not serve,
// which Chromium answers for a domain a target's type has none of — a worker
// asked for Page, a tab target for Runtime — as a [*ProtocolError]'s Code.
const MethodNotFound = -32601

// VersionError refuses a browser older than [MinMajor].
type VersionError struct {
	// Product is what the browser called itself (Browser.getVersion's
	// product), and Major its major version.
	Product string
	Major   int
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("cdp: the browser is %s, major %d, and the oldest Chromium the engine drives is major %d: "+
		"run a Chromium of major %d or newer", e.Product, e.Major, MinMajor, MinMajor)
}
