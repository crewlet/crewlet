package queries

import (
	"fmt"
	"strings"
	"time"
)

// dayBoundsParams is the asker's day bounds a code question names in `day_bounds`: RFC 3339 instants,
// comma-separated or a list off a socket frame, each the reader's to judge; nil where it names none.
func dayBoundsParams(p Params) ([]time.Time, error) {
	listed, ok := p.List("day_bounds")
	if !ok {
		return nil, fmt.Errorf("%w: the day_bounds parameter is RFC 3339 instants, comma-separated", ErrBadParams)
	}
	var bounds []time.Time
	for _, v := range listed {
		for raw := range strings.SplitSeq(v, ",") {
			if raw = strings.TrimSpace(raw); raw == "" {
				continue
			}
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return nil, fmt.Errorf("%w: day_bounds must be RFC 3339 instants (2026-04-16T09:00:00Z), and %q "+
					"isn't", ErrBadParams, raw)
			}
			bounds = append(bounds, at)
		}
	}
	return bounds, nil
}
