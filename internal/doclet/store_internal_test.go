package doclet

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY REFUSAL A SEAT IS TOLD TO ACT ON HAS A PRODUCER.
//
// The tool surface branches on [ErrTitleTaken], [ErrStaleVersion] and
// [ErrConflict] and tells the model something different for each — "that page
// exists, edit it", "re-base and save again", "somebody else is editing this,
// read it again". The conflict branch matched nothing for as long as the retry
// lived in the framework and this translation did not know about it: a write
// that lost sixteen races came back as the generic "did not land", which tells
// a model to report a failure rather than to re-read and retry.
//
// A test over values, because the producing condition is sixteen lost races.
func TestEveryRefusalACallerActsOnIsTranslated(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		in     error
		want   error
		framed bool // the framework's own sentinel survives the translation
		// named is whether the subject is in the error: a lost race names
		// it for the log, and a title held does not, because a title's
		// subject is its container and a DIGEST of the title, which says
		// nothing to the person who typed the title; that refusal says
		// what to do instead.
		named bool
	}{
		"a title somebody already holds": {
			in: fmt.Errorf("append: %w", statelog.ErrExists), want: ErrTitleTaken,
		},
		"a write that ran out of rounds": {
			in: fmt.Errorf("append: %w", statelog.ErrConflict), want: ErrConflict,
			framed: true, named: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := refusal(tc.in, statelog.Subject{Kind: string(KindTitle),
				ID: "ENG.deploy-runbook"})
			if !errors.Is(got, tc.want) {
				t.Fatalf("refusal(%v) = %v, which is not %v — the tool surface "+
					"branches on that sentinel and would fall through to the "+
					"answer that tells a model the change is simply lost",
					tc.in, got, tc.want)
			}
			if strings.Contains(got.Error(), "ENG.deploy-runbook") != tc.named {
				t.Errorf("the refusal names the subject = %v, want %v: %v",
					!tc.named, tc.named, got)
			}
			if framed := errors.Is(got, tc.in); framed != tc.framed {
				t.Errorf("the framework's own error survives = %v, want %v",
					framed, tc.framed)
			}
		})
	}
}

// AND NOTHING ELSE IS RESHAPED. An unavailable broker is not a refusal a
// person can act on, and translating it into one of the two above would tell a
// seat to re-read a page over a store it could not reach.
func TestARefusalThisPackageHasNoWordForTravelsUnchanged(t *testing.T) {
	t.Parallel()

	page := statelog.Subject{Kind: string(KindPage), ID: "ENG.runbook"}
	if got := refusal(nil, page); got != nil {
		t.Errorf("a write that landed came back as %v", got)
	}
	unreachable := fmt.Errorf("dial: %w", statelog.ErrUnavailable)
	got := refusal(unreachable, page)
	if !errors.Is(got, statelog.ErrUnavailable) {
		t.Fatalf("an unavailable log came back as %v", got)
	}
	for _, mistaken := range []error{ErrTitleTaken, ErrConflict, ErrStaleVersion} {
		if errors.Is(got, mistaken) {
			t.Errorf("an unavailable log reads as %v, so a caller acts on a "+
				"refusal the broker never made", mistaken)
		}
	}
}

