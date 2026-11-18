package cdp

// SessionID is Target.SessionID: a session attached to a target, flat on its
// connection.
type SessionID string

// TargetID is Target.TargetID: a page, a frame, a worker or the browser.
type TargetID string

// TargetInfo is Target.TargetInfo: what Chromium says of a target.
type TargetInfo struct {
	TargetID TargetID `json:"targetId"`
	// Type is "page", "iframe", "worker", "shared_worker",
	// "service_worker", "browser", "tab" or another Chromium names.
	Type string `json:"type"`
	// URL and OpenerID are what the attacher's cases tell the targets they
	// started apart from the browser's own by.
	URL      string   `json:"url"`
	OpenerID TargetID `json:"openerId"`
}

// TargetFilterEntry is one entry of Target.TargetFilter: a target type to
// take or, with Exclude, to leave. A filter matches a target by its first
// entry that names its type, or one with no type.
type TargetFilterEntry struct {
	Exclude bool   `json:"exclude,omitempty"`
	Type    string `json:"type,omitempty"`
}

// targetSetAutoAttach is Target.setAutoAttach: attach every new target of a
// session, flat, holding each until it is resumed. This package's AutoAttach
// alone sends it, and it is unexported so that nothing else can: sent again
// on a session AutoAttach watches, it would replace the attacher's own
// settings, its hold among them.
var targetSetAutoAttach = Method[targetSetAutoAttachParams, struct{}]{name: "Target.setAutoAttach"}

// targetSetAutoAttachParams is Target.setAutoAttach's parameters.
type targetSetAutoAttachParams struct {
	AutoAttach             bool                `json:"autoAttach"`
	WaitForDebuggerOnStart bool                `json:"waitForDebuggerOnStart"`
	Flatten                bool                `json:"flatten,omitempty"`
	Filter                 []TargetFilterEntry `json:"filter,omitempty"`
}

// TargetCreateBrowserContext is Target.createBrowserContext: a context of its
// own for a session, its cookies and storage apart from every other's. First
// consumer: the render's tab (internal/artifactrender).
var TargetCreateBrowserContext = Method[TargetCreateBrowserContextParams, TargetCreateBrowserContextResult]{name: "Target.createBrowserContext"}

// TargetCreateBrowserContextParams is Target.createBrowserContext's
// parameters.
type TargetCreateBrowserContextParams struct {
	DisposeOnDetach bool `json:"disposeOnDetach,omitempty"`
}

// TargetCreateBrowserContextResult is Target.createBrowserContext's result.
type TargetCreateBrowserContextResult struct {
	BrowserContextID BrowserContextID `json:"browserContextId"`
}

// TargetDisposeBrowserContext is Target.disposeBrowserContext, which closes
// every target in the context. First consumer: the render's close.
var TargetDisposeBrowserContext = Method[TargetDisposeBrowserContextParams, struct{}]{name: "Target.disposeBrowserContext"}

// TargetDisposeBrowserContextParams is Target.disposeBrowserContext's
// parameters.
type TargetDisposeBrowserContextParams struct {
	BrowserContextID BrowserContextID `json:"browserContextId"`
}

// TargetCreateTarget is Target.createTarget: a new tab. Under [AutoAttach],
// open it on about:blank and navigate it once its Setup's answers are in,
// opening no window from it before then: a tab opened with an address runs
// before its setup, and a window a tab on about:blank opens can be released
// by the resume of another held tab sharing its renderer (the package doc
// says what was measured). First consumer: the render's tab.
var TargetCreateTarget = Method[TargetCreateTargetParams, TargetCreateTargetResult]{name: "Target.createTarget"}

// TargetCreateTargetParams is Target.createTarget's parameters.
type TargetCreateTargetParams struct {
	URL              string           `json:"url"`
	BrowserContextID BrowserContextID `json:"browserContextId,omitempty"`
}

// TargetCreateTargetResult is Target.createTarget's result.
type TargetCreateTargetResult struct {
	TargetID TargetID `json:"targetId"`
}

// TargetAttachToTarget is Target.attachToTarget: a session on one target,
// flat when Flatten is set, announced by Target.attachedToTarget before the
// answer. First consumer: the render's tab.
var TargetAttachToTarget = Method[TargetAttachToTargetParams, TargetAttachToTargetResult]{name: "Target.attachToTarget"}

// TargetAttachToTargetParams is Target.attachToTarget's parameters.
type TargetAttachToTargetParams struct {
	TargetID TargetID `json:"targetId"`
	Flatten  bool     `json:"flatten,omitempty"`
}

// TargetAttachToTargetResult is Target.attachToTarget's result.
type TargetAttachToTargetResult struct {
	SessionID SessionID `json:"sessionId"`
}

// TargetAttachedToTarget is Target.attachedToTarget: a target attached to a
// session, its own session flat on the connection. The connection registers
// the child before it delivers this. First consumer: this package's
// AutoAttach.
var TargetAttachedToTarget = Event[TargetAttachedToTargetEvent]{name: "Target.attachedToTarget"}

// TargetAttachedToTargetEvent is Target.attachedToTarget's parameters.
type TargetAttachedToTargetEvent struct {
	SessionID          SessionID  `json:"sessionId"`
	TargetInfo         TargetInfo `json:"targetInfo"`
	WaitingForDebugger bool       `json:"waitingForDebugger"`
}

// TargetDetachedFromTarget is Target.detachedFromTarget: a child session
// detached, which ends it with a [*DetachedError]. First consumer: this
// package's session ends.
var TargetDetachedFromTarget = Event[TargetDetachedFromTargetEvent]{name: "Target.detachedFromTarget"}

// TargetDetachedFromTargetEvent is Target.detachedFromTarget's parameters.
type TargetDetachedFromTargetEvent struct {
	SessionID SessionID `json:"sessionId"`
}
