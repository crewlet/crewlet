package cdp

// RequestID is Fetch.RequestId: one paused request, valid until it is
// answered.
type RequestID string

// FetchEnable is Fetch.enable: pause every request that matches one of the
// patterns, before it is sent. First consumer: the render, which answers every
// request of its page and frames in process (internal/artifactrender), on each
// one's own session. A request it pauses arrives with its body
// (Fetch.requestPaused), so a page's POST of about 7 MiB, whose event is past
// ReadLimit, ends the session that listens for it — on the browser's own, the
// connection ([ReadLimit] says why).
var FetchEnable = Method[FetchEnableParams, struct{}]{name: "Fetch.enable"}

// FetchEnableParams is Fetch.enable's parameters.
type FetchEnableParams struct {
	Patterns []RequestPattern `json:"patterns,omitempty"`
}

// RequestPattern is Fetch.RequestPattern.
type RequestPattern struct {
	// URLPattern is a wildcard pattern; empty matches every address.
	URLPattern string `json:"urlPattern,omitempty"`
}

// HeaderEntry is Fetch.HeaderEntry.
type HeaderEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// FetchFailRequest is Fetch.failRequest: refuse a paused request. First
// consumer: the render's serving.
var FetchFailRequest = Method[FetchFailRequestParams, struct{}]{name: "Fetch.failRequest"}

// FetchFailRequestParams is Fetch.failRequest's parameters.
type FetchFailRequestParams struct {
	RequestID RequestID `json:"requestId"`
	// ErrorReason is Network.ErrorReason: "BlockedByClient", "Aborted", …
	ErrorReason string `json:"errorReason"`
}

// FetchFulfillRequest is Fetch.fulfillRequest: answer a paused request
// without sending it. First consumer: the render's serving.
var FetchFulfillRequest = Method[FetchFulfillRequestParams, struct{}]{name: "Fetch.fulfillRequest"}

// FetchFulfillRequestParams is Fetch.fulfillRequest's parameters.
type FetchFulfillRequestParams struct {
	RequestID       RequestID     `json:"requestId"`
	ResponseCode    int64         `json:"responseCode"`
	ResponseHeaders []HeaderEntry `json:"responseHeaders,omitempty"`
	// Body is the response's bytes, which the protocol carries in base64.
	Body []byte `json:"body,omitempty"`
}

// FetchRequestPaused is Fetch.requestPaused: a request waits, before it is
// sent, on its listener's answer. First consumer: the render's serving.
var FetchRequestPaused = Event[FetchRequestPausedEvent]{name: "Fetch.requestPaused"}

// FetchRequestPausedEvent is Fetch.requestPaused's parameters.
type FetchRequestPausedEvent struct {
	RequestID RequestID `json:"requestId"`
	Request   Request   `json:"request"`
	FrameID   FrameID   `json:"frameId"`
	// ResourceType is Network.ResourceType: "Document", "XHR", …
	ResourceType string `json:"resourceType"`
}
