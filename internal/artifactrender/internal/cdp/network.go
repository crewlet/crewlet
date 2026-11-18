package cdp

// Request is Network.Request, as much of it as the render reads.
type Request struct {
	URL    string `json:"url"`
	Method string `json:"method"`
	// Headers is Network.Headers, an object of header names to values.
	Headers map[string]string `json:"headers"`
}
