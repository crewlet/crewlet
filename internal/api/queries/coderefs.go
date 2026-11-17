package queries

import (
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// refListingParams is a refs listing's order, window, find, detail and active
// count as the request names them; nil where it names none, so an older data
// node is asked nothing it refuses.
func refListingParams(p Params) (*gitlet.RefListingQuery, error) {
	q := gitlet.RefListingQuery{Order: gitlet.RefOrder(strings.TrimSpace(p.String("order"))),
		Contains: p.String("contains")}
	var err error
	for _, instant := range []struct {
		key string
		at  *time.Time
	}{{"moved_since", &q.MovedSince}, {"moved_before", &q.MovedBefore}, {"active_since", &q.ActiveSince}} {
		if *instant.at, err = instantParam(p, instant.key); err != nil {
			return nil, err
		}
	}
	if q.Detail, err = askedFor(p, "detail"); err != nil {
		return nil, err
	}
	if q.Order == gitlet.RefOrderName {
		q.Order = "" // today's order, so an explicit name alone sends no ref_listing
	}
	if q == (gitlet.RefListingQuery{}) {
		return nil, nil
	}
	return &q, nil
}
