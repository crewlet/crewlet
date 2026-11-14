package cdp

// BrowserContextID is Browser.BrowserContextID: one isolated context of a
// browser, with cookies and storage of its own.
type BrowserContextID string

// BrowserGetVersion is Browser.getVersion: what the browser is, which the
// floor is read from. First consumer: this package's CheckVersion.
var BrowserGetVersion = Method[struct{}, BrowserGetVersionResult]{name: "Browser.getVersion"}

// BrowserGetVersionResult is Browser.getVersion's result.
type BrowserGetVersionResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	// Product is "HeadlessChrome/158.0.8127.18", "Chrome/…" or
	// "Chromium/…".
	Product   string `json:"product"`
	Revision  string `json:"revision"`
	UserAgent string `json:"userAgent"`
	JSVersion string `json:"jsVersion"`
}

// BrowserSetDownloadBehavior is Browser.setDownloadBehavior: where a
// context's downloads land, and whether their events are sent. First
// consumer: held files (internal/browser).
var BrowserSetDownloadBehavior = Method[BrowserSetDownloadBehaviorParams, struct{}]{name: "Browser.setDownloadBehavior"}

// BrowserSetDownloadBehaviorParams is Browser.setDownloadBehavior's
// parameters.
type BrowserSetDownloadBehaviorParams struct {
	// Behavior is "deny", "allow", "allowAndName" or "default".
	Behavior         string           `json:"behavior"`
	BrowserContextID BrowserContextID `json:"browserContextId,omitempty"`
	DownloadPath     string           `json:"downloadPath,omitempty"`
	EventsEnabled    bool             `json:"eventsEnabled,omitempty"`
}

// BrowserDownloadWillBegin is Browser.downloadWillBegin. First consumer: held
// files.
var BrowserDownloadWillBegin = Event[BrowserDownloadWillBeginEvent]{name: "Browser.downloadWillBegin"}

// BrowserDownloadWillBeginEvent is Browser.downloadWillBegin's parameters.
type BrowserDownloadWillBeginEvent struct {
	FrameID           FrameID `json:"frameId"`
	GUID              string  `json:"guid"`
	URL               string  `json:"url"`
	SuggestedFilename string  `json:"suggestedFilename"`
}

// BrowserDownloadProgress is Browser.downloadProgress. First consumer: held
// files.
var BrowserDownloadProgress = Event[BrowserDownloadProgressEvent]{name: "Browser.downloadProgress"}

// BrowserDownloadProgressEvent is Browser.downloadProgress' parameters.
type BrowserDownloadProgressEvent struct {
	GUID          string  `json:"guid"`
	TotalBytes    float64 `json:"totalBytes"`
	ReceivedBytes float64 `json:"receivedBytes"`
	// State is "inProgress", "completed" or "canceled".
	State string `json:"state"`
	// FilePath is where a completed download landed, under the behavior
	// "allowAndName".
	FilePath string `json:"filePath"`
}
