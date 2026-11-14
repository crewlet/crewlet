package cdp

// FrameID is Page.FrameId: one frame of a page. An out-of-process frame's
// target id is its frame id.
type FrameID string

// LoaderID is Network.LoaderId: one navigation of a frame.
type LoaderID string

// ScriptIdentifier is Page.ScriptIdentifier: a script added to every new
// document of a target.
type ScriptIdentifier string

// PageEnable is Page.enable: report the page's frames, navigations, dialogs
// and lifecycle. First consumer: navigation (internal/browser).
var PageEnable = Method[struct{}, struct{}]{name: "Page.enable"}

// PageNavigate is Page.navigate. First consumer: navigation.
var PageNavigate = Method[PageNavigateParams, PageNavigateResult]{name: "Page.navigate"}

// PageNavigateParams is Page.navigate's parameters.
type PageNavigateParams struct {
	URL string `json:"url"`
	// TransitionType is how the navigation is recorded: "typed" for an
	// address the seat asked for.
	TransitionType string `json:"transitionType,omitempty"`
}

// PageNavigateResult is Page.navigate's result.
type PageNavigateResult struct {
	FrameID  FrameID  `json:"frameId"`
	LoaderID LoaderID `json:"loaderId"`
	// ErrorText is why the navigation failed, where it did ("net::…").
	ErrorText string `json:"errorText"`
	// IsDownload is a navigation that became a download.
	IsDownload bool `json:"isDownload"`
}

// PageGetNavigationHistory is Page.getNavigationHistory. First consumer:
// history.
var PageGetNavigationHistory = Method[struct{}, PageGetNavigationHistoryResult]{name: "Page.getNavigationHistory"}

// PageGetNavigationHistoryResult is Page.getNavigationHistory's result.
type PageGetNavigationHistoryResult struct {
	CurrentIndex int64             `json:"currentIndex"`
	Entries      []NavigationEntry `json:"entries"`
}

