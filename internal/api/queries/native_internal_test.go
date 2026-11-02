package queries

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A FRESHNESS REFUSAL ON ANY DOMAIN'S QUESTION NAMES ITS PARAMETER AND NO
// DOMAIN.
//
// [Freshness] serves every native question — the board's, the knowledge
// base's, the inbox's — so its refusal is the one a person asking a wiki
// question reads. It read through the tracker's grammar and said "worklet:",
// naming a domain the caller never asked about. It is a bad request, and the
// grammar's own refusal stays reachable beneath it.
//
// Mutation: wrap the grammar's refusal in a domain's prefix here, or parse
// through anything but [statelog.ParseFreshness], and this fails.
func TestAFreshnessRefusalOnAnyQuestionNamesNoDomain(t *testing.T) {
	t.Parallel()
	for _, bag := range []map[string]any{
		{"read_level": "eventually"},
		{"read_level": "session"},
		{"min_position": "4711"},
		{"read_level": "linearizable", "max_lag_seq": "10"},
	} {
		_, err := Freshness(FromMap(bag))
		if !errors.Is(err, ErrBadParams) {
			t.Errorf("%v answered %v, want a bad request", bag, err)
			continue
		}
		var refused *statelog.FreshnessError
		if !errors.As(err, &refused) {
			t.Errorf("%v answered %v, which is not the grammar's own refusal", bag, err)
		}
		for _, pkg := range []string{"worklet:", "doclet:", "statelog:"} {
			if strings.Contains(err.Error(), pkg) {
				t.Errorf("%v was refused %q, which names %q", bag, err, pkg)
			}
		}
	}
}
