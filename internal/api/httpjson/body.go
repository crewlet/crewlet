package httpjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strings"
)

// DeclaresJSON reports whether a request's Content-Type header declares a JSON
// body: `application/json`, with no charset or `utf-8`.
//
// THE DOOR A BROWSER LEAVES OPEN is what this shuts: a cross-site HTML form can
// post `text/plain`, `application/x-www-form-urlencoded` or
// `multipart/form-data` without a preflight, and a body that happens to parse
// as JSON would otherwise reach a write. `application/json` from another
// origin needs a preflight, and the cross-site write gate in front of every
// guarded route refuses its Origin as well, so this is the first lock of two.
func DeclaresJSON(header string) bool {
	media, params, err := mime.ParseMediaType(header)
	if err != nil || media != "application/json" {
		return false
	}
	charset, named := params["charset"]
	return !named || strings.EqualFold(charset, "utf-8")
}

// ErrTrailingData reports a body that carries something after its one JSON
// value.
var ErrTrailingData = errors.New("the body carries something after its JSON object; send exactly one")

// DecodeStrict decodes raw into v as exactly one JSON value, refusing a field v
// does not declare and anything after the value ([ErrTrailingData]).
//
// A KEY THE ROUTE DOES NOT READ IS REFUSED rather than ignored: a client
// naming something in the body the route takes elsewhere (an operation, an
// owner) would otherwise send every request believing it named what it did
// not. The decoder's own error is returned as it is, for the caller to say
// what its body should be.
func DecodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ErrTrailingData
	}
	return nil
}
