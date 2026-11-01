// Package routing is who a committed record concerns, and which of them it
// wakes: the one rule every native domain routes its notices by. The work
// tracker and the knowledge base each declare their own reasons for telling
// somebody about a change; this package is what those reasons are applied
// by, so "who hears about this, and why" has one answer however many domains
// ask it, rather than a copy per domain that drifts.
//
// # Two functions, and why the answer is never one
//
// A domain's candidate function offers its record's mentions and routing
// snapshot to a [Builder]. Where the domain's APPLIER writes a notice row per
// candidate, as the tracker's does, that function is PURE OVER ITS RECORD:
// it reads no clock, no organization, no registry and no actor, because the
// applier calls it inside its transaction, where the determinism rules
// forbid anything a second data node could answer differently, and the wake
// path calls it again on another node minutes later and must get the same
// set.
//
// [Route] is the post-filter, and it needs the ONE fact a record cannot
// carry: who the company employs RIGHT NOW. Who made the change IS on the
// record, which is why an applier takes Route's actor step too
// ([Candidate.Hears]) and leaves only the roster to the wake. Splitting the
// roster out is what makes an applier's notice rows a complete record of
// everyone a change concerned but its author, while the wake goes only to
// the people still there.
//
// # One reason per handle, and the order is the precedence
//
// The first reason that names a handle is the one that handle hears under,
// and every later offer of the same handle is dropped — [Builder]'s add-once
// rule. A person who is mentioned AND watching is told they were mentioned:
// the stronger fact, and the one they will act on. So the order a domain
// OFFERS its reasons in IS its precedence, decided in the one function that
// offers them.
//
// A domain that also declares its reasons as a LIST in that order, for the
// surfaces that print and filter by reason (the tracker's Reasons is one),
// has written the precedence twice. It holds the list to its candidate
// function with a test: for every kind it routes, with a different handle in
// every field it routes from, the candidates come back in the list's order.
// A reordered arm and a reordered list then both fail. A list held only by a
// comment saying it matches drifts, and nothing fails.
//
// EVERY OFFER IS HELD TO THE RULE, a notice about another subject than the
// record's own included, because a record reaches one handle ONCE in both
// places that keep what it reached: a domain's notice row is keyed on the
// record and the recipient, and a wake's id is derived from the same two, so
// a second notice to one person on one record collapses into the first and
// is never delivered. A domain with news for somebody about ANOTHER subject
// writes it on a record of that subject's own, which reaches them under its
// own reason and answers for itself afterwards: the tracker's unblocked
// repair tells a dependent's assignee in a commit on the dependent, not on
// the task whose close freed it. A domain whose ONE record carries several
// subjects' news by construction — the code host's, which folds pushes from
// unrelated clients into one record and lands several queue entries in one —
// keys both on the subject too, and offers each subject's news to a Builder
// of its own (internal/code's Concerns and NoticeID).
//
// # The fallback is a chain
//
// A domain offers a lead to fall back on when nobody ordinary hears a change,
// and offers it as an ORDERED LIST ([Builder.Fallback]) rather than one
// handle, for one common case: a lead filing an unattended item into their
// own team. With one candidate, the actor drop removes it and the change
// reaches NOBODY; with the chain, the unit's lead is dropped as the actor and
// the next lead hears. And only the lowest-ranked fallback that survives is
// woken, never the whole chain: waking every lead in it for every unattended
// change is how a fallback becomes something people filter out.
//
// # Reasons stay the domain's
//
// This package declares no reason. A domain's reasons are its own closed set
// — a named string type with a Valid method — and [Candidate] is generic over
// that type, so a domain's [Builder] accepts only its own reasons, and a
// candidate's reason is still that type when the domain's own predicates
// read it back: which reasons wake the actor, which oblige an answer. A
// reason field of plain string would accept any string at the offer and need
// a conversion at every read. The knowledge base is the degenerate case: two
// reasons, a one-link chain, and no reason that wakes its actor — and,
// writing no notice rows, it takes that one link from the org chart its
// parser holds rather than from the record.
//
// # Who a record reached, as a reader can say it afterwards
//
// A domain that keeps one row per recipient can be asked, record by record,
// who each one reached. An EMPTY answer is three facts rather than one — the
// routing resolved to nobody, a retention sweep took the rows, or there is
// nothing to date the absence against — and each sends a reader somewhere
// different, so [DeliveryOf] answers a [Delivery] rather than a boolean.
//
// # Values only
//
// The standard library and nothing else. A domain's applier runs a [Builder]
// inside its transaction and every domain imports this package, so it opens
// no store, reads no clock and no configuration, and holds no roster. The
// one thing that would differ between two nodes, the roster, is passed in by
// the caller that has it, as [Route]'s registry; the actor passed beside it
// is the record's own, the same on every node that reads the record.
package routing

