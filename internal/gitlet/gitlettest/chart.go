package gitlettest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/iam"
)

// The fixture chart every topology judges a seat by unless a case names its
// own. Its unit owns every case's repository (Settings); the human seat pat's
// person holds a fleet token like every person's (Credentials). The company's
// clock is UTC and its identity domain unset (seats.invalid).

// The fixture chart's unit and seats.
const (
	FixtureUnit  = "ENG"
	FixtureLead  = "lead"
	FixtureAlice = "alice"
	FixtureBob   = "bob"
	FixturePat   = "pat"
)

// fixtureSeats is every seat of the fixture chart.
var fixtureSeats = map[string]gitlet.RosterSeat{
	FixtureLead:  {Handle: FixtureLead, Tier: gitlet.TierFullAccess, Unit: FixtureUnit},
	FixtureAlice: {Handle: FixtureAlice, Tier: gitlet.TierFullAccess, Unit: FixtureUnit},
	FixtureBob:   {Handle: FixtureBob, Tier: gitlet.TierReadOnly, Unit: FixtureUnit},
	FixturePat:   {Handle: FixturePat, Human: true, Unit: FixtureUnit},
}

// fixtureRoster is the fixture chart as the grant rules read it (gitlet.Roster):
// a unit reference resolved as the chart resolves one, its case folded.
type fixtureRoster struct{}

// FixtureRoster is the fixture chart.
func FixtureRoster() gitlet.Roster { return fixtureRoster{} }

func (fixtureRoster) Seat(handle string) (gitlet.RosterSeat, bool) {
	s, ok := fixtureSeats[handle]
	return s, ok
}

func (fixtureRoster) Lead(unit string) string {
	if strings.EqualFold(unit, FixtureUnit) {
		return FixtureLead
	}
	return ""
}

func (r fixtureRoster) Leads(unit string) []string {
	if lead := r.Lead(unit); lead != "" {
		return []string{lead}
	}
	return nil
}

func (fixtureRoster) Within(unit, ancestor string) bool {
	return strings.EqualFold(unit, FixtureUnit) && strings.EqualFold(ancestor, FixtureUnit)
}

// FixtureRosterWithout is the fixture chart with seat taken out of it: the
// chart a revision that revoked the seat leaves (Cluster.Revoke).
func FixtureRosterWithout(seat string) gitlet.Roster { return revokedRoster{seat: seat} }

// revokedRoster is the fixture chart without one seat — the unit it led, if
// it led one, then led by nobody.
type revokedRoster struct {
	fixtureRoster
	seat string
}

func (r revokedRoster) Seat(handle string) (gitlet.RosterSeat, bool) {
	if handle == r.seat {
		return gitlet.RosterSeat{}, false
	}
	return r.fixtureRoster.Seat(handle)
}

func (r revokedRoster) Lead(unit string) string {
	if lead := r.fixtureRoster.Lead(unit); lead != r.seat {
		return lead
	}
	return ""
}

func (r revokedRoster) Leads(unit string) []string {
	if lead := r.Lead(unit); lead != "" {
		return []string{lead}
	}
	return nil
}

// FixtureSeatRules is the fixture chart and the company's clock as a data
// node's committer judges a seat's push by them (gitlet.CommitterOptions), and
// its change writer a seat's write of a change's conversation and every
// reviewer an ask names (gitlet.ChangeWriterOptions).
func FixtureSeatRules() (gitlet.SeatRules, bool) {
	return gitlet.SeatRules{Roster: FixtureRoster(), Zone: time.UTC}, true
}

// FixtureSigner signs an engine-built commit as the engine's signer does when
// roles state no name or email: the seat's handle as author name,
// `<handle>@seats.invalid` as its address, `Crewlet <engine@seats.invalid>` as
// committer. A principal naming no fixture seat signs nothing, as a node whose
// chart lacks the seat does (gitlet.ErrNotServing).
type FixtureSigner struct{}

// The fixture signer's committer, as the engine's own signs every commit with
// an unset identity domain.
const (
	FixtureCommitterName  = "Crewlet"
	FixtureCommitterEmail = "engine@" + config.DefaultCodeIdentityDomain
)

// Sign is the signature of a commit p makes.
func (FixtureSigner) Sign(p gitlet.Principal) (host.Signature, error) {
	if _, ok := fixtureSeats[p.Seat]; !ok {
		return host.Signature{}, fmt.Errorf("%w: the fixture chart holds no seat %q to sign the commit as",
			gitlet.ErrNotServing, p.Seat)
	}
	return host.Signature{AuthorName: p.Seat, AuthorEmail: FixtureAuthorEmail(p.Seat),
		CommitterName: FixtureCommitterName, CommitterEmail: FixtureCommitterEmail}, nil
}

// Author names any seat handle as a commit's author the way Sign names an
// acting one, a handle the chart does not hold too, as the engine's signer
// names a seat that left.
func (FixtureSigner) Author(handle string) (string, string, error) {
	return handle, FixtureAuthorEmail(handle), nil
}

// Committer is the address the fixture signer commits as, which a coding box's
// pack may hold no new commit of.
func (FixtureSigner) Committer() (string, error) { return FixtureCommitterEmail, nil }

// FixtureAuthorEmail is the address the fixture signer names a seat's commit
// by.
func FixtureAuthorEmail(seat string) string { return seat + "@" + config.DefaultCodeIdentityDomain }

// FixturePeople is the identity directory the fixture's change writers ask who
// holds a human seat (gitlet.People): every seat's holder is an active person
// using every app, so a review ask naming a person is decided by the chart
// alone unless a case names its own people.
type FixturePeople struct{}

// Requester is seat's holder: an active person holding every app.
func (FixturePeople) Requester(_ context.Context, seat string) (iam.Principal, error) {
	return iam.Principal{Kind: iam.KindPerson, Seat: seat, Stage: iam.StageActive,
		Grants: slices.Clone(iam.Apps)}, nil
}
