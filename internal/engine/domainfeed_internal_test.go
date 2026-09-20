package engine

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/tracker"
)

// TestTheTrimAsksEachDomainForItsOwnWakeFeed is the invariant the knowledge
// base's log grew without.
//
// The trim will not purge past what a domain's wake feed has acknowledged,
// and it asks the broker for that consumer BY NAME. The name it asked was the
// TRACKER's, spelled into the term, for every domain that has a feed — so on
// CREWLET_PAGES_LOG the lookup found no such consumer, the term read that as
// a floor of zero, it permitted nothing, and the log only ever grew. Nothing
// says so anywhere: the term is simply the lowest of six and wins every tick
// in silence.
//
// EVERY REGISTERED DOMAIN HAS A ROW, so a fourth one fails here rather than
// inheriting whichever group a map happened to answer.
func TestTheTrimAsksEachDomainForItsOwnWakeFeed(t *testing.T) {
	t.Parallel()

	// THE GROUP NAMES ARE LITERALS, for the reason engine_test's own
	// TestTheNativeFeedGroupNamesNeverMove gives: the name is where the
	// fleet's position IS, so it is a stored value rather than an
	// expression — and a test that re-derived it from the code under test
	// would agree with a rename it exists to refuse.
	//
	// An empty string is a domain with NO wake feed, which is a different
	// answer from a feed nobody has started: absent excuses the term,
	// unstarted blocks it.
	wantGroup := map[string]string{
		tracker.Domain{}.Name(): "crewlet-tracker-feed",
		pages.Domain{}.Name():   "crewlet-pages-feed",
		search.Domain{}.Name():  "",
	}

	const floor = 4242
	for _, domain := range registeredDomains() {
		name := domain.Name()
		want, listed := wantGroup[name]
		if !listed {
			t.Errorf("%s is registered and has no row here: its wake feed's "+
				"durable consumer is what the trim will not purge past, and a "+
				"domain that inherits another's group reports a floor of zero "+
				"for ever", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var asked []string
			// THE BROKER'S HONEST ANSWER for a consumer that is not on
			// this log is "no such consumer" — which is exactly what
			// the defect produced, and exactly what a correct term
			// must never provoke.
			seq, has, readable := feedTermOf(name, func(group string) (uint64, bool, error) {
				asked = append(asked, group)
				if group != want {
					return 0, false, nil
				}
				return floor, true, nil
			})
			if want == "" {
				if has || readable || len(asked) != 0 {
					t.Fatalf("a domain with no wake feed reported has=%v "+
						"readable=%v after asking %v — a term it does not "+
						"have must be ABSENT, or its log never trims",
						has, readable, asked)
				}
				return
			}
			if len(asked) != 1 || asked[0] != want {
				t.Fatalf("the feed term asked for %v, want exactly [%q] — any "+
					"other name is a consumer that is not on this domain's "+
					"log, and the term then permits nothing for ever",
					asked, want)
			}
			if !has || !readable || seq != floor {
				t.Fatalf("the feed term reported seq=%d has=%v readable=%v, "+
					"want %d/true/true", seq, has, readable, floor)
			}
		})
	}
}

// TestNoTwoDomainsShareAWakeFeedsConsumer is the other half: the term is only
// correct per domain if the names are distinct per domain.
//
// Two domains on one group is worse than a wrong name — one consumer would be
// delivered records from a log it cannot decode, and each domain's trim would
// be held at the other's position.
func TestNoTwoDomainsShareAWakeFeedsConsumer(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, domain := range registeredDomains() {
		group, feeds := feedGroup(domain.Name())
		if !feeds {
			continue
		}
		if group == "" {
			t.Errorf("%s declares a wake feed with no consumer name", domain.Name())
			continue
		}
		if other, clash := seen[group]; clash {
			t.Errorf("%s and %s share the durable consumer %q", other, domain.Name(), group)
			continue
		}
		seen[group] = domain.Name()
	}
}

// TestTheFeedTermBlocksTheTrimWhenTheConsumerCannotBeRead is the three-valued
// half of the term: "I could not read it" is not "there is nothing to keep".
//
// Reporting an unreadable consumer as a satisfied term would let one broker
// blip purge records the company's own notification feed has never seen —
// which is a wake nobody ever receives, with the record gone to prove it.
func TestTheFeedTermBlocksTheTrimWhenTheConsumerCannotBeRead(t *testing.T) {
	t.Parallel()
	seq, has, readable := feedTermOf(tracker.Domain{}.Name(),
		func(string) (uint64, bool, error) {
			return 0, false, errors.New("the broker is unreachable")
		})
	if !has || readable || seq != 0 {
		t.Fatalf("an unreadable consumer reported seq=%d has=%v readable=%v, "+
			"want 0/true/false — a term nobody could read is not a term "+
			"nobody needs", seq, has, readable)
	}
}

// TestTheFeedTermAsksTheConsumerTheFeedActuallyOpens ties the two halves of
// [nativeFeeds] together.
//
// The trim reads a consumer's acknowledged floor and the change feed CREATES
// that consumer, from the translator's own [changefeed.Source]. If the two
// ever come from different places the trim silently measures a consumer that
// does not exist — which is the whole defect, and it had no other symptom.
func TestTheFeedTermAsksTheConsumerTheFeedActuallyOpens(t *testing.T) {
	t.Parallel()
	// A NON-NIL SKILLS FUNCTION, because the registry takes one and the
	// claim is that it changes nothing about a translator's identity.
	feeds := nativeFeeds(func() string { return "Tool Skills" })
	for _, domain := range registeredDomains() {
		feed, registered := feeds[domain.Name()]
		group, terms := feedGroup(domain.Name())
		if registered != terms {
			t.Errorf("%s: the feed registry says %v and the trim's term says %v — "+
				"one of them is measuring a consumer the other never opens",
				domain.Name(), registered, terms)
			continue
		}
		if !registered {
			continue
		}
		if opened := feed.translator.Source().Group; opened != group {
			t.Errorf("%s: the feed opens %q and the trim reads %q",
				domain.Name(), opened, group)
		}
	}
}
