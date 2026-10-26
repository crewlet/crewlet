package routing_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/routing"
)

// reason is the closed reason set of a domain under test, in the shape every
// domain's has: a named string type the builder is instantiated over.
type reason string

const (
	mentioned    reason = "mention"
	assigned     reason = "assignee"
	watching     reason = "watcher"
	unblocked    reason = "unblocked"
	unwatched    reason = "unwatched"
	leadFallback reason = "lead_fallback"
)

// newsToTheActor is this domain's actor exception: a notice that something
// ELSE became workable is the one reason that reaches the person who made the
// change, as the tracker's `unblocked` is.
func newsToTheActor(r reason) bool { return r == unblocked }

// everyone is a registry that employs anybody.
func everyone(string) bool { return true }

// handlesOf is the candidates' handles, in the order they came back.
func handlesOf(cands []routing.Candidate[reason]) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Handle)
	}
	return out
}

// THE FIRST REASON THAT NAMES A HANDLE WINS, so the order a domain offers its
// reasons in is its precedence.
//
// A person who is mentioned AND assigned AND watching is told once that they
// were mentioned: the stronger fact, and the one they will act on. Without the
// add-once rule they would be told three times, and a notice row keyed on
// (record, recipient) would keep whichever reason happened to be written first
// rather than the one the precedence names. An inbox-only offer and a fallback
// are held to the rule like any other, and an empty handle — a snapshot field
// nothing filled — is nobody.
func TestTheFirstReasonThatNamesAHandleWins(t *testing.T) {
	t.Parallel()
	var b routing.Builder[reason]
	b.AddAll([]string{"ana", ""}, mentioned, true)
	b.Add("ana", assigned, true)
	b.Add("bo", assigned, true)
	b.AddAll([]string{"ana", "bo", "cy"}, watching, false)
	b.InboxOnly("cy", unwatched)
	b.Fallback(leadFallback, "bo")

	want := []routing.Candidate[reason]{
		{Handle: "ana", Reason: mentioned, Addressed: true},
		{Handle: "bo", Reason: assigned, Addressed: true},
		{Handle: "cy", Reason: watching},
	}
	if got := b.Candidates(); !slices.Equal(got, want) {
		t.Fatalf("the builder kept %+v, want %+v — one candidate per handle, "+
			"under the first reason that named it, in the order offered", got, want)
	}

	// AND NOTHING OFFERED IS NOBODY, never an empty list: a quiet record
	// concerns nobody on both surfaces alike.
	var quiet routing.Builder[reason]
	if got := quiet.Candidates(); got != nil {
		t.Fatalf("a builder offered nothing kept %+v", got)
	}
}

// A FALLBACK REACHES ONLY WHEN NOBODY ORDINARY SURVIVES, and then only the
// lowest-ranked one that does.
//
// Any ordinary survivor keeps the whole chain quiet: a fallback is for a change
// nobody would otherwise hear about. When every ordinary candidate is gone —
// the actor, a departed handle, an inbox-only record — the first link that
// survives hears, and only it. And the chain is why it is a list: a lead filing
// an unattended item into their own team is the first link and the actor, and
// with a single fallback the change would reach nobody at all.
func TestAFallbackReachesOnlyWhenNobodyOrdinarySurvives(t *testing.T) {
	t.Parallel()
	build := func(ordinary ...string) []routing.Candidate[reason] {
		var b routing.Builder[reason]
		b.AddAll(ordinary, watching, false)
		b.InboxOnly("ida", unwatched)
		b.Fallback(leadFallback, "unit-lead", "project-lead")
		return b.Candidates()
	}
	// employing is a roster that has lost the named handles.
	employing := func(left ...string) func(string) bool {
		return func(handle string) bool { return !slices.Contains(left, handle) }
	}

	for _, c := range []struct {
		name     string
		cands    []routing.Candidate[reason]
		registry func(string) bool
		actor    string
		want     []string
	}{
		{"an ordinary survivor keeps the chain quiet", build("ana"),
			everyone, "bo", []string{"ana"}},
		{"nobody ordinary: the first link hears, alone", build(),
			everyone, "bo", []string{"unit-lead"}},
		{"the actor and the departed are nobody", build("bo", "gone"),
			employing("gone"), "bo", []string{"unit-lead"}},
		{"the first link is the actor: the next one hears", build(),
			everyone, "unit-lead", []string{"project-lead"}},
		{"the first link has left: the next one hears", build(),
			employing("unit-lead"), "bo", []string{"project-lead"}},
		{"every link is gone: nobody", build(),
			employing("project-lead"), "unit-lead", nil},
		// THE RANK DECIDES, never the place in the slice a caller
		// assembled.
		{"the lowest rank, wherever it sits", []routing.Candidate[reason]{
			{Handle: "second", Reason: leadFallback, FallbackOnly: true, FallbackRank: 1},
			{Handle: "first", Reason: leadFallback, FallbackOnly: true, FallbackRank: 0},
		}, everyone, "bo", []string{"first"}},
	} {
		got := handlesOf(routing.Route(c.cands, c.registry, c.actor, newsToTheActor))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: woke %v, want %v", c.name, got, c.want)
		}
	}
}

