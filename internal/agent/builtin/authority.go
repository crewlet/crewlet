package builtin

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Who may direct a seat's work, decided from the chart.
//
// # The rule
//
// The person tools that act ON A SEAT — pausing it, resuming it, steering the
// turn it is running — are a person's decision about somebody else's work, and
// the chart already says whose: the people a seat REPORTS TO, anybody whose
// line it is in ([org.Organization.LeadsInLine]), which is the same authority a
// lead holds over a report's queue and reads a report's day by. Past the chart
// there is one more: an ADMIN ([Actor.Admin]), who runs the engine and so may
// stop or redirect any of its agents. Nobody else — a member being a person in
// the company does not make every agent in it theirs to stop.
//
// # Why from the chart the call reads, once
//
// [org.Organization.LeadsInLine] finds a lead by POINTER, and every read of a
// live chart builds a new one, so the seat a tool resolved and the line it
// checks are read from ONE value — the tool's own — never a chart read once for
// the target and again for the line.
//
// # And why a refusal names who may
//
// A person told only "no" asks the wrong somebody next. Each refusal here names
// the people who could make the change — the PERSONS above the seat, nearest
// first, because an agent that manages it holds no tool that does — and "an
// admin", which a member's dashboard turns into the admin to ask.

// leadsSeat reports whether the person behind actor leads the seat handle in
// company: whether handle is in their line.
//
// THE PERSON, NEVER THE CREDENTIAL: a lead relation is between two seats in the
// chart, so it is asked of the seat the key is linked to ([Actor.Seat]) — a key
// nobody linked leads nobody, whatever it is named, and a token that happens
// to share a seat's handle is not that seat. These tools are a person's and no
// seat's registry holds them, so there is no in-turn actor to fall back to.
func leadsSeat(company *org.Organization, actor Actor, handle string) bool {
	lead := seatOf(company, strings.TrimSpace(actor.Seat))
	return lead != nil && slices.Contains(company.LeadsInLine(lead), handle)
}

// leadsOf is the PEOPLE a seat reports to, nearest first, by the name the chart
// gives them — the persons [leadsSeat] admits for that seat. An agent above it
// is left out: it holds none of the tools that ask this.
func leadsOf(company *org.Organization, seat *org.Role) []string {
	if company == nil || seat == nil {
		return nil
	}
	var out []string
	for _, lead := range company.Ancestors(seat) {
		if lead.IsHuman() {
			out = append(out, nameOf(lead))
		}
	}
	return out
}

// nameOf is how a refusal names a seat to a person: the name the chart gives
// it, or its handle where it has none.
func nameOf(seat *org.Role) string {
	if name := strings.TrimSpace(seat.Name); name != "" {
		return name
	}
	return seat.Handle()
}

// seatOf is the seat handle names in company, or nil — for a chart that is not
// loaded too, which a surface built before the company's first apply reads.
func seatOf(company *org.Organization, handle string) *org.Role {
	if company == nil {
		return nil
	}
	return company.SeatByHandle(handle)
}

// seatNamed is the chart's name for handle, or handle itself for a seat this
// chart does not have.
func seatNamed(company *org.Organization, handle string) string {
	if seat := seatOf(company, handle); seat != nil {
		return nameOf(seat)
	}
	return handle
}

// onlyWho is "Jane Founder or an admin", or "Tess Lead, Jane Founder, or an
// admin" — the parties a refusal names, with an admin always last because an
// admin may always. The serial comma past two, because a party may carry an
// "or" of its own ("whoever owns, filed or follows ENG-42"), and without it
// the last party reads as running into the admin.
func onlyWho(parties ...string) string {
	var named []string
	for _, party := range parties {
		if party = strings.TrimSpace(party); party != "" && !slices.Contains(named, party) {
			named = append(named, party)
		}
	}
	switch len(named) {
	case 0:
		return "an admin"
	case 1:
		return named[0] + " or an admin"
	}
	return strings.Join(named, ", ") + ", or an admin"
}

// followsTask reports whether any of a person's identities ([Actor.Party])
// owns, filed or follows the task: its assignee, its reporter, or a watcher
// who has not muted it.
//
// BOTH NAMES, because one person's rows carry two: the seat on what a colleague
// assigned them, the key's own id on what their own assistant filed — the
// reporter of a task a person created through the operator surface is the
// credential ([Actor.Handle]).
//
// MUTED IS NOT WATCHING. [tracker.Task.Watchers] is the set and
// [tracker.Task.Muted] the subtraction, and somebody who chose to stop
// following a task is not one of the people it is followed by.
func followsTask(task tracker.Task, identities []string) bool {
	for _, who := range identities {
		switch {
		case who == "":
			continue
		case task.Assignee == who, task.Reporter == who:
			return true
		case slices.Contains(task.Watchers, who) && !slices.Contains(task.Muted, who):
			return true
		}
	}
	return false
}
