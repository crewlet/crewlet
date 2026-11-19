package cdp

// FrameID is Page.FrameId: one frame of a page. An out-of-process frame's
// target id is its frame id.
type FrameID string

// PageEnable is Page.enable: report the page's frames, navigations, dialogs
// and lifecycle. First consumer: the render's loads (internal/artifactrender).
var PageEnable = Method[struct{}, struct{}]{name: "Page.enable"}

// PageNavigate is Page.navigate. First consumer: the render's loads.
var PageNavigate = Method[PageNavigateParams, PageNavigateResult]{name: "Page.navigate"}

// PageNavigateParams is Page.navigate's parameters.
type PageNavigateParams struct {
	URL string `json:"url"`
}

// PageNavigateResult is Page.navigate's result.
type PageNavigateResult struct {
	// ErrorText is why the navigation failed, where it did ("net::…").
	ErrorText string `json:"errorText"`
}

// PageGetFrameTree is Page.getFrameTree. First consumer: the render's frame
// lookup.
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
	ID  FrameID `json:"id"`
	URL string  `json:"url"`
}

// PageCreateIsolatedWorld is Page.createIsolatedWorld: a JavaScript world of
// the engine's own in one frame, where no page script reaches its globals.
// First consumer: the render's checks.
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
// document before the page's own. First consumer: the render's host script.
var PageAddScriptToEvaluateOnNewDocument = Method[PageAddScriptToEvaluateOnNewDocumentParams, struct{}]{name: "Page.addScriptToEvaluateOnNewDocument"}

// PageAddScriptToEvaluateOnNewDocumentParams is
// Page.addScriptToEvaluateOnNewDocument's parameters.
type PageAddScriptToEvaluateOnNewDocumentParams struct {
	Source string `json:"source"`
	// WorldName runs it in that isolated world rather than the page's
	// own.
	WorldName string `json:"worldName,omitempty"`
}

// PageCaptureScreenshot is Page.captureScreenshot. First consumer: the
// render's shots, whose size it bounds at its viewport ([ReadLimit]).
var PageCaptureScreenshot = Method[PageCaptureScreenshotParams, PageCaptureScreenshotResult]{name: "Page.captureScreenshot"}

// PageCaptureScreenshotParams is Page.captureScreenshot's parameters.
type PageCaptureScreenshotParams struct {
	// Format is "png", "jpeg" or "webp".
	Format string `json:"format,omitempty"`
}

// PageCaptureScreenshotResult is Page.captureScreenshot's result.
type PageCaptureScreenshotResult struct {
	// Data is the image's bytes, which the protocol carries in base64.
	Data []byte `json:"data"`
}
