package pages

import (
	"github.com/crewlet/crewlet/internal/seatnames"
)

// A PERSON IS WRITTEN AND READ BY THEIR SEAT'S IDENTITY, and shown by the
// handle the seat answers to now — internal/seatnames states the rule, and
// this file is where the knowledge base takes it.
//
// # What it cost here
//
// A seat's identity is the handle it was CREATED under (ADR-0026), and a rename
// moves only the address. This domain stored the handle a person answered to AT
// THE WRITE in every value that names somebody: a page's author, its watchers
// and its mutes, a revision's author, a remark's author and whom it mentions,
// the actor on every change. So once the chart renamed `cto` to `chief`, the
// seat could not edit or take down a remark it had written — "written by cto,
// and only its author may edit it" — its unwatch removed `chief` from a set
// that held `cto` and left it watching, a watch it made after the rename added
// it a second time, and a listing of the pages `chief` watches came back
// empty. The applier writes what the record carries, so every one of those
// values is the IDENTITY, resolved once by the store before any decide reads a
// row, and every answer shows it as the handle the seat answers to now.
//
// For a seat never renamed the identity IS its handle, so every row already
// written is keyed correctly and nothing is migrated. A name no seat answers
// to — a person's login, a Tier A token, a seat since removed — is kept exactly
// as written, both ways.

// Identities is the chart seam a person is written and read through.
//
// CONSUMER-DEFINED, and the same shape internal/tracker declares for its own:
// this package holds no org chart, and the engine answers from its live epoch.
// A READING rather than an answer: Pin is the chart as it stands now, held for
// everything one call names — see internal/seatnames, "One reading per call".
type Identities interface {
	Pin() seatnames.Chart
}

// people is this domain's walker over every value tagged `person:"seat"`. No
// arms: nothing this domain stores names a person in a shape a tag cannot say.
var people = seatnames.NewWalker()

// pinOf is one reading of a seam that may be nil — a build holding no chart,
// which records and shows every value as it was given.
func pinOf(ids Identities) seatnames.Chart {
	if ids == nil {
		return nil
	}
	return ids.Pin()
}

// identified is v with every person it names rewritten to their identity, by
// one reading of the chart.
func identified[T any](c seatnames.Chart, v T) T {
	return seatnames.Identified(people, c, v)
}

// shown is v with every person it names rewritten to the handle they answer to
// now, by one reading of the chart.
func shown[T any](c seatnames.Chart, v T) T {
	return seatnames.Shown(people, c, v)
}
