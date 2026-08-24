package webhooks

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

// MaxBodyBytes bounds what an unverified caller can make this process buffer.
//
// The body must be read BEFORE the signature can be checked — the signature is
// over the body — so without a bound anyone who can reach the port picks the
// allocation size. 25 MiB is GitHub's own documented ceiling and the largest of
// the seven providers', so no legitimate delivery is refused by it.
const MaxBodyBytes = 25 << 20

// errBodyTooLarge is what a body over the cap surfaces as.
var errBodyTooLarge = errors.New("webhooks: body over the limit")

// readBody reads at most MaxBodyBytes.
//
// It reads the WHOLE body even when the request will be refused. An HTTP
// server that answers without draining leaves unread bytes in the socket, and
// the provider sees a connection reset instead of the status it was sent —
// which for a 401 means "retry forever" rather than "your signature is wrong".
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err == nil {
		return raw, nil
	}
	var overflow *http.MaxBytesError
	if errors.As(err, &overflow) {
		return nil, errBodyTooLarge
	}
	return nil, err
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
