package queries

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// countsParams is the parts a counts question names in `parts`, comma-separated or a list off
// a socket frame, as the read's own keys; nil where it names none, which the reader refuses.
func countsParams(p Params) (*gitlet.CountsQuery, error) {
	listed, ok := p.List("parts")
	if !ok {
		return nil, fmt.Errorf("%w: the parts parameter is part names, comma-separated", ErrBadParams)
	}
	var q gitlet.CountsQuery
	named := false
	for _, v := range listed {
		for part := range strings.SplitSeq(v, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			if !q.Ask(gitlet.CountPart(part)) {
				names := make([]string, 0, len(gitlet.CountParts()))
				for _, known := range gitlet.CountParts() {
					names = append(names, string(known))
				}
				return nil, badParams("parts", part, names)
			}
			named = true
		}
	}
	if !named {
		return nil, nil
	}
	return &q, nil
}
