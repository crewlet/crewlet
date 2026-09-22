package chartapi

import (
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// freshnessParams is the request's query string, narrowed to the keys the
// freshness grammar reads.
//
// NARROWED, because that grammar REFUSES a key nobody parsed — deliberately,
// since a filter nothing read is a board showing more than the person asked
// for. This surface has parameters of its own (`runtime`), so handing it the
// whole query string would make every chart read that asked for the runtime
// half fail as an unknown filter. Narrowing here rather than widening the
// grammar keeps that refusal intact for the keys it is about.
func freshnessParams(r *http.Request) tracker.Params {
	got := r.URL.Query()
	kept := make(map[string][]string, len(got))
	for _, key := range tracker.QueryKeys {
		if values, held := got[key]; held {
			kept[key] = values
		}
	}
	return queries.FromQuery(kept)
}

// LimitParam is how many rows a listing route was asked for.
const LimitParam = "limit"

// chartParams are the keys this surface reads that the freshness grammar does
// not, so an unknown one can be refused on the same terms.
var chartParams = []string{RuntimeParam, LimitParam}

// refuseUnknownParams reports the first query key neither this surface nor
// the freshness grammar reads.
//
// THE SAME PROMISE THE GRAMMAR MAKES, kept over the union rather than over
// half of it: a caller who misspells `runtime` and is silently served the
// stripped answer has no way to tell that from a company with no runtime at
// all, which is the exact ambiguity the `runtime` flag in the response exists
// to remove.
func refuseUnknownParams(r *http.Request) string {
	known := append(slices.Clone(tracker.QueryKeys), chartParams...)
	for key := range r.URL.Query() {
		if !slices.Contains(known, key) {
			return key
		}
	}
	return ""
}

// readParams is every read route's opening: refuse a parameter nobody reads,
// then resolve the level this read runs at.
//
// ONE HELPER rather than the same eight lines per route, because the two
// steps have an ORDER and it is not obvious. An unknown parameter is refused
// FIRST: a caller who misspelled `read_level` and was served a stale answer
// at the default level has been told nothing, and the misspelling is the
// thing they can fix.
func (s *Service) readParams(w http.ResponseWriter, r *http.Request) (
	statelog.Freshness, bool) {

	if key := refuseUnknownParams(r); key != "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": "this route does not read " + key})
		return statelog.Freshness{}, false
	}
	fresh, err := s.freshness(r)
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
		return statelog.Freshness{}, false
	}
	return fresh, true
}