import (
	"cmp"
	"slices"
)

// Candidate is one handle a record concerns, and why.
//
// R is the domain's own reason type; see the package doc for why it is a
// parameter rather than a string.
type Candidate[R ~string] struct {
	// Handle is the seat the notice is for, in the spelling a record's
	// actor is written in — a person writes as the seat they hold
	// (iam.ActorFor) — which is what lets [Route] compare the two.
	Handle string

	// Reason is the one reason this handle hears under: the first its
	// domain offered for it.
	Reason R

	// Addressed marks a notice that asks something of its recipient — a
	// turn that must answer rather than absorb. The domain decides it per
	// reason and per record: a bulk gesture, for one, addresses nobody.
	Addressed bool

	// FallbackOnly marks a candidate kept only when no ordinary one
	// survives [Route], and FallbackRank is its place in the chain
	// [Builder.Fallback] offered, which orders those among themselves.
	FallbackOnly bool
	FallbackRank int

	// InboxOnly marks a candidate written to the inbox and never woken.
	InboxOnly bool
}

// Hears reports whether c is told of a change actor made.
//
// Everybody but the actor is. The actor is told only under a reason
// wakesActor says is news to them — the tracker's `unblocked` tells somebody
// a task of theirs became workable, which is news whoever wrote the record
// saying so — and a nil wakesActor names no such reason. Otherwise telling
// somebody what they just did is a round a model spends on nothing, and a
// notice in a person's own inbox about their own write.
//
// THE ONE STEP OF [Route] A RECORD CAN TAKE ON ITS OWN: who wrote the record
// is on the record, so an applier writing notice rows — which holds no roster
// and so takes none of Route's other steps — asks this too, and a row is
// written for the change's own author only under a reason that is news to
// them.
func (c Candidate[R]) Hears(actor string, wakesActor func(R) bool) bool {
	return c.Handle != actor || (wakesActor != nil && wakesActor(c.Reason))
}

// Builder collects a record's candidates in its domain's precedence order:
// the first offer that names a handle is the one it hears under (see the
// package doc). The zero value is ready to use, and one builder serves one
// record.
type Builder[R ~string] struct {
	out   []Candidate[R]
	named map[string]bool

	// ranks is how many places [Builder.Fallback] has handed out.
	ranks int
}

// Add offers handle under reason. An empty handle, and a handle an earlier
// offer already named, add nothing — the empty one because a snapshot field
// nothing filled is an empty handle, and a reason whose field went unfilled
// must route to nobody rather than to a recipient called "".
func (b *Builder[R]) Add(handle string, reason R, addressed bool) {
	b.claim(Candidate[R]{Handle: handle, Reason: reason, Addressed: addressed})
}

// AddAll offers each handle in turn, as [Builder.Add] does one.
func (b *Builder[R]) AddAll(handles []string, reason R, addressed bool) {
	for _, handle := range handles {
		b.Add(handle, reason, addressed)
	}
}

// InboxOnly offers handle as a candidate written to the inbox and never
// woken: the notice of a decision somebody else made about them, which a
// person reads and a seat is not turned for.
//
// IT CLAIMS ITS HANDLE like any other offer, so a stronger reason offered
// earlier keeps it, and nothing offered later outranks it.
func (b *Builder[R]) InboxOnly(handle string, reason R) {
	b.claim(Candidate[R]{Handle: handle, Reason: reason, InboxOnly: true})
}