// THE ROSTER IS ASKED ONLY ABOUT A HANDLE ROUTE COULD WAKE.
//
// What a registry is asked is what a domain counts and pays for: the knowledge
// base logs every handle its roster refused as a dropped recipient, and a code
// change's snapshot names hundreds of handles, each a lookup. So the actor is
// never asked — the record says who they are, and they are dropped whatever
// the roster answers — nor an inbox-only candidate, which is never woken; a
// fallback chain is not asked about while an ordinary candidate survives; and
// when none does, the chain is asked link by link in RANK order, only until
// one link is still employed. Asked first, the actor who has since left would
// be counted as a recipient dropped from a change that reached everyone it
// should, and so would a departed lead whose copy nobody needed.
func TestTheRosterIsAskedOnlyAboutAHandleThatCouldBeWoken(t *testing.T) {
	t.Parallel()
	build := func(ordinary ...string) []routing.Candidate[reason] {
		var b routing.Builder[reason]
		b.AddAll(ordinary, watching, false)
		b.InboxOnly("ida", unwatched)
		b.Fallback(leadFallback, "unit-lead", "project-lead", "vp")
		return b.Candidates()
	}
	var repair routing.Builder[reason]
	repair.Add("ana", unblocked, false)

	for _, c := range []struct {
		name  string
		cands []routing.Candidate[reason]
		left  []string // the handles the roster no longer has
		actor string
		woke  []string
		asked []string
	}{
		{"an ordinary survivor: neither the actor nor the chain is asked",
			build("bo", "ana", "cy"), []string{"bo"}, "bo",
			[]string{"ana", "cy"}, []string{"ana", "cy"}},
		{"nobody ordinary: the chain is asked until a link is employed",
			build("bo"), []string{"unit-lead"}, "bo",
			[]string{"project-lead"}, []string{"unit-lead", "project-lead"}},
		{"a departed ordinary candidate is asked, and then the chain",
			build("gone"), []string{"gone"}, "bo",
			[]string{"unit-lead"}, []string{"gone", "unit-lead"}},
		{"a link that is the actor is passed over unasked",
			build(), nil, "unit-lead",
			[]string{"project-lead"}, []string{"project-lead"}},
		{"every link gone: each is asked once, in rank order",
			build(), []string{"unit-lead", "project-lead", "vp"}, "bo",
			nil, []string{"unit-lead", "project-lead", "vp"}},
		// THE RANK ORDERS THE ASKING, never the place in the slice a
		// caller assembled.
		{"in rank order, wherever a link sits", []routing.Candidate[reason]{
			{Handle: "second", Reason: leadFallback, FallbackOnly: true, FallbackRank: 1},
			{Handle: "first", Reason: leadFallback, FallbackOnly: true, FallbackRank: 0},
		}, []string{"first"}, "bo",
			[]string{"second"}, []string{"first", "second"}},
		// AND THE ACTOR IS ASKED about where a reason wakes them, because
		// there they are somebody Route could wake.
		{"the actor under a reason that wakes them is a recipient like any",
			repair.Candidates(), nil, "ana",
			[]string{"ana"}, []string{"ana"}},
	} {
		var asked []string
		registry := func(handle string) bool {
			asked = append(asked, handle)
			return !slices.Contains(c.left, handle)
		}
		got := handlesOf(routing.Route(c.cands, registry, c.actor, newsToTheActor))
		if !slices.Equal(got, c.woke) {
			t.Errorf("%s: woke %v, want %v", c.name, got, c.woke)
		}
		if !slices.Equal(asked, c.asked) {
			t.Errorf("%s: the roster was asked about %v, want %v — only a "+
				"handle Route could wake is a question worth asking", c.name,
				asked, c.asked)
		}
	}
}

// A FALLBACK'S RANK IS ITS PLACE IN THE CHAIN, counted across every chain the
// builder was offered. An empty link and a link an earlier reason already
// named add nothing and keep their places, so the next lead is ranked by where
// the domain put it rather than by how many links before it happened to be
// filled in.
func TestAFallbacksRankIsItsPlaceInTheChain(t *testing.T) {
	t.Parallel()
	var b routing.Builder[reason]
	b.Add("ana", watching, false)
	b.Fallback(leadFallback, "", "ana", "project-lead")
	b.Fallback(leadFallback, "vp")

	want := []routing.Candidate[reason]{
		{Handle: "ana", Reason: watching},
		{Handle: "project-lead", Reason: leadFallback, FallbackOnly: true, FallbackRank: 2},
		{Handle: "vp", Reason: leadFallback, FallbackOnly: true, FallbackRank: 3},
	}
	if got := b.Candidates(); !slices.Equal(got, want) {
		t.Fatalf("the builder kept %+v, want %+v", got, want)
	}
}

