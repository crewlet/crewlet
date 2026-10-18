package pagepolicy

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
)

// WHICH REPRESENTATION A BROWSER IS SENT, beside what it is told about it: the
// dashboard's assets and the artifact origin's files are both gzipped for a
// client that asks, under one negotiation and one entity-tag scheme, so the
// two surfaces cannot come to read one Accept-Encoding two ways.

// GzipSuffix marks the gzip representation's entity tag. Each representation
// has its own, as a strong tag must: a tag names BYTES, and a cache that holds
// both (one keyed per Accept-Encoding, which is what Vary asks of it) would
// otherwise revalidate one body with the other's tag and be told it was still
// current.
const GzipSuffix = "-gz"

// GzipTag is the gzip representation's entity tag, derived from the identity
// one: `"abc"` becomes `"abc-gz"`.
func GzipTag(identity string) string {
	return strings.TrimSuffix(identity, `"`) + GzipSuffix + `"`
}

// GzipSmaller compresses data at level, or answers nil when the result is not
// smaller: a tiny file costs more in gzip's own header and trailer than it
// saves, and a body that grew is a body nobody should be sent. nil means "serve
// it as it is", which is also the answer to a failure that cannot happen: a
// gzip writer into memory at a valid level has no error to return.
func GzipSmaller(data []byte, level int) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(data); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	if buf.Len() >= len(data) {
		return nil
	}
	return buf.Bytes()
}

// AcceptsGzip reports whether a request's Accept-Encoding admits gzip.
//
// ASKED, NEVER ASSUMED. RFC 9110 reads a request with no Accept-Encoding at
// all as accepting any coding, and no browser sends such a request; the
// clients that do are curl, a script and a health probe, which is to say the
// ones that would print compressed bytes. So an absent or empty header is
// identity, and so is one that names only codings this does not produce.
//
// gzip is admitted by a `gzip` member (or `x-gzip`, which §12.5.3 has a
// recipient treat as the same coding) with a weight above zero, or failing any
// such member by a `*` with one. A member that names gzip outranks the
// wildcard whatever the order, so `*, gzip;q=0` refuses it. A weight that is
// not a qvalue admits nothing: identity is always a correct answer, and an
// encoded body a client did not ask for is not.
func AcceptsGzip(h http.Header) bool {
	named, namedOK, wildOK := false, false, false
	for _, field := range h.Values("Accept-Encoding") {
		for _, member := range strings.Split(field, ",") {
			coding, params, _ := strings.Cut(member, ";")
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "gzip", "x-gzip":
				named = true
				namedOK = namedOK || positiveWeight(params)
			case "*":
				wildOK = wildOK || positiveWeight(params)
			}
		}
	}
	if named {
		return namedOK
	}
	return wildOK
}

// positiveWeight reports whether a member's parameters give it a weight above
// zero. No `q` parameter is weight 1.
func positiveWeight(params string) bool {
	for _, param := range strings.Split(params, ";") {
		name, value, found := strings.Cut(param, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		return positiveQValue(strings.TrimSpace(value))
	}
	return true
}

// positiveQValue reads a weight by RFC 9110 §12.4.2's grammar,
//
//	qvalue = ( "0" [ "." 0*3DIGIT ] ) / ( "1" [ "." 0*3("0") ] )
//
// and reports whether it is above zero. Anything the grammar does not produce
// is not a weight, so it is not a positive one.
func positiveQValue(v string) bool {
	whole, frac, _ := strings.Cut(v, ".")
	if len(frac) > 3 {
		return false
	}
	for _, c := range frac {
		if c < '0' || c > '9' || (whole == "1" && c != '0') {
			return false
		}
	}
	switch whole {
	case "1":
		return true
	case "0":
		return strings.Trim(frac, "0") != ""
	default:
		return false
	}
}
