package webhooks

import (
	"github.com/crewlet/crewlet/internal/api/httpjson"

	"encoding/json"
	"net/http"
	"strconv"
)

// MaxBodyBytes bounds what an unverified caller can make this process buffer.
//
// The body must be read BEFORE the signature can be checked — the signature is
// over the body — so without a bound anyone who can reach the port picks the
// allocation size. 25 MiB is GitHub's own documented ceiling and the largest of
// the six providers', so no legitimate delivery is refused by it.
//
// It is NOT the transport's limit, and deriving it from one was tried and
// withdrawn. What a delivery costs on the wire is a property of its BYTES
// rather than its length: the published event carries the parsed body
// re-marshaled AND the exact signed bytes as base64, and encoding/json escapes
// '<', '>' and '&' to six bytes each — so a body of ampersands encodes at 7.3x
// where the same length of plain text encodes at 2.3x (measured). Any single
// divisor is therefore wrong for one of them and wasteful for the other, and
// the one that looked safe still let an escape-heavy body through. The
// transport answers that question itself: an event that does not fit is
// refused with queue.ErrTooLarge, and the receiver turns that into a 413 —
// see Receiver.accept.
const MaxBodyBytes = 25 << 20

// errBodyTooLarge is what a body over the cap surfaces as.
var errBodyTooLarge = httpjson.ErrTooLarge

// readBody reads at most MaxBodyBytes. The draining rule it relies on — an
// answer without a drained body reaches the provider as a connection reset
// rather than the status it was sent — lives with [httpjson.ReadBody].
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return httpjson.ReadBody(w, r, MaxBodyBytes)
}

// parseObject decodes a webhook body, accepting only a JSON object.
//
// json.Unmarshal happily produces a slice or a scalar, and every reader here
// immediately asks it for a field. A correctly signed list body must be a 400,
// not a panic on the way to a 500.
func parseObject(raw []byte) (map[string]any, bool) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, false
	}
	// A literal `null` unmarshals into a nil map with no error, which every
	// accessor below then reads as an empty object — a delivery with no
	// content at all, reported as accepted.
	if body == nil {
		return nil, false
	}
	return body, true
}

// The accessors. A webhook body is the only input in this package with no
// schema, so every read states what it does with a value of the wrong shape
// rather than asserting.

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func object(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}

func list(m map[string]any, key string) []any {
	l, _ := m[key].([]any)
	return l
}

// num renders a scalar identifier — an issue number, a merge-request iid.
//
// It accepts a string as well as a number because providers disagree about
// which one an id is. A value of any other shape yields "", so a renamed field
// shortens the line rather than printing "<nil>".
//
// JSON decodes every number as a float64, so the format matters: 'f' with
// precision -1 is the SHORTEST representation that round trips, which renders
// an integral value with no decimal point at all. PR #42 reads "42", where a
// naive %f reads "42.000000".
//
// It carried an int64-conversion branch ahead of this, and mutation testing
// found the branch dead: measured across the range, it produces byte-identical
// output for every value it applies to, and the values it cannot handle are
// exactly the ones whose float-to-int conversion is undefined — which fall
// through to here anyway.
func num(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// firstOf returns the first key that holds a non-empty string.
//
// Confluence names its event three different ways depending on the deployment
// and the era, and a reader that knew one of them would file every delivery
// from the others under "unknown".
func firstOf(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := str(m, k); v != "" {
			return v
		}
	}
	return ""
}