// NavigationEntry is Page.NavigationEntry: one step of a tab's history.
type NavigationEntry struct {
	ID    int64  `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

// PageNavigateToHistoryEntry is Page.navigateToHistoryEntry. First consumer:
// history.
var PageNavigateToHistoryEntry = Method[PageNavigateToHistoryEntryParams, struct{}]{name: "Page.navigateToHistoryEntry"}

// PageNavigateToHistoryEntryParams is Page.navigateToHistoryEntry's
// parameters.
type PageNavigateToHistoryEntryParams struct {
	EntryID int64 `json:"entryId"`
}

// PageGetFrameTree is Page.getFrameTree. First consumer: snapshots.
var PageGetFrameTree = Method[struct{}, PageGetFrameTreeResult]{name: "Page.getFrameTree"}

// PageGetFrameTreeResult is Page.getFrameTree's result.
type PageGetFrameTreeResult struct {
	FrameTree FrameTree `json:"frameTree"`
}

// FrameTree is Page.FrameTree: a frame and its children in this process.
type FrameTree struct {
	Frame       Frame       `json:"frame"`
	ChildFrames []FrameTree `json:"childFrames"`
}

// Frame is Page.Frame.
type Frame struct {
	ID             FrameID  `json:"id"`
	ParentID       FrameID  `json:"parentId"`
	LoaderID       LoaderID `json:"loaderId"`
	Name           string   `json:"name"`
	URL            string   `json:"url"`
	SecurityOrigin string   `json:"securityOrigin"`
	MimeType       string   `json:"mimeType"`
}

// PageCreateIsolatedWorld is Page.createIsolatedWorld: a JavaScript world of
// the engine's own in one frame, where no page script reaches its globals.
// First consumer: perception.
var PageCreateIsolatedWorld = Method[PageCreateIsolatedWorldParams, PageCreateIsolatedWorldResult]{name: "Page.createIsolatedWorld"}

// PageCreateIsolatedWorldParams is Page.createIsolatedWorld's parameters.
type PageCreateIsolatedWorldParams struct {
	FrameID   FrameID `json:"frameId"`
	WorldName string  `json:"worldName,omitempty"`
}

// PageCreateIsolatedWorldResult is Page.createIsolatedWorld's result.
type PageCreateIsolatedWorldResult struct {
	ExecutionContextID ExecutionContextID `json:"executionContextId"`
}

// PageAddScriptToEvaluateOnNewDocument is
// Page.addScriptToEvaluateOnNewDocument: a script that runs in every new
// document before the page's own. First consumer: the narrowing script
// (internal/browser).
var PageAddScriptToEvaluateOnNewDocument = Method[PageAddScriptToEvaluateOnNewDocumentParams, PageAddScriptToEvaluateOnNewDocumentResult]{name: "Page.addScriptToEvaluateOnNewDocument"}

// PageAddScriptToEvaluateOnNewDocumentParams is
// Page.addScriptToEvaluateOnNewDocument's parameters.
type PageAddScriptToEvaluateOnNewDocumentParams struct {
	Source string `json:"source"`
	// WorldName runs it in that isolated world rather than the page's
	// own.
	WorldName string `json:"worldName,omitempty"`
	// RunImmediately runs it in the documents already loaded too.
	RunImmediately bool `json:"runImmediately,omitempty"`
}

// PageAddScriptToEvaluateOnNewDocumentResult is
// Page.addScriptToEvaluateOnNewDocument's result.
type PageAddScriptToEvaluateOnNewDocumentResult struct {
	Identifier ScriptIdentifier `json:"identifier"`
}

// PageSetLifecycleEventsEnabled is Page.setLifecycleEventsEnabled. First
// consumer: waiting on a page.
var PageSetLifecycleEventsEnabled = Method[PageSetLifecycleEventsEnabledParams, struct{}]{name: "Page.setLifecycleEventsEnabled"}

// PageSetLifecycleEventsEnabledParams is Page.setLifecycleEventsEnabled's
// parameters.
type PageSetLifecycleEventsEnabledParams struct {
	Enabled bool `json:"enabled"`
}

// PageCaptureScreenshot is Page.captureScreenshot. First consumer:
// screenshots, whose size the caller bounds at its source.
var PageCaptureScreenshot = Method[PageCaptureScreenshotParams, PageCaptureScreenshotResult]{name: "Page.captureScreenshot"}

// PageCaptureScreenshotParams is Page.captureScreenshot's parameters.
type PageCaptureScreenshotParams struct {
	// Format is "png", "jpeg" or "webp".
	Format string `json:"format,omitempty"`
	// Quality is a jpeg's or webp's, 0 to 100.
	Quality               int64     `json:"quality,omitempty"`
	Clip                  *Viewport `json:"clip,omitempty"`
	CaptureBeyondViewport bool      `json:"captureBeyondViewport,omitempty"`
}

// PageCaptureScreenshotResult is Page.captureScreenshot's result.
type PageCaptureScreenshotResult struct {
	// Data is the image's bytes, which the protocol carries in base64.
	Data []byte `json:"data"`
}

// Viewport is Page.Viewport: a rectangle of the page, in CSS pixels, and the
// scale to capture it at.
type Viewport struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
	Scale  float64 `json:"scale"`
}

// PageGetLayoutMetrics is Page.getLayoutMetrics, read in CSS pixels: its
// device-pixel fields are deprecated. First consumer: screenshots and hit
// tests.
var PageGetLayoutMetrics = Method[struct{}, PageGetLayoutMetricsResult]{name: "Page.getLayoutMetrics"}

// PageGetLayoutMetricsResult is Page.getLayoutMetrics' result.
type PageGetLayoutMetricsResult struct {
	CSSLayoutViewport LayoutViewport `json:"cssLayoutViewport"`
	CSSVisualViewport VisualViewport `json:"cssVisualViewport"`
	CSSContentSize    Rect           `json:"cssContentSize"`
}

// LayoutViewport is Page.LayoutViewport.
type LayoutViewport struct {
	PageX        int64 `json:"pageX"`
	PageY        int64 `json:"pageY"`
	ClientWidth  int64 `json:"clientWidth"`
	ClientHeight int64 `json:"clientHeight"`
}

// VisualViewport is Page.VisualViewport.
type VisualViewport struct {
	OffsetX      float64 `json:"offsetX"`
	OffsetY      float64 `json:"offsetY"`
	PageX        float64 `json:"pageX"`
	PageY        float64 `json:"pageY"`
	ClientWidth  float64 `json:"clientWidth"`
	ClientHeight float64 `json:"clientHeight"`
	Scale        float64 `json:"scale"`
	Zoom         float64 `json:"zoom"`
}

// PageStartScreencast is Page.startScreencast: frames of the page as it
// paints, each to be acknowledged. First consumer: the live view.
var PageStartScreencast = Method[PageStartScreencastParams, struct{}]{name: "Page.startScreencast"}

// PageStartScreencastParams is Page.startScreencast's parameters.
type PageStartScreencastParams struct {
	// Format is "jpeg" or "png".
	Format        string `json:"format,omitempty"`
	Quality       int64  `json:"quality,omitempty"`
	MaxWidth      int64  `json:"maxWidth,omitempty"`
	MaxHeight     int64  `json:"maxHeight,omitempty"`
	EveryNthFrame int64  `json:"everyNthFrame,omitempty"`
}

// PageStopScreencast is Page.stopScreencast. First consumer: the live view.
var PageStopScreencast = Method[struct{}, struct{}]{name: "Page.stopScreencast"}

// PageScreencastFrameAck is Page.screencastFrameAck: the frame was taken, and
// the next may come. First consumer: the live view.
var PageScreencastFrameAck = Method[PageScreencastFrameAckParams, struct{}]{name: "Page.screencastFrameAck"}

// PageScreencastFrameAckParams is Page.screencastFrameAck's parameters.
type PageScreencastFrameAckParams struct {
	// SessionID is the frame's number, Page.screencastFrame's sessionId:
	// no session of this package's.
	SessionID int64 `json:"sessionId"`
}

// PageHandleJavaScriptDialog is Page.handleJavaScriptDialog. First consumer:
// dialogs.
var PageHandleJavaScriptDialog = Method[PageHandleJavaScriptDialogParams, struct{}]{name: "Page.handleJavaScriptDialog"}

// PageHandleJavaScriptDialogParams is Page.handleJavaScriptDialog's
// parameters.
type PageHandleJavaScriptDialogParams struct {
	Accept     bool   `json:"accept"`
	PromptText string `json:"promptText,omitempty"`
}

// PageSetInterceptFileChooserDialog is Page.setInterceptFileChooserDialog: a
// file chooser is announced (Page.fileChooserOpened) rather than shown. First
// consumer: uploads.
var PageSetInterceptFileChooserDialog = Method[PageSetInterceptFileChooserDialogParams, struct{}]{name: "Page.setInterceptFileChooserDialog"}

// PageSetInterceptFileChooserDialogParams is
// Page.setInterceptFileChooserDialog's parameters.
type PageSetInterceptFileChooserDialogParams struct {
	Enabled bool `json:"enabled"`
}

// PageLifecycleEvent is Page.lifecycleEvent: "init", "DOMContentLoaded",
// "load", "networkIdle" and the rest, per frame and navigation. First
// consumer: waiting on a page.
var PageLifecycleEvent = Event[PageLifecycleEventEvent]{name: "Page.lifecycleEvent"}

// PageLifecycleEventEvent is Page.lifecycleEvent's parameters.
type PageLifecycleEventEvent struct {
	FrameID  FrameID  `json:"frameId"`
	LoaderID LoaderID `json:"loaderId"`
	Name     string   `json:"name"`
	// Timestamp is Chromium's monotonic clock, in seconds.
	Timestamp float64 `json:"timestamp"`
}

// PageScreencastFrame is Page.screencastFrame. First consumer: the live view.
var PageScreencastFrame = Event[PageScreencastFrameEvent]{name: "Page.screencastFrame"}

// PageScreencastFrameEvent is Page.screencastFrame's parameters.
type PageScreencastFrameEvent struct {
	// Data is the frame's image, which the protocol carries in base64.
	Data     []byte                  `json:"data"`
	Metadata ScreencastFrameMetadata `json:"metadata"`
	// SessionID is the frame's number, acknowledged with
	// Page.screencastFrameAck.
	SessionID int64 `json:"sessionId"`
}

// ScreencastFrameMetadata is Page.ScreencastFrameMetadata.
type ScreencastFrameMetadata struct {
	OffsetTop       float64 `json:"offsetTop"`
	PageScaleFactor float64 `json:"pageScaleFactor"`
	DeviceWidth     float64 `json:"deviceWidth"`
	DeviceHeight    float64 `json:"deviceHeight"`
	ScrollOffsetX   float64 `json:"scrollOffsetX"`
	ScrollOffsetY   float64 `json:"scrollOffsetY"`
	// Timestamp is the frame's, in seconds since the epoch.
	Timestamp float64 `json:"timestamp"`
}

// PageJavascriptDialogOpening is Page.javascriptDialogOpening: an alert, a
// confirm, a prompt or a beforeunload waits on an answer. First consumer:
// dialogs.
var PageJavascriptDialogOpening = Event[PageJavascriptDialogOpeningEvent]{name: "Page.javascriptDialogOpening"}

// PageJavascriptDialogOpeningEvent is Page.javascriptDialogOpening's
// parameters.
type PageJavascriptDialogOpeningEvent struct {
	URL     string  `json:"url"`
	FrameID FrameID `json:"frameId"`
	Message string  `json:"message"`
	// Type is "alert", "confirm", "prompt" or "beforeunload".
	Type              string `json:"type"`
	HasBrowserHandler bool   `json:"hasBrowserHandler"`
	DefaultPrompt     string `json:"defaultPrompt"`
}

// PageJavascriptDialogClosed is Page.javascriptDialogClosed. First consumer:
// dialogs.
var PageJavascriptDialogClosed = Event[PageJavascriptDialogClosedEvent]{name: "Page.javascriptDialogClosed"}

// PageJavascriptDialogClosedEvent is Page.javascriptDialogClosed's
// parameters.
type PageJavascriptDialogClosedEvent struct {
	FrameID   FrameID `json:"frameId"`
	Result    bool    `json:"result"`
	UserInput string  `json:"userInput"`
}

// PageFileChooserOpened is Page.fileChooserOpened: a page opened a file
// chooser while it is intercepted. First consumer: uploads.
var PageFileChooserOpened = Event[PageFileChooserOpenedEvent]{name: "Page.fileChooserOpened"}

// PageFileChooserOpenedEvent is Page.fileChooserOpened's parameters.
type PageFileChooserOpenedEvent struct {
	FrameID FrameID `json:"frameId"`
	// Mode is "selectSingle" or "selectMultiple".
	Mode string `json:"mode"`
	// BackendNodeID is the input that opened it.
	BackendNodeID BackendNodeID `json:"backendNodeId"`
}

// PageFrameNavigated is Page.frameNavigated: a frame committed a navigation.
// First consumer: navigation.
var PageFrameNavigated = Event[PageFrameNavigatedEvent]{name: "Page.frameNavigated"}

// PageFrameNavigatedEvent is Page.frameNavigated's parameters.
type PageFrameNavigatedEvent struct {
	Frame Frame `json:"frame"`
	// Type is "Navigation" or "BackForwardCacheRestore".
	Type string `json:"type"`
}

// PageNavigatedWithinDocument is Page.navigatedWithinDocument: a fragment or
// history API navigation. First consumer: navigation.
var PageNavigatedWithinDocument = Event[PageNavigatedWithinDocumentEvent]{name: "Page.navigatedWithinDocument"}

// PageNavigatedWithinDocumentEvent is Page.navigatedWithinDocument's
// parameters.
type PageNavigatedWithinDocumentEvent struct {
	FrameID FrameID `json:"frameId"`
	URL     string  `json:"url"`
	// NavigationType is "fragment", "historyApi" or "other".
	NavigationType string `json:"navigationType"`
}

// PageFrameAttached is Page.frameAttached. First consumer: snapshots.
var PageFrameAttached = Event[PageFrameAttachedEvent]{name: "Page.frameAttached"}

// PageFrameAttachedEvent is Page.frameAttached's parameters.
type PageFrameAttachedEvent struct {
	FrameID       FrameID `json:"frameId"`
	ParentFrameID FrameID `json:"parentFrameId"`
}

// PageFrameDetached is Page.frameDetached. First consumer: snapshots.
var PageFrameDetached = Event[PageFrameDetachedEvent]{name: "Page.frameDetached"}

// PageFrameDetachedEvent is Page.frameDetached's parameters.
type PageFrameDetachedEvent struct {
	FrameID FrameID `json:"frameId"`
	// Reason is "remove", or "swap" for a frame that moved to another
	// process and lives on there.
	Reason string `json:"reason"`
}
