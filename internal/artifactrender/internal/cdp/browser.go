package cdp

// BrowserContextID is Browser.BrowserContextID: one isolated context of a
// browser, with cookies and storage of its own.
type BrowserContextID string

// BrowserGetVersion is Browser.getVersion: what the browser is, which the
// floor is read from. First consumer: this package's CheckVersion.
var BrowserGetVersion = Method[struct{}, BrowserGetVersionResult]{name: "Browser.getVersion"}

// BrowserGetVersionResult is Browser.getVersion's result.
type BrowserGetVersionResult struct {
	// Product is "HeadlessChrome/158.0.8127.18", "Chrome/…" or
	// "Chromium/…".
	Product string `json:"product"`
}
