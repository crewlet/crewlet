package iam

import "slices"

// Colleague is how far into the company's own work a principal reaches: the
// OTHER hat, beside [Grant].
//
// # Two hats, and why one set could not have been both
//
// A grant is authority over the DEPLOYMENT — the config document, the secret
// store, the fleet, the audit trail — and it is the same authority wherever
// the holder sits in the org chart. A colleague level is authority inside the
// COMPANY's work: filing an item, commenting on it, saving a page. The two are
// orthogonal, and every attempt to fold them together names the same pair of
// counterexamples:
//
//   - An auditor holds [GrantTranscriptRead] and [GrantConfigRead] over the
//     whole deployment and is nobody's colleague: they must not be
//     assignable, mentionable or delegable, because a seat that can be
//     delegated to is a seat an agent will delegate to.
//   - An SRE holds [ColleagueWrite] so they can file a bug against the team
//     that owns the service, and holds no grant at all: they change nothing
//     about the deployment.
//
// Collapsed into one ladder, the auditor either gains the ability to be
// assigned work or the SRE loses the ability to file it. So there are two
// fields and a principal carries both.
//
// # The zero is [ColleagueNone] and it is spelled out
//
// Unlike [Grant] and [Access], whose zero values are INVALID so a gate nobody
// filled in fails closed, this type's zero is a real and useful setting:
// most credentials are nobody's colleague. Making it invalid would mean every
// machine token in a config file had to say so, and a required field whose
// answer is almost always the same is one operators paste without reading.
//
// What keeps that safe is the DIRECTION of the zero: [ColleagueNone] is the
// closed end of the ladder, so the value somebody forgot to write grants
// nothing. That is the opposite of the `allow_anonymous_read` bool this
// vocabulary replaced, whose zero value was the open end — which is exactly
// why a false there could not survive an export round trip.
type Colleague string

const (
	// ColleagueNone is not a colleague at all: invisible to the roster,
	// to the colleague lookup, to a mention and to a delegation. An
	// operator credential, a CI pipeline, an auditor.
	//
	// INVISIBILITY IS THE POINT, not a side effect of holding no
	// permission. internal/agent/colleague builds its corpus from the
	// seats a turn could address, and a principal in that corpus is one
	// an executor may hand work to — so "can be found" and "can be given
	// work" are the same fact, and the only honest way to say no to the
	// second is to be absent from the first.
	ColleagueNone Colleague = "none"

	// ColleagueRead sees the company's work and changes none of it: the
	// board, the pages, an item's history. Addressable by a person and
	// visible in a roster, so a mention resolves — but never a target an
	// agent can assign to, because there is nothing it could then do.
	ColleagueRead Colleague = "read"

	// ColleagueWrite is an ordinary member of the company: files items,
	// comments, ranks, relates, depends, tags, creates and saves pages.
	// Every human seat the chart holds is this, and so is anybody who has
	// to be able to hand work to one.
	ColleagueWrite Colleague = "write"
)

// Colleagues are the three, in ladder order from closed to open.
var Colleagues = []Colleague{ColleagueNone, ColleagueRead, ColleagueWrite}

// Valid reports whether a level off the wire is one this build knows.
//
// AN UNKNOWN LEVEL IS NOT VALID AND IS NOT [ColleagueNone]. A newer peer's
// fourth rung is a level this build cannot place on its own ladder, and
// guessing either end is wrong in a way nothing would report: guessing the
// open end grants what was never asked for, and guessing the closed end makes
// a person silently vanish from every roster in the company. The readers below
// answer false, and a surface that needs to tell "no" from "cannot say" checks
// Valid first.
func (c Colleague) Valid() bool { return slices.Contains(Colleagues, c) }

// MayRead reports whether this level sees the company's work at all.
func (c Colleague) MayRead() bool {
	return c == ColleagueRead || c == ColleagueWrite
}

// MayWrite reports whether this level changes the company's work.
func (c Colleague) MayWrite() bool { return c == ColleagueWrite }

// AtMost clamps this level to the ceiling, which is what a delegated
// credential is bounded by: a personal access token may narrow its owner's
// level and may never widen it.
//
// AN UNKNOWN LEVEL ON EITHER SIDE CLAMPS TO [ColleagueNone], for the reason
// [Colleague.Valid] gives — a rung this build cannot place is one it cannot
// compare, and the safe answer to "how does this compare" is the closed end.
func (c Colleague) AtMost(ceiling Colleague) Colleague {
	rung := func(level Colleague) int { return slices.Index(Colleagues, level) }
	mine, theirs := rung(c), rung(ceiling)
	if mine < 0 || theirs < 0 {
		return ColleagueNone
	}
	if mine < theirs {
		return c
	}
	return ceiling
}
