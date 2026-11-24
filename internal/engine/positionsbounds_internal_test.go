package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY LOG THIS BUILD RUNS FITS WHAT A POSITIONS RECORD NAMES: the register
// holds a node's positions, a hold's and a backup's streams and a feed's mark
// to at most coord.MaxLogDomains logs, each domain, stream and feed group
// named in a token of at most its bound, and a trim floor to
// coord.MaxTrimTerms terms, so a domain the register gains moves those bounds
// here rather than refusing every node's row once it ships.
//
// Mutation: lower coord.MaxLogDomains below fourteen, or coord.MaxTrimTerms
// below six, and this is red.
func TestEveryLogThisBuildRunsFitsWhatAPositionsRecordNames(t *testing.T) {
	t.Parallel()
	domains := registeredDomains()
	if len(domains) == 0 || len(domains) > coord.MaxLogDomains {
		t.Errorf("the build registers %d logs, and a positions record names at most %d", len(domains),
			coord.MaxLogDomains)
	}
	for _, d := range domains {
		if !coord.ValidDomainName(d.Name()) || !coord.ValidStreamName(d.Stream().Name) {
			t.Errorf("the %q log on %q is no name a positions record holds", d.Name(), d.Stream().Name)
		}
		if group := d.FeedGroup(); group != "" && !coord.ValidDomainName(group) {
			t.Errorf("the %q log's feed group %q is no name a feed mark holds", d.Name(), group)
		}
	}
	terms := statelog.TrimInputs{}.Terms()
	if len(terms) > coord.MaxTrimTerms {
		t.Errorf("the trim evaluates %d terms, and a floor carries at most %d", len(terms), coord.MaxTrimTerms)
	}
	for _, term := range terms {
		if len(term.Name) == 0 || len(term.Name) > coord.MaxTrimTermNameBytes {
			t.Errorf("the trim's term %q is no name a floor holds", term.Name)
		}
	}
}
