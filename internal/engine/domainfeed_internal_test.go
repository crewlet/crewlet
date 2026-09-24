package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY REGISTERED DOMAIN'S FEED IS THE ONE IT DECLARES.
//
// Two parties name a log's wake consumer: the feed that advances it and the
// trim that waits for it, from a node that may build no feed at all. The trim
// reads the domain's declaration, so a feed running under any other group —
// or a feed nobody declared, or a declaration nobody runs — leaves the trim
// waiting for ever or waiting for nothing. Every registered domain has to
// build cleanly, so this build cannot boot into either.
func TestEveryRegisteredDomainsFeedIsTheOneItDeclares(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	for _, domain := range registeredDomains() {
		t.Run(domain.Name(), func(t *testing.T) {
			t.Parallel()
			translator, _, err := e.feedFor(&runningDomain{domain: domain})
			if err != nil {
				t.Fatalf("feedFor: %v", err)
			}
			declared := domain.FeedGroup()
			switch {
			case declared == "" && translator != nil:
				t.Fatalf("%s declares no wake feed and runs one as %q",
					domain.Name(), translator.Source().Group)
			case declared != "" && translator == nil:
				t.Fatalf("%s declares the wake feed %q and runs none",
					domain.Name(), declared)
			case translator != nil && translator.Source().Group != declared:
				t.Fatalf("%s declares %q and its feed runs as %q",
					domain.Name(), declared, translator.Source().Group)
			}
		})
	}
}

// pagesOnTrackersGroup is the knowledge base declaring the tracker's
// consumer, which is the answer the trim used to give on its behalf.
type pagesOnTrackersGroup struct{ pages.Domain }

func (pagesOnTrackersGroup) FeedGroup() string { return tracker.FeedGroup }

// pagesWithoutFeed is the knowledge base declaring no feed while one runs.
type pagesWithoutFeed struct{ pages.Domain }

func (pagesWithoutFeed) FeedGroup() string { return "" }

// vectorsWithFeed is the vectors declaring a feed nothing runs.
type vectorsWithFeed struct{ search.Domain }

func (vectorsWithFeed) FeedGroup() string { return "crewlet-vectors-feed" }

// A DOMAIN THAT DISAGREES WITH ITS OWN FEED IS REFUSED BY NAME, which is what
// says the case above can fail as well as pass. Each arm is a trim that goes
// wrong silently at run time.
func TestAFeedThatDisagreesWithItsDeclarationIsRefused(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	for name, tc := range map[string]struct {
		running *runningDomain
		names   string
	}{
		"a feed running under another domain's group": {
			running: &runningDomain{domain: pagesOnTrackersGroup{}},
			names:   "its feed runs as",
		},
		"a feed running with no declaration": {
			running: &runningDomain{domain: pagesWithoutFeed{}},
			names:   "declares no wake feed",
		},
		"a declaration no feed runs": {
			running: &runningDomain{domain: vectorsWithFeed{}},
			names:   "runs none",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			translator, opener, err := e.feedFor(tc.running)
			if err == nil {
				t.Fatalf("feedFor built a feed for %s", name)
			}
			if translator != nil || opener != nil {
				t.Errorf("feedFor refused and still handed back a feed to start")
			}
			if !strings.Contains(err.Error(), tc.names) ||
				!strings.Contains(err.Error(), tc.running.domain.Name()) {
				t.Errorf("the refusal does not say which domain or why: %v", err)
			}
		})
	}
}
