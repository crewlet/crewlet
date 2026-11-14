package cdp

// NetworkRequestID is Network.RequestId: Network's id for a request, apart
// from Fetch's [RequestID] for the same request while it is paused.
type NetworkRequestID string

// NetworkEnable is Network.enable: report the target's traffic, which
// Network.responseReceivedExtraInfo rides on. First consumer: the request
// gate.
var NetworkEnable = Method[struct{}, struct{}]{name: "Network.enable"}

// NetworkSetBypassServiceWorker is Network.setBypassServiceWorker: send every
// request to the network rather than to a service worker, which would answer
// from beyond the gate's sight. First consumer: the request gate.
var NetworkSetBypassServiceWorker = Method[NetworkSetBypassServiceWorkerParams, struct{}]{name: "Network.setBypassServiceWorker"}

// NetworkSetBypassServiceWorkerParams is Network.setBypassServiceWorker's
// parameters.
type NetworkSetBypassServiceWorkerParams struct {
	Bypass bool `json:"bypass"`
}

// NetworkDeleteCookies is Network.deleteCookies, a partitioned cookie named
// by its partition key. First consumer: forgetting a sign-in
// (internal/browser).
var NetworkDeleteCookies = Method[NetworkDeleteCookiesParams, struct{}]{name: "Network.deleteCookies"}

// NetworkDeleteCookiesParams is Network.deleteCookies' parameters.
type NetworkDeleteCookiesParams struct {
	Name         string              `json:"name"`
	URL          string              `json:"url,omitempty"`
	Domain       string              `json:"domain,omitempty"`
	Path         string              `json:"path,omitempty"`
	PartitionKey *CookiePartitionKey `json:"partitionKey,omitempty"`
}

// CookiePartitionKey is Network.CookiePartitionKey: the top-level site a
// partitioned (CHIPS) cookie is kept for, an object since the protocol
// stopped spelling it as a string.
type CookiePartitionKey struct {
	TopLevelSite         string `json:"topLevelSite"`
	HasCrossSiteAncestor bool   `json:"hasCrossSiteAncestor"`
}

// Cookie is Network.Cookie. Its value is a credential: this package never
// logs or reports it.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	Session  bool    `json:"session"`
	// SameSite is "Strict", "Lax" or "None", or empty.
	SameSite string `json:"sameSite"`
	// Priority is "Low", "Medium" or "High".
	Priority string `json:"priority"`
	// SourceScheme is "Unset", "NonSecure" or "Secure".
	SourceScheme string              `json:"sourceScheme"`
	SourcePort   int64               `json:"sourcePort"`
	PartitionKey *CookiePartitionKey `json:"partitionKey"`
}

// CookieParam is Network.CookieParam: a cookie to set, as a restored sign-in
// sets it.
type CookieParam struct {
	Name         string              `json:"name"`
	Value        string              `json:"value"`
	URL          string              `json:"url,omitempty"`
	Domain       string              `json:"domain,omitempty"`
	Path         string              `json:"path,omitempty"`
	Secure       bool                `json:"secure,omitempty"`
	HTTPOnly     bool                `json:"httpOnly,omitempty"`
	SameSite     string              `json:"sameSite,omitempty"`
	Expires      float64             `json:"expires,omitempty"`
	Priority     string              `json:"priority,omitempty"`
	SourceScheme string              `json:"sourceScheme,omitempty"`
	SourcePort   int64               `json:"sourcePort,omitempty"`
	PartitionKey *CookiePartitionKey `json:"partitionKey,omitempty"`
}

// Request is Network.Request, as much of it as the gate reads.
type Request struct {
	URL    string `json:"url"`
	Method string `json:"method"`
	// Headers is Network.Headers, an object of header names to values.
	Headers     map[string]string `json:"headers"`
	HasPostData bool              `json:"hasPostData"`
}

// NetworkResponseReceivedExtraInfo is Network.responseReceivedExtraInfo: a
// response's headers as they arrived, Set-Cookie included. First consumer:
// sign-in checkpoints (internal/browser).
var NetworkResponseReceivedExtraInfo = Event[NetworkResponseReceivedExtraInfoEvent]{name: "Network.responseReceivedExtraInfo"}

// NetworkResponseReceivedExtraInfoEvent is
// Network.responseReceivedExtraInfo's parameters.
type NetworkResponseReceivedExtraInfoEvent struct {
	RequestID  NetworkRequestID  `json:"requestId"`
	StatusCode int64             `json:"statusCode"`
	Headers    map[string]string `json:"headers"`
}
