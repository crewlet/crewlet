package assistantapi

import (
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/plainjson"
)

// readBodyWithin reads a JSON body into v: declared JSON, within max bytes,
// exactly one object, and no field v does not declare. Every route of this
// surface that takes a body reads it here, each with its own bound.
//
// JSON AND NOTHING ELSE, for the act route's reason: a cross-site form can post
// text/plain without a preflight, and the Origin gate in front of every guarded
// write is the second lock, never the only one.
func readBodyWithin(w http.ResponseWriter, r *http.Request, v any, max int64) bool {
	if !httpjson.DeclaresJSON(r.Header.Get("Content-Type")) {
		httpjson.FailWithFields(w, http.StatusUnsupportedMediaType,
			httpjson.CodeUnsupportedMediaType, httpjson.Detail{
				"detail": "the body must be declared Content-Type: application/json"})
		return false
	}
	raw, err := httpjson.ReadBody(w, r, max)
	if err != nil {
		httpjson.Refuse(w, err)
		return false
	}
	// IN WORDS, never the decoder's own ([plainjson.Unreadable]): its error
	// names Go's types and struct fields, which a person reading the detail
	// can do nothing with.
	switch err := httpjson.DecodeStrict(raw, v); {
	case errors.Is(err, httpjson.ErrTrailingData):
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": plainjson.Unreadable("the body", plainjson.ErrTrailing)})
		return false
	case err != nil:
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": plainjson.Unreadable("the body", err) +
				". Send one JSON object of the route's own fields"})
		return false
	}
	return true
}
