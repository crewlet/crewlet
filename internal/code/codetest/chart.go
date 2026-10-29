package codetest

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
)

// THE FIXTURE CHART every topology judges a seat by unless a case names its
// own: the unit ENG — the owner of every case's repository (Settings) — led by
// `lead`, holding the seats `alice` (full_access) and `bob` (read_only), and
// the human seat `pat`, whose person holds a token of the fleet's like every
// person's (Credentials). The company's clock is UTC and its identity domain
// unset (seats.invalid).

// The fixture chart's unit and seats.
const (
	FixtureUnit  = "ENG"
	FixtureLead  = "lead"
	FixtureAlice = "alice"
	FixtureBob   = "bob"
	FixturePat   = "pat"
)

// fixtureSeats is every seat of the fixture chart.
var fixtureSeats = map[string]code.RosterSeat{
	FixtureLead:  {Handle: FixtureLead, Tier: code.TierFullAccess, Unit: FixtureUnit},
	FixtureAlice: {Handle: FixtureAlice, Tier: code.TierFullAccess, Unit: FixtureUnit},
	FixtureBob:   {Handle: FixtureBob, Tier: code.TierReadOnly, Unit: FixtureUnit},
	FixturePat:   {Handle: FixturePat, Human: true, Unit: FixtureUnit},
}

// fixtureRoster is the fixture chart as the grant rules read it (code.Roster):
// a unit reference resolved as the chart resolves one, its case folded.
type fixtureRoster struct{}

// FixtureRoster is the fixture chart.
func FixtureRoster() code.Roster { return fixtureRoster{} }

func (fixtureRoster) Seat(handle string) (code.RosterSeat, bool) {
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

// FixtureSeatRules is the fixture chart and the company's clock as a data
// node's committer judges a seat's push by them (code.CommitterOptions), and
// its change writer a seat's write of a change's conversation and every
// reviewer an ask names (code.ChangeWriterOptions).
func FixtureSeatRules() (code.SeatRules, bool) {
	return code.SeatRules{Roster: FixtureRoster(), Zone: time.UTC}, true
}

// FixtureSigner signs an engine-built commit as the engine's signer does over
// a chart whose roles state no name of their own and no email address: the
// seat's handle as its author's name, `<handle>@seats.invalid` as its address,
// and the engine, `Crewlet <engine@seats.invalid>`, as its committer. A
// principal naming no seat of the fixture chart signs nothing, as a node whose
// chart holds no such seat signs nothing (code.ErrNotServing).
type FixtureSigner struct{}

// The fixture signer's committer, as the engine's own signs every commit with
// an unset identity domain.
const (
	FixtureCommitterName  = "Crewlet"
	FixtureCommitterEmail = "engine@" + config.DefaultCodeIdentityDomain
)

// Sign is the signature of a commit p makes.
func (FixtureSigner) Sign(p code.Principal) (host.Signature, error) {
	if _, ok := fixtureSeats[p.Seat]; !ok {
		return host.Signature{}, fmt.Errorf("%w: the fixture chart holds no seat %q to sign the commit as",
			code.ErrNotServing, p.Seat)
	}
	return host.Signature{AuthorName: p.Seat, AuthorEmail: FixtureAuthorEmail(p.Seat),
		CommitterName: FixtureCommitterName, CommitterEmail: FixtureCommitterEmail}, nil
}

// FixtureAuthorEmail is the address the fixture signer names a seat's commit
// by.
func FixtureAuthorEmail(seat string) string { return seat + "@" + config.DefaultCodeIdentityDomain }
