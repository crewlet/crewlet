package cdp

// RequestID is Fetch.RequestId: one paused request, valid until it is
// continued, failed or fulfilled. Network's own id for the same request is
// another space ([NetworkRequestID]), which Fetch.requestPaused names too.
type RequestID string

// StreamHandle is IO.StreamHandle: a body read in chunks through IO.read.
type StreamHandle string

// FetchEnable is Fetch.enable: pause every request that matches one of the
// patterns, at its request or its response stage. First consumer: the request
// gate (internal/browser), on the browser session, before any page of its own
// opens. A request it pauses arrives with its body (Fetch.requestPaused), so a
// page's POST past ReadLimit ends the session the gate listens on — on the
// browser's, the connection ([ReadLimit] says why).
var FetchEnable = Method[FetchEnableParams, struct{}]{name: "Fetch.enable"}

// FetchEnableParams is Fetch.enable's parameters.
type FetchEnableParams struct {
	Patterns []RequestPattern `json:"patterns,omitempty"`
	// HandleAuthRequests pauses an authentication challenge as
	// Fetch.authRequired, answered with Fetch.continueWithAuth.
	HandleAuthRequests bool `json:"handleAuthRequests,omitempty"`
}

// RequestPattern is Fetch.RequestPattern.
type RequestPattern struct {
	// URLPattern is a wildcard pattern; empty matches every address.
	URLPattern string `json:"urlPattern,omitempty"`
	// ResourceType is Network.ResourceType: "Document", "Script", …
	ResourceType string `json:"resourceType,omitempty"`
	// RequestStage is "Request" (the default) or "Response".
	RequestStage string `json:"requestStage,omitempty"`
}

// HeaderEntry is Fetch.HeaderEntry.
type HeaderEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// FetchContinueRequest is Fetch.continueRequest: let a paused request go, as
// it is. First consumer: the request gate.
var FetchContinueRequest = Method[FetchContinueRequestParams, struct{}]{name: "Fetch.continueRequest"}

// FetchContinueRequestParams is Fetch.continueRequest's parameters.
type FetchContinueRequestParams struct {
	RequestID RequestID `json:"requestId"`
	// InterceptResponse pauses the same request again at its response.
	InterceptResponse bool `json:"interceptResponse,omitempty"`
}

// FetchFailRequest is Fetch.failRequest: refuse a paused request. First
// consumer: the request gate.
var FetchFailRequest = Method[FetchFailRequestParams, struct{}]{name: "Fetch.failRequest"}

// FetchFailRequestParams is Fetch.failRequest's parameters.
type FetchFailRequestParams struct {
	RequestID RequestID `json:"requestId"`
	// ErrorReason is Network.ErrorReason: "BlockedByClient", "Aborted", …
	ErrorReason string `json:"errorReason"`
}

// FetchFulfillRequest is Fetch.fulfillRequest: answer a paused request
// without sending it. First consumer: the request gate.
var FetchFulfillRequest = Method[FetchFulfillRequestParams, struct{}]{name: "Fetch.fulfillRequest"}

// FetchFulfillRequestParams is Fetch.fulfillRequest's parameters.
type FetchFulfillRequestParams struct {
	RequestID       RequestID     `json:"requestId"`
	ResponseCode    int64         `json:"responseCode"`
	ResponseHeaders []HeaderEntry `json:"responseHeaders,omitempty"`
	// Body is the response's bytes, which the protocol carries in base64.
	Body           []byte `json:"body,omitempty"`
	ResponsePhrase string `json:"responsePhrase,omitempty"`
}

// FetchContinueResponse is Fetch.continueResponse: let a response paused at
// its response stage go on, its headers replaced where ResponseHeaders is
// set. First consumer: the content security policy (internal/browser).
var FetchContinueResponse = Method[FetchContinueResponseParams, struct{}]{name: "Fetch.continueResponse"}

