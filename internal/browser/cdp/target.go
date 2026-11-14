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
	Type            string   `json:"type"`
	Title           string   `json:"title"`
	URL             string   `json:"url"`
	Attached        bool     `json:"attached"`
	OpenerID        TargetID `json:"openerId"`
	CanAccessOpener bool     `json:"canAccessOpener"`
	OpenerFrameID   FrameID  `json:"openerFrameId"`
	// ParentFrameID is the frame an out-of-process frame's target sits in,
	// where its own tree is inlined.
	ParentFrameID    FrameID          `json:"parentFrameId"`
	BrowserContextID BrowserContextID `json:"browserContextId"`
	Subtype          string           `json:"subtype"`
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

// TargetSetDiscoverTargets is Target.setDiscoverTargets: announce every target
// the browser makes and loses, which Target.targetCrashed rides on. First
// consumer: the browser session's open sequence (internal/browser).
var TargetSetDiscoverTargets = Method[TargetSetDiscoverTargetsParams, struct{}]{name: "Target.setDiscoverTargets"}

// TargetSetDiscoverTargetsParams is Target.setDiscoverTargets' parameters.
type TargetSetDiscoverTargetsParams struct {
	Discover bool                `json:"discover"`
	Filter   []TargetFilterEntry `json:"filter,omitempty"`
}

// TargetCreateBrowserContext is Target.createBrowserContext: a context of its
// own for a session, its cookies and storage apart from every other's. First
// consumer: the open sequence.
var TargetCreateBrowserContext = Method[TargetCreateBrowserContextParams, TargetCreateBrowserContextResult]{name: "Target.createBrowserContext"}

// TargetCreateBrowserContextParams is Target.createBrowserContext's
// parameters.
type TargetCreateBrowserContextParams struct {
	DisposeOnDetach bool   `json:"disposeOnDetach,omitempty"`
	ProxyServer     string `json:"proxyServer,omitempty"`
	ProxyBypassList string `json:"proxyBypassList,omitempty"`
}

// TargetCreateBrowserContextResult is Target.createBrowserContext's result.
type TargetCreateBrowserContextResult struct {
	BrowserContextID BrowserContextID `json:"browserContextId"`
}

// TargetDisposeBrowserContext is Target.disposeBrowserContext. First consumer:
// the session's close.
var TargetDisposeBrowserContext = Method[TargetDisposeBrowserContextParams, struct{}]{name: "Target.disposeBrowserContext"}

// TargetDisposeBrowserContextParams is Target.disposeBrowserContext's
// parameters.
type TargetDisposeBrowserContextParams struct {
	BrowserContextID BrowserContextID `json:"browserContextId"`
}

// TargetGetBrowserContexts is Target.getBrowserContexts: the contexts a
// browser holds, so a session refuses one that holds another's. First
// consumer: the open sequence.
var TargetGetBrowserContexts = Method[struct{}, TargetGetBrowserContextsResult]{name: "Target.getBrowserContexts"}

// TargetGetBrowserContextsResult is Target.getBrowserContexts' result.
type TargetGetBrowserContextsResult struct {
	BrowserContextIDs []BrowserContextID `json:"browserContextIds"`
}

// TargetCreateTarget is Target.createTarget: a new tab. Under [AutoAttach],
// open it on about:blank and navigate it once its Setup's answers are in,
// opening no window from it before then: a tab opened with an address runs
// before its setup, and a window a tab on about:blank opens can be released
// by the resume of another held tab sharing its renderer (AutoAttach says
// what was measured). First consumer: the open sequence and tabs.
var TargetCreateTarget = Method[TargetCreateTargetParams, TargetCreateTargetResult]{name: "Target.createTarget"}

// TargetCreateTargetParams is Target.createTarget's parameters.
type TargetCreateTargetParams struct {
	URL              string           `json:"url"`
	BrowserContextID BrowserContextID `json:"browserContextId,omitempty"`
	NewWindow        bool             `json:"newWindow,omitempty"`
	Background       bool             `json:"background,omitempty"`
}

// TargetCreateTargetResult is Target.createTarget's result.
type TargetCreateTargetResult struct {
	TargetID TargetID `json:"targetId"`
}

// TargetCloseTarget is Target.closeTarget. Its result's success field is
// deprecated, and not read: a refusal is the error. First consumer: tabs.
var TargetCloseTarget = Method[TargetCloseTargetParams, struct{}]{name: "Target.closeTarget"}

// TargetCloseTargetParams is Target.closeTarget's parameters.
type TargetCloseTargetParams struct {
	TargetID TargetID `json:"targetId"`
}

// TargetGetTargets is Target.getTargets. First consumer: tabs.
var TargetGetTargets = Method[TargetGetTargetsParams, TargetGetTargetsResult]{name: "Target.getTargets"}

// TargetGetTargetsParams is Target.getTargets' parameters.
type TargetGetTargetsParams struct {
	Filter []TargetFilterEntry `json:"filter,omitempty"`
}

// TargetGetTargetsResult is Target.getTargets' result.
type TargetGetTargetsResult struct {
	TargetInfos []TargetInfo `json:"targetInfos"`
}

// TargetAttachToTarget is Target.attachToTarget: a session on one target,
// flat when Flatten is set, announced by Target.attachedToTarget before the
// answer. First consumer: the open sequence.
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

// TargetActivateTarget is Target.activateTarget: bring a tab to the front.
// First consumer: tabs.
var TargetActivateTarget = Method[TargetActivateTargetParams, struct{}]{name: "Target.activateTarget"}

// TargetActivateTargetParams is Target.activateTarget's parameters.
type TargetActivateTargetParams struct {
	TargetID TargetID `json:"targetId"`
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

// TargetTargetCrashed is Target.targetCrashed: a target's renderer crashed,
// which ends every session attached to it with a [*CrashedError]. Delivered
// at the browser level once discovery is on. First consumer: this package's
// session ends.
var TargetTargetCrashed = Event[TargetTargetCrashedEvent]{name: "Target.targetCrashed"}

// TargetTargetCrashedEvent is Target.targetCrashed's parameters.
type TargetTargetCrashedEvent struct {
	TargetID  TargetID `json:"targetId"`
	Status    string   `json:"status"`
	ErrorCode int64    `json:"errorCode"`
}

// TargetTargetCreated is Target.targetCreated. First consumer: tabs.
var TargetTargetCreated = Event[TargetTargetCreatedEvent]{name: "Target.targetCreated"}

// TargetTargetCreatedEvent is Target.targetCreated's parameters.
type TargetTargetCreatedEvent struct {
	TargetInfo TargetInfo `json:"targetInfo"`
}

// TargetTargetDestroyed is Target.targetDestroyed. First consumer: tabs.
var TargetTargetDestroyed = Event[TargetTargetDestroyedEvent]{name: "Target.targetDestroyed"}

// TargetTargetDestroyedEvent is Target.targetDestroyed's parameters.
type TargetTargetDestroyedEvent struct {
	TargetID TargetID `json:"targetId"`
}

// TargetTargetInfoChanged is Target.targetInfoChanged: a target's title or
// address moved. First consumer: tabs.
var TargetTargetInfoChanged = Event[TargetTargetInfoChangedEvent]{name: "Target.targetInfoChanged"}

// TargetTargetInfoChangedEvent is Target.targetInfoChanged's parameters.
type TargetTargetInfoChangedEvent struct {
	TargetInfo TargetInfo `json:"targetInfo"`
}
