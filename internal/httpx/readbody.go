package httpx

import (
	"errors"
	"fmt"
	"io"
)

// ErrResponseTooLarge is what [ReadBody] answers for a body past its ceiling.
//
// A SENTINEL, because the three clients that buffer each wrap it with their
// own endpoint and path, and a caller deciding what to do about it —
// a reconcile pass that wants to report the endpoint rather than retry — has
// to be able to tell it from the syntax error it used to arrive as.
var ErrResponseTooLarge = errors.New("httpx: response past the ceiling this build reads")

// ReadBody buffers a response body whole, REFUSING one past max.
//
// A CAP IS NOT A CUT, and this is the difference. io.LimitReader stops at its
// limit and reports io.EOF — indistinguishable, to everything downstream,
// from a body that genuinely ended there. A client that capped and then
// decoded therefore read a truncated answer as an ordinary syntax error
// ("unexpected end of JSON input", naming neither the endpoint nor the cap),
// and a client that capped and then quoted the result read half a refusal as
// the whole of one. Reading ONE BYTE PAST the ceiling is what makes the
// overrun visible, which is the same idiom the operator CLI, the OTLP
// receiver and the CLI-agent bundle reader already use.
//
// It is for the clients that BUFFER — the ones that read once and then branch
// on the status, because the body is both the refusal's detail and the
// success's payload. A client that streams into a json.Decoder does not need
// it: the decoder consumes as it reads, so there is nothing to buffer and its
// own [MaxResponseBody] LimitReader is the ceiling.
func ReadBody(r io.Reader, max int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > max {
		return nil, fmt.Errorf("%w: %d bytes and still going — the endpoint "+
			"is not answering a page this engine asked for", ErrResponseTooLarge, max)
	}
	return buf, nil
}
