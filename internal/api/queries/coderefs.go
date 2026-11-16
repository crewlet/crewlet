package queries

import (
	"fmt"
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
	for _, instant := range []struct {
		key string
		at  *time.Time
	}{{"moved_since", &q.MovedSince}, {"moved_before", &q.MovedBefore}, {"active_since", &q.ActiveSince}} {
		v := strings.TrimSpace(p.String(instant.key))
		if v == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, fmt.Errorf("%w: %s %q is not an RFC 3339 instant", ErrBadParams, instant.key, v)
		}
		*instant.at = at
	}
	var err error
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