// A REFUSAL ON THE LOG'S OWN SUBJECTS STAYS IN THE FRAMEWORK'S WORDS.
//
// A node's eviction gate, a reanchor's generation and a barrier are published
// through the same store as a page edit, and were translated alike: two
// operators contending over one node's gate were told "the page kept changing
// under this write: <node id>", about an edit nobody was making. A page, a
// title and a container are what a person edits, and only their refusals are
// a person's.
//
// EVERY KIND THE DOMAIN DECLARES IS CLASSED HERE, read from [ObjectKinds]: a
// kind added without a row is red until somebody says whether a person edits
// it, because [refusal] translates only the kinds it names and a new kind a
// person does edit would otherwise answer every lost race in the framework's
// words, which the tool surface does not branch on.
//
// Mutation: translate whatever the subject's kind and the log's own rows go
// red; translate nothing and the edited rows do; declare a seventh kind and
// it is named.
func TestARefusalOnTheLogsOwnSubjectsIsTheFrameworks(t *testing.T) {
	t.Parallel()
	// edited is whether a person edits a kind's objects, for every kind.
	edited := map[ObjectKind]bool{
		KindContainer: true, KindPage: true, KindTitle: true,
		KindEviction: false, KindGeneration: false, KindBarrier: false,
	}
	for kind := range edited {
		if !kind.Valid() {
			t.Errorf("%s has a row here and the domain declares no such kind", kind)
		}
	}
	for _, kind := range ObjectKinds {
		person, classed := edited[kind]
		if !classed {
			t.Errorf("%s is a kind the domain declares and this table does not class: "+
				"say whether a person edits it, and translate its refusals in "+
				"refusal() if one does", kind)
			continue
		}
		subject := statelog.Subject{Kind: string(kind), ID: "ENG.runbook"}
		for _, c := range []struct{ in, translated error }{
			{fmt.Errorf("append: %w", statelog.ErrConflict), ErrConflict},
			{fmt.Errorf("append: %w", statelog.ErrExists), ErrTitleTaken},
		} {
			got := refusal(c.in, subject)
			if person {
				if !errors.Is(got, c.translated) {
					t.Errorf("refusal(%v) on a %s subject = %v, want %v", c.in, kind,
						got, c.translated)
				}
				continue
			}
			// THE SAME ERROR IN THE SAME WORDS: wrapped in this package's,
			// it would still answer errors.Is and read as a page edit.
			if !errors.Is(got, c.in) || got.Error() != c.in.Error() {
				t.Errorf("refusal(%v) on a %s subject = %v, want the framework's own",
					c.in, kind, got)
			}
			if errors.Is(got, c.translated) {
				t.Errorf("a %s refusal reads as %v", kind, c.translated)
			}
		}
	}
}

// A PAGE WRITE'S SCOPE FOLLOWS THE PAGE, and a decide that finds it elsewhere
// sends the write back to read where it is.
//
// The request's scope is stated before the decide, so it is read outside it —
// and a page can arrive, or move, in between: the read found nothing and the
// decide found the page, or the read found one space and the decide another.
// Published anyway, the write would have probed a space the page is not in,
// which is the defect the read exists to remove. So the decide refuses a head
// in any other space than the one the request named, and the loop reads again.
func TestAPageWriteFollowsItsPageToTheSpaceItIsIn(t *testing.T) {
	t.Parallel()
	head := Page{ID: "page-a", Container: "ENG"}
	for _, stale := range []string{"", "PROD"} {
		if err := inContainer(head, stale); !errors.Is(err, errPageMoved) {
			t.Fatalf("a head in ENG against a request scoped to %q = %v, want "+
				"it refused as moved", stale, err)
		}
	}
	if err := inContainer(head, "ENG"); err != nil {
		t.Fatalf("a head in the space the request named = %v, want it accepted", err)
	}

	// THE LOOP: read, publish, and on a move read again.
	reads := []string{"", "ENG"}
	var published []string
	_, err := followMoves(
		func() (string, error) {
			next := reads[0]
			reads = reads[1:]
			return next, nil
		},
		func(container string) (statelog.Result, error) {
			published = append(published, container)
			return statelog.Result{}, inContainer(head, container)
		})
	if err != nil || len(published) != 2 || published[1] != "ENG" {
		t.Fatalf("a write whose page arrived after its first read published "+
			"under %v and answered %v, want a second publish scoped to ENG",
			published, err)
	}

	// AND IT IS BOUNDED: a page moving faster than a write can land is
	// answered as moved rather than chased for ever.
	var attempts int
	_, err = followMoves(
		func() (string, error) { return "PROD", nil },
		func(string) (statelog.Result, error) {
			attempts++
			return statelog.Result{}, inContainer(head, "PROD")
		})
	if !errors.Is(err, errPageMoved) || attempts != pageMoveAttempts {
		t.Fatalf("a page that kept moving was published %d times and answered "+
			"%v, want %d attempts and a move", attempts, err, pageMoveAttempts)
	}
}
