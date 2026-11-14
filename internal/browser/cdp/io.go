package cdp

// IORead is IO.read: the next chunk of a stream, bounded by Size at its
// source. First consumer: downloads on a remote backend.
var IORead = Method[IOReadParams, IOReadResult]{name: "IO.read"}

// IOReadParams is IO.read's parameters.
type IOReadParams struct {
	Handle StreamHandle `json:"handle"`
	// Size is the most bytes this chunk may hold.
	Size int64 `json:"size,omitempty"`
}

// IOReadResult is IO.read's result.
type IOReadResult struct {
	// Base64Encoded says Data is base64, as a binary body's is.
	Base64Encoded bool   `json:"base64Encoded"`
	Data          string `json:"data"`
	EOF           bool   `json:"eof"`
}

// IOClose is IO.close. First consumer: downloads on a remote backend.
var IOClose = Method[IOCloseParams, struct{}]{name: "IO.close"}

// IOCloseParams is IO.close's parameters.
type IOCloseParams struct {
	Handle StreamHandle `json:"handle"`
}