// THE ACTOR HEARS ONLY UNDER A REASON THAT WAKES THEM.
//
// Telling somebody what they just did is a round a model spends on nothing,
// and a notice in a person's own inbox about their own write — so the actor is
// dropped under every reason but those their domain says are news to them, and
// under every reason when it names none. Such a reason rides a record of its
// own, as the tracker's unblocked repair does — a commit on the task that
// became workable, naming its assignee and nobody else — and that record is
// the one built here. The predicate is ONE function,
// [routing.Candidate.Hears], because an applier writing notice rows takes this
// step with no roster and the wake takes it with one: two copies would be two
// answers for one record.
func TestTheActorHearsOnlyUnderAReasonThatWakesThem(t *testing.T) {
	t.Parallel()
	var own routing.Builder[reason]
	own.Add("ana", assigned, true)
	own.Add("bo", watching, false)
	if got := handlesOf(routing.Route(own.Candidates(), everyone, "ana", newsToTheActor)); !slices.Equal(got, []string{"bo"}) {
		t.Fatalf("ana's own change woke %v, want bo alone — she is told "+
			"nothing she just did", got)
	}

	// THE REPAIR'S RECORD REACHES ANA THOUGH SHE WROTE IT: that a task of
	// hers became workable is news whoever writes it down.
	var repair routing.Builder[reason]
	repair.Add("ana", unblocked, false)
	want := []routing.Candidate[reason]{{Handle: "ana", Reason: unblocked}}
	if got := routing.Route(repair.Candidates(), everyone, "ana", newsToTheActor); !slices.Equal(got, want) {
		t.Fatalf("ana's own unblocked record woke %+v, want %+v — the actor "+
			"hears the notice that is news to them", got, want)
	}

	// A DOMAIN THAT NAMES NO SUCH REASON wakes its actor under none.
	if got := routing.Route(repair.Candidates(), everyone, "ana", nil); len(got) != 0 {
		t.Fatalf("with no reason waking the actor, ana's own record woke %+v", got)
	}

	// AND THE APPLIER'S STEP IS THE SAME PREDICATE, asked with no roster.
	for _, c := range slices.Concat(own.Candidates(), repair.Candidates()) {
		wanted := c.Handle != "ana" || c.Reason == unblocked
		if got := c.Hears("ana", newsToTheActor); got != wanted {
			t.Errorf("%s under %q hears ana's change = %v, want %v", c.Handle,
				c.Reason, got, wanted)
		}
	}
}

// AN INBOX-ONLY CANDIDATE IS A RECORD, NEVER A WAKE: it is written to its
// recipient's inbox and Route never returns it. It claims its handle like any
// other offer, so a later reason for the same person adds nothing — the person
// whose watch a lead removed reads that, rather than being woken as a watcher.
func TestAnInboxOnlyCandidateIsARecordAndNeverAWake(t *testing.T) {
	t.Parallel()
	var b routing.Builder[reason]
	b.InboxOnly("ana", unwatched)
	b.Add("ana", watching, false)
	cands := b.Candidates()

	want := []routing.Candidate[reason]{{Handle: "ana", Reason: unwatched, InboxOnly: true}}
	if !slices.Equal(cands, want) {
		t.Fatalf("the builder kept %+v, want %+v", cands, want)
	}
	if got := routing.Route(cands, everyone, "lead", newsToTheActor); len(got) != 0 {
		t.Fatalf("an inbox-only candidate was woken: %+v", got)
	}
}

// A HANDLE THE COMPANY NO LONGER EMPLOYS IS DROPPED AT ROUTE, NOT AT THE
// BUILDER. The candidates are the complete record of whom a change concerned —
// what an applier writes rows from, with no roster — and only the wake goes to
// the people still there. A nil registry admits everybody: a deployment with
// no organization loaded has nobody to judge by.
func TestADepartedHandleKeepsItsRecordAndLosesItsWake(t *testing.T) {
	t.Parallel()
	var b routing.Builder[reason]
	b.AddAll([]string{"ana", "gone"}, watching, false)
	cands := b.Candidates()
	if got := handlesOf(cands); !slices.Equal(got, []string{"ana", "gone"}) {
		t.Fatalf("the record of whom the change concerned is %v", got)
	}

	employed := func(handle string) bool { return handle != "gone" }
	if got := handlesOf(routing.Route(cands, employed, "bo", nil)); !slices.Equal(got, []string{"ana"}) {
		t.Fatalf("woke %v, and a departed handle is nobody to wake", got)
	}
	if got := handlesOf(routing.Route(cands, nil, "bo", nil)); !slices.Equal(got, []string{"ana", "gone"}) {
		t.Fatalf("with no registry the change woke %v, want everybody it concerned", got)
	}
}