// FetchContinueResponseParams is Fetch.continueResponse's parameters.
type FetchContinueResponseParams struct {
	RequestID       RequestID     `json:"requestId"`
	ResponseCode    int64         `json:"responseCode,omitempty"`
	ResponsePhrase  string        `json:"responsePhrase,omitempty"`
	ResponseHeaders []HeaderEntry `json:"responseHeaders,omitempty"`
}

// FetchContinueWithAuth is Fetch.continueWithAuth: answer an authentication
// challenge. Its password is a credential, never logged or reported. First
// consumer: HTTP authentication (internal/browser).
var FetchContinueWithAuth = Method[FetchContinueWithAuthParams, struct{}]{name: "Fetch.continueWithAuth"}

// FetchContinueWithAuthParams is Fetch.continueWithAuth's parameters.
type FetchContinueWithAuthParams struct {
	RequestID             RequestID             `json:"requestId"`
	AuthChallengeResponse AuthChallengeResponse `json:"authChallengeResponse"`
}

// AuthChallengeResponse is Fetch.AuthChallengeResponse.
type AuthChallengeResponse struct {
	// Response is "Default", "CancelAuth" or "ProvideCredentials".
	Response string `json:"response"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// FetchTakeResponseBodyAsStream is Fetch.takeResponseBodyAsStream: a response
// paused at its response stage, its body handed over to read in chunks. First
// consumer: downloads on a remote backend.
var FetchTakeResponseBodyAsStream = Method[FetchTakeResponseBodyAsStreamParams, FetchTakeResponseBodyAsStreamResult]{name: "Fetch.takeResponseBodyAsStream"}

// FetchTakeResponseBodyAsStreamParams is Fetch.takeResponseBodyAsStream's
// parameters.
type FetchTakeResponseBodyAsStreamParams struct {
	RequestID RequestID `json:"requestId"`
}

// FetchTakeResponseBodyAsStreamResult is Fetch.takeResponseBodyAsStream's
// result.
type FetchTakeResponseBodyAsStreamResult struct {
	Stream StreamHandle `json:"stream"`
}

// FetchRequestPaused is Fetch.requestPaused: a request waits on the gate's
// answer, at its request stage or, where ResponseStatusCode or
// ResponseErrorReason is set, at its response. First consumer: the request
// gate.
var FetchRequestPaused = Event[FetchRequestPausedEvent]{name: "Fetch.requestPaused"}

// FetchRequestPausedEvent is Fetch.requestPaused's parameters.
type FetchRequestPausedEvent struct {
	RequestID RequestID `json:"requestId"`
	Request   Request   `json:"request"`
	FrameID   FrameID   `json:"frameId"`
	// ResourceType is Network.ResourceType: "Document", "XHR", …
	ResourceType        string        `json:"resourceType"`
	ResponseErrorReason string        `json:"responseErrorReason"`
	ResponseStatusCode  int64         `json:"responseStatusCode"`
	ResponseStatusText  string        `json:"responseStatusText"`
	ResponseHeaders     []HeaderEntry `json:"responseHeaders"`
	// NetworkID is Network's id for the same request.
	NetworkID NetworkRequestID `json:"networkId"`
	// RedirectedRequestID is the request a redirect paused at its response
	// stage continues as.
	RedirectedRequestID RequestID `json:"redirectedRequestId"`
}

// FetchAuthRequired is Fetch.authRequired: a request met an authentication
// challenge. First consumer: HTTP authentication.
var FetchAuthRequired = Event[FetchAuthRequiredEvent]{name: "Fetch.authRequired"}

// FetchAuthRequiredEvent is Fetch.authRequired's parameters.
type FetchAuthRequiredEvent struct {
	RequestID     RequestID     `json:"requestId"`
	Request       Request       `json:"request"`
	FrameID       FrameID       `json:"frameId"`
	ResourceType  string        `json:"resourceType"`
	AuthChallenge AuthChallenge `json:"authChallenge"`
}

// AuthChallenge is Fetch.AuthChallenge.
type AuthChallenge struct {
	// Source is "Server" or "Proxy".
	Source string `json:"source"`
	Origin string `json:"origin"`
	Scheme string `json:"scheme"`
	Realm  string `json:"realm"`
}
