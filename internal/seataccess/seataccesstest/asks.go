package seataccesstest

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AskFaults is why any grant opts offers could not be asked of a person
// ([seataccess.RequestAsk]), each asked at its longest: a seat name past any
// real one, every scope entry opts allows, the longest target, reason and
// hours, and as a write the provisioner lacks.
func AskFaults(opts seataccess.Options) []error {
	seat := strings.Repeat("Principal Site Reliability Engineer ", 16)
	target := "b" + strings.Repeat("x", 254)
	reason := strings.Repeat("r", decision.MaxRationale)
	var out []error
	for _, e := range opts.Grants {
		e.Write, e.Unavailable = true, cmp.Or(e.Unavailable, "The provisioner does not hold "+e.Name)
		if len(e.Missing) == 0 {
			e.Missing = []string{e.Name}
		}
		hours := seataccess.MaxGrantHours
		if e.OneChange() {
			hours = e.MaxHours
		}
		var scope []string
		if e.ScopeKind != seataccess.GrantScopeAccount {
			scope = opts.GrantScope
		}
		req := seataccess.GrantRequest{ID: seataccess.NewRequestID(), Kind: opts.Lane.Kind(), Lane: opts.Lane,
			Handle: "agent", Permission: e.ID, Scope: scope, Hours: hours, Reason: reason, Target: target}
		body, asked := seataccess.RequestAsk(seat, req, e)
		if err := asked.ValidateRecommendedAt(decision.FieldPrefix); err != nil {
			out = append(out, fmt.Errorf("%s: %w", e.ID, err))
		}
		if len(body) > worklet.MaxCommentBody {
			out = append(out, fmt.Errorf("%s: the body is %d bytes, past a comment's %d", e.ID, len(body),
				worklet.MaxCommentBody))
		}
	}
	return out
}