// Fallback offers chain as the ordered fallback, under reason.
//
// A HANDLE'S RANK IS ITS PLACE IN THE CHAIN — counted across every Fallback
// call on this builder — and an empty link or an already-named one keeps its
// place while adding nothing, so the second lead is ranked second whether or
// not there is a first. [Route] keeps the lowest-ranked fallback that
// survives, and only when no ordinary candidate did.
func (b *Builder[R]) Fallback(reason R, chain ...string) {
	for _, handle := range chain {
		rank := b.ranks
		b.ranks++
		b.claim(Candidate[R]{
			Handle: handle, Reason: reason, FallbackOnly: true, FallbackRank: rank,
		})
	}
}

// Candidates is every candidate the builder kept, in the order offered — which
// is the domain's precedence. Nil when nothing was kept.
func (b *Builder[R]) Candidates() []Candidate[R] { return b.out }

// claim adds c unless its handle is empty or already named — the add-once
// rule every offer is held to.
func (b *Builder[R]) claim(c Candidate[R]) {
	if c.Handle == "" || b.named[c.Handle] {
		return
	}
	if b.named == nil {
		b.named = map[string]bool{}
	}
	b.named[c.Handle] = true
	b.out = append(b.out, c)
}

// Route is the post-filter: which of a record's candidates are woken.
//
// Four steps, in this order, and the last one is the whole reason the
// fallback is a chain:
//
//  1. Drop the inbox-only candidates — they are a record, not a wake.
//  2. Drop the actor, except under a reason wakesActor says is news to them
//     ([Candidate.Hears]). wakesActor is the domain's, because which of its
//     reasons are news to the person who made the change is a fact about
//     those reasons.
//  3. Drop handles the company no longer employs. registry answers whether
//     it still does, and a nil registry admits everybody: the honest answer
//     for a deployment with no organization loaded yet. A departed handle
//     keeps its notice row, written before anybody asked, and loses only its
//     wake — a wake addressed to nobody is one nothing reports.
//  4. If no ordinary candidate survived, keep the LOWEST-RANKED fallback the
//     company still employs, and only that one.
//
// THE REGISTRY IS ASKED ONLY ABOUT A HANDLE ROUTE COULD WAKE: never the
// actor, never an inbox-only candidate, never a fallback while an ordinary
// candidate survives, and the chain link by link in rank order, stopping at
// the first link still employed. Steps 2 and 3 are both filters, so taking
// the record's own step first changes no answer — it changes what the roster
// is asked, and that is what a domain counts and pays for: the knowledge base
// logs every handle the roster refused as a dropped recipient, which asked
// first would count an actor who has since left, or a departed lead whose
// copy nobody needed, from a change that reached everyone it should; and a
// snapshot of hundreds of handles costs a lookup per would-be recipient
// rather than per name.
//
// ONE NAME IS THE ACTOR. A person bound to a seat writes AS that seat — the
// author iam.ActorFor records for them — so the handle their own gestures
// land on is the author itself, and a credential nobody is bound through
// writes, and is watched, under its own login.
func Route[R ~string](candidates []Candidate[R], registry func(string) bool,
	actor string, wakesActor func(R) bool) []Candidate[R] {

	employed := func(handle string) bool { return registry == nil || registry(handle) }
	var ordinary, fallback []Candidate[R]
	for _, c := range candidates {
		if c.InboxOnly || !c.Hears(actor, wakesActor) {
			continue
		}
		if c.FallbackOnly {
			// NOT ASKED ABOUT YET: a link is a question only once nobody
			// ordinary survives, which the loop has not finished saying.
			fallback = append(fallback, c)
			continue
		}
		if employed(c.Handle) {
			ordinary = append(ordinary, c)
		}
	}
	if len(ordinary) > 0 {
		return ordinary
	}
	// THE RANK DECIDES, never the place in the slice a caller assembled —
	// stable, so two links a caller gave one rank keep the order it gave.
	slices.SortStableFunc(fallback, func(a, b Candidate[R]) int {
		return cmp.Compare(a.FallbackRank, b.FallbackRank)
	})
	for _, c := range fallback {
		if employed(c.Handle) {
			return []Candidate[R]{c}
		}
	}
	return nil
}
