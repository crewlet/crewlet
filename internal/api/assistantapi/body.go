package assistantapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// readBodyWithin reads a JSON body into v: declared JSON, within max bytes,
// exactly one object, and no field v does not declare. Every route of this
// surface that takes a body reads it here, each with its own bound.
//
// JSON AND NOTHING ELSE, for the act route's reason: a cross-site form can post
// text/plain without a preflight, and the Origin gate in front of every guarded
// write is the second lock, never the only one.
func readBodyWithin(w http.ResponseWriter, r *http.Request, v any, max int64) bool {
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if charset, named := params["charset"]; err != nil || media != "application/json" ||
		(named && !strings.EqualFold(charset, "utf-8")) {
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
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "the body must be one JSON object of the route's " +
				"own fields: " + err.Error()})
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "the body carries something after its JSON object"})
		return false
	}
	return true
}
