package seataccess

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seatname"
)

// A seat's identity at a lane (ADR-0179): the document's setting
// ([Desired.Identity]), the ledger's record ([IdentityRecord]) and the
// resolver ([ResolveIdentity]), the same for every lane. A vendor that offers
// one says how in its [LaneSpec.Identity] and implements [IdentityMaker].

// IdentityMode is the document's choice of who signs a seat's calls on a lane.
// The values are stored (`identity` on a growth seat block).
type IdentityMode string

const (
	// IdentityOwn is the seat's own identity at the vendor.
	IdentityOwn IdentityMode = "own"
	// IdentityCompany is the company's identity, which every seat on it shares.
	IdentityCompany IdentityMode = "company"
)

// Valid reports whether m is a mode this build stores; empty is the lane's
// default ([IdentitySpec.Default]).
func (m IdentityMode) Valid() bool { return m == IdentityOwn || m == IdentityCompany }

// IdentityWay is how a seat's own identity is made. The values are the wire's.
type IdentityWay string

const (
	// WayHandedIn is an identity a person makes at the vendor and hands in
	// (an App Store Connect team key's file).
	WayHandedIn IdentityWay = "handed_in"
	// WayInvited is an identity Crewlet already made that a person adds at
	// the product (a Google growth identity added at Search Console).
	WayInvited IdentityWay = "invited"
	// WayMade is an identity Crewlet makes itself with the company's admin
	// access (a Meta system user).
	WayMade IdentityWay = "made"
)

// Valid reports whether w is a way this build words.
func (w IdentityWay) Valid() bool { return w == WayHandedIn || w == WayInvited || w == WayMade }

// IdentityStatus is where a seat's identity at a lane stands, in the one chip
// vocabulary every vendor's box draws. The values are the wire's and stored.
type IdentityStatus string

const (
	// IdentityCompanys is a seat whose calls go as the company's identity.
	IdentityCompanys IdentityStatus = "company"
	// IdentityNone is a seat set to its own identity that holds none yet:
	// not handed in, not added at the product, or not made.
	IdentityNone IdentityStatus = "none"
	// IdentityPreparing is a seat's own identity Crewlet is still making,
	// or made so lately that the vendor cannot find it yet: nothing a person
	// can add until it settles ([IdentityCheck.ReadyAt]).
	IdentityPreparing IdentityStatus = "preparing"
	// IdentityReady is a seat's own identity the last check found working,
	// at the role its level needs, in the company's team.
	IdentityReady IdentityStatus = "ready"
	// IdentityUnchecked is a seat's own identity the last check found
	// working, whose role the vendor shows to nobody Crewlet signs in as: it
	// is kept, and its role is said to be unverified, never in sync.
	IdentityUnchecked IdentityStatus = "unchecked"
	// IdentityWrongRole is an identity at a role that does not match the
	// seat's level: too narrow, or broader than Crewlet allows.
	IdentityWrongRole IdentityStatus = "wrong_role"
	// IdentityWrongTeam is an identity of another team, business or account.
	IdentityWrongTeam IdentityStatus = "wrong_team"
	// IdentityRefused is an identity the vendor refuses: revoked, its token
	// invalidated, removed from the product.
	IdentityRefused IdentityStatus = "refused"
	// IdentityReplacing is a seat's new identity working while the old one
	// waits to be revoked at the vendor.
	IdentityReplacing IdentityStatus = "replacing"
)

// IdentityStatuses is every status, in the chip set's order.
func IdentityStatuses() []IdentityStatus {
	return []IdentityStatus{IdentityCompanys, IdentityNone, IdentityPreparing, IdentityReady, IdentityUnchecked,
		IdentityWrongRole, IdentityWrongTeam, IdentityRefused, IdentityReplacing}
}

// Valid reports whether s is a status this build words.
func (s IdentityStatus) Valid() bool { return slices.Contains(IdentityStatuses(), s) }

// Label is the status as its chip says it. The dashboard's "Checking" is its
// own, drawn while a check runs, and never stored.
func (s IdentityStatus) Label() string {
	switch s {
	case IdentityCompanys:
		return "Company's"
	case IdentityNone:
		return "Not set up"
	case IdentityPreparing:
		return "Getting ready"
	case IdentityReady:
		return "Ready"
	case IdentityUnchecked:
		return "Role not checked"
	case IdentityWrongRole:
		return "Wrong role"
	case IdentityWrongTeam:
		return "Wrong team"
	case IdentityRefused:
		return "Refused"
	case IdentityReplacing:
		return "Replacing"
	}
	return string(s)
}

// works reports a status whose identity may sign calls.
func (s IdentityStatus) works() bool {
	return s == IdentityReady || s == IdentityReplacing || s == IdentityUnchecked
}

// Keeps reports a verdict whose identity is kept: it works, whether or not
// its role could be read.
func (s IdentityStatus) Keeps() bool { return s == IdentityReady || s == IdentityUnchecked }

// IdentitySpec is how a lane offers a seat its own identity, statically.
type IdentitySpec struct {
	// Ways are how the seat's own identity is made, the first the one the
	// sheet leads with; empty where the vendor offers no identity per seat,
	// and the company's is the lane's normal state (Apple Ads, AdSense,
	// AdMob).
	Ways []IdentityWay

	// Default is what an unset `identity` reads as.
	Default IdentityMode

	// Company is whether a seat may use the company's identity at all: false
	// where each seat's identity is its own by construction (a Google growth
	// identity, an Expo robot), so `identity` is never written there.
	Company bool

	// Name is the vendor's rule for the name Crewlet gives the identity.
	Name seatname.Rule

	// Standing is a lane whose identity Crewlet already made for every seat
	// (a Google growth identity), which a person only adds at the product:
	// every pass checks it and keeps it exactly while it passes, so nobody
	// comes back to ask, and one removed at the product reads Needs you again.
	Standing bool

	// Credential is the field the seat's own identity's credential is sealed
	// under ([SinkName]: `APP_STORE_CONNECT_KEY_<HANDLE>` for `KEY`), empty
	// for an identity with none to seal. Never one of [Catalog.Credentials],
	// and never put in a box (ADR-0170).
	Credential string

	// Retiring is the field prefix a superseded identity's credential stays
	// sealed under while a person revokes it at the vendor ([RetiringField]),
	// so each pass can read whether the vendor refuses it yet; empty where
	// none is kept.
	Retiring string
}

// RetiringField is the field superseded identity id stays sealed under on a
// lane with spec: one per identity, so a second replacement loses track of
// none. "" where the lane keeps none.
func RetiringField(spec IdentitySpec, id string) string {
	if spec.Retiring == "" || id == "" {
		return ""
	}
	return spec.Retiring + "_" + id
}

// IdentitySink is where a seat's own identity's credential is sealed on a lane
// with spec, "" where it seals none.
func IdentitySink(kind integration.Kind, spec IdentitySpec, handle string) string {
	if spec.Credential == "" {
		return ""
	}
	return SinkName(kind, spec.Credential, handle)
}

// Offered reports whether the vendor offers a seat an identity of its own.
func (s IdentitySpec) Offered() bool { return len(s.Ways) > 0 }

// Declared reports a lane that says who its vendor sees: its own identity
// offered, or the company's alone ([CompanyOnly]). A lane that declares
// nothing answers no identity.
func (s IdentitySpec) Declared() bool { return s.Offered() || s.Company }

// CompanyOnly is the spec of a lane whose vendor offers no identity per
// seat, so the company's is its normal state (Apple Ads, AdSense, AdMob).
var CompanyOnly = IdentitySpec{Default: IdentityCompany, Company: true}

// ModeOf is the mode d asks for on a lane with spec s: the company's where the
// vendor offers no own identity, the lane's default where d names none, and
// the seat's own where the vendor has no company identity to share.
func (s IdentitySpec) ModeOf(d Desired) IdentityMode {
	switch {
	case !s.Offered():
		return IdentityCompany
	case !s.Company:
		return IdentityOwn
	case d.Identity.Valid():
		return d.Identity
	case s.Default.Valid():
		return s.Default
	}
	return IdentityCompany
}

// IdentityRecord is what the ledger keeps of a seat's identity at one lane.
type IdentityRecord struct {
	// Way is how the kept identity was made; empty where none is kept.
	Way IdentityWay `json:"way,omitempty"`

	// ID is the vendor's identifier for the kept identity (a key ID, a
	// system user ID, an identity's email) and Name its name there; empty
	// where none is kept. Since is when Crewlet kept it.
	ID    string    `json:"id,omitempty"`
	Name  string    `json:"name,omitempty"`
	Since time.Time `json:"since,omitzero"`

	// Credential is where the kept identity's credential is sealed
	// ([SinkName]); nil for an identity with none (a Google identity, whose
	// tokens are minted at use). Never the value.
	Credential *Credential `json:"credential,omitempty"`

	// Check is what the last check of the kept identity read back, on every
	// pass and at keeping; nil where none is kept.
	Check *IdentityCheck `json:"check,omitempty"`

	// Attempt is the last identity a person handed in, or asked Crewlet to
	// make, that was NOT kept, and why: what the box says until a later one
	// is kept. Nil once one is.
	Attempt *IdentityCheck `json:"attempt,omitempty"`

	// Retiring are the identities a replacement or a drop superseded, which
	// a person revokes at the vendor, oldest first.
	Retiring []RetiringIdentity `json:"retiring,omitempty"`

	// Scope is what a standing identity was last checked against, which its
	// drop takes it off once the seat's own scope is gone (a seat turned off
	// with nothing left named, or removed).
	Scope []string `json:"scope,omitempty"`

	// Detail is the value handed in beside the kept identity that its calls
	// need ([GuideBack.Detail]: App Store Connect's vendor number for a Sales
	// key); never a credential. Empty where none was kept.
	Detail string `json:"detail,omitempty"`
}

// Waits reports a record a pass must keep: an identity kept, or one still
// waiting for its revoke.
func (r *IdentityRecord) Waits() bool { return r.Kept() || (r != nil && len(r.Retiring) > 0) }

// Kept reports a record that holds an identity of the seat's own.
func (r *IdentityRecord) Kept() bool { return r != nil && r.ID != "" }

// Status is where the record stands: the kept identity's last check, or, with
// none kept, the last attempt's verdict or [IdentityNone].
func (r *IdentityRecord) Status() IdentityStatus {
	switch {
	case r.Kept() && r.Check != nil && r.Check.Status.Valid():
		if r.Check.Status == IdentityReady && len(r.Retiring) > 0 {
			return IdentityReplacing
		}
		return r.Check.Status
	case r.Kept():
		// KEPT WITH NO CHECK READ BACK is never one that works.
		return IdentityRefused
	case r != nil && r.Attempt != nil && r.Attempt.Status.Valid() && r.Attempt.Status != IdentityReady:
		return r.Attempt.Status
	}
	return IdentityNone
}

// RetiringIdentity is a seat's former identity, superseded by a replacement,
// which a person revokes at the vendor (Crewlet cannot revoke an App Store
// Connect key). Crewlet clears it once the vendor refuses it.
type RetiringIdentity struct {
	ID    string    `json:"id"`
	Name  string    `json:"name,omitempty"`
	Since time.Time `json:"since,omitzero"`
}

// IdentityCheck is what one check of an identity read back.
type IdentityCheck struct {
	// Status is the verdict.
	Status IdentityStatus `json:"status"`

	// ID and Name are the identity the check proved, as the vendor names it.
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`

	// Role is the role the vendor holds it at, read back, and Needs the role
	// the seat's level needs, both in the vendor's words.
	Role  string `json:"role,omitempty"`
	Needs string `json:"needs,omitempty"`

	// Team is the team, business or account the identity belongs to.
	Team string `json:"team,omitempty"`

	// Reads are the reads the check made, in order, as a person reads them.
	Reads []IdentityRead `json:"reads,omitempty"`

	// Said is the verdict in the engine's words, and Remedy what to do.
	Said   string `json:"said,omitempty"`
	Remedy string `json:"remedy,omitempty"`

	// Broader is an identity whose role holds more than the level needs, which
	// the vendor's lane allows with a warning in Said.
	Broader bool `json:"broader,omitempty"`

	// Blocked is why the vendor cannot give the seat its own identity now (a
	// limit on how many it allows), in the engine's words: the seat's calls
	// stay on the company's identity and the box says why.
	Blocked string `json:"blocked,omitempty"`

	// Entries are the seat's scope entries as the check found its identity
	// on each, in the seat's order; nil where the vendor has no entries.
	Entries []IdentityEntry `json:"entries,omitempty"`

	// ReadyAt is when the vendor can find a [IdentityPreparing] identity, so
	// a person can add it; zero for any other status.
	ReadyAt time.Time `json:"ready_at,omitzero"`

	// RetiringGone are the record's retiring identities the vendor now
	// refuses: revoked, so the sync stops asking a person to revoke them.
	RetiringGone []string `json:"-"`

	// At is when the check ran.
	At time.Time `json:"at,omitzero"`
}

// IdentityEntry is one of a seat's scope entries as a check found its
// identity there, and where a person adds it.
type IdentityEntry struct {
	ID string `json:"id"`
	// Name is the vendor's name for it and Group what holds it (an
	// Analytics property's account); "" where neither was read.
	Name  string     `json:"name,omitempty"`
	Group string     `json:"group,omitempty"`
	State EntryState `json:"state"`
	// Role is the role read there, in the vendor's words.
	Role string `json:"role,omitempty"`
	// AddURL is the vendor's page that adds a user on the entry, and
	// GroupURL the one on what holds it; "" where it has none.
	AddURL   string `json:"add_url,omitempty"`
	GroupURL string `json:"group_url,omitempty"`
}

// EntryState is where a seat's identity stands on one scope entry.
type EntryState string

const (
	// EntryMissing is an entry the identity is not added to.
	EntryMissing EntryState = "missing"
	// EntryApplying is one the vendor shows it added to but does not let it
	// in yet.
	EntryApplying EntryState = "applying"
	// EntryAdded is one it reaches at the role its level needs.
	EntryAdded EntryState = "added"
	// EntryWrongRole is one it reaches at a role short of its level's.
	EntryWrongRole EntryState = "wrong_role"
	// EntryUnchecked is one it reaches whose role nobody could read.
	EntryUnchecked EntryState = "unchecked"
)

// IdentityRead is one read a check made: what it asked, and what it found.
type IdentityRead struct {
	Label string `json:"label"`
	OK    bool   `json:"ok"`
	Said  string `json:"said,omitempty"`
	// Detail is what the vendor answered the read's calls, a machine's line
	// (an operation, a status, the vendor's code), never a credential.
	Detail string `json:"detail,omitempty"`
}

// IdentityMaker is the session of a vendor that offers a seat its own
// identity ([IdentitySpec.Offered]): the calls the shared core makes to keep
// one, check it each pass, and give it up.
type IdentityMaker interface {
	// KeepIdentity proves an identity for the seat's lane and answers what to
	// keep: the credential a person handed in (WayHandedIn), an identity a
	// person added at the product (WayInvited), or one the vendor makes now
	// (WayMade). It checks before it answers, and an identity whose check
	// fails is answered as [IdentityKept.Check] with nothing to keep. A
	// [*Refusal] is the person's to fix; any other error is a fault.
	KeepIdentity(ctx context.Context, in IdentityInput) (IdentityKept, error)

	// CheckIdentity reads back the identity the seat keeps, with reads only:
	// alive, the right team, the role against the level, the reach against
	// the scope. Each pass, and after a call the vendor refused 401 or 403.
	CheckIdentity(ctx context.Context, in IdentityInput) (IdentityCheck, error)

	// DropIdentity gives up the identity the seat keeps, as its seat turns
	// off or back to the company's: what the vendor lets Crewlet undo it
	// undoes, and the rest it answers as a person's step.
	DropIdentity(ctx context.Context, in IdentityInput) (IdentityDropped, error)
}

// IdentityRefresher keeps a kept identity current each writing pass before its
// check: a credential near its end renewed, or a made identity brought to a
// changed level or scope. Nothing due answers nothing.
type IdentityRefresher interface {
	RefreshIdentity(ctx context.Context, in IdentityInput) (IdentityRefreshed, error)
}

// IdentityRefreshed is what a refresh made.
type IdentityRefreshed struct {
	// Minted is the credential to seal over the kept one, by field; empty
	// where nothing was due. A value that cannot be sealed is revoked.
	Minted []Minted

	// Retire revokes the credential Minted replaces, called once the new one
	// is sealed; nil where nothing is left to revoke.
	Retire func(ctx context.Context) error `json:"-"`
}

// IdentityGuider is a [Vendor] whose lane offers a seat its own identity: the
// words of the sheet that sets one up, PURE, so the tab draws it whether or
// not the vendor answers.
type IdentityGuider interface {
	IdentityGuide(c *config.Company, seat SeatRef, lane Lane, want Desired) IdentityGuide
}

// IdentityInput is one seat's lane as an [IdentityMaker] reads it.
type IdentityInput struct {
	Seat SeatRef
	Lane Lane
	Want Desired

	// Record is the seat's identity record, nil where it has none, and
	// Records every seat's record of the kind, so an identity another seat
	// keeps is refused.
	Record  *IdentityRecord
	Records map[string]Record

	// Way is how the person asked for it, on a KeepIdentity.
	Way IdentityWay

	// Value is what a person handed in (a .p8 key's contents, a token), and
	// FileName the name of the file it came in, which can carry the vendor's
	// ID for it (`AuthKey_<KEY ID>.p8`), and ID the vendor's ID where the
	// person named it. Never logged or echoed.
	Value    string `json:"-"`
	FileName string
	ID       string

	// Detail is the value handed in beside it where the guide asks for one
	// ([GuideBack.Detail]), "" where none was.
	Detail string

	// Sinks are the engine-owned names the seat's credentials are sealed
	// under, by field ([SinkNames]), for a session that resolves the kept
	// credential to check or revoke it.
	Sinks map[string]string
}

// IdentityKept is a KeepIdentity's answer.
type IdentityKept struct {
	// Check is the verdict. Only one that [IdentityStatus.Keeps] is kept.
	Check IdentityCheck

	// Minted is the credential to seal for the seat, by field; empty for an
	// identity with none. A value that cannot be sealed is revoked.
	Minted []Minted

	// Way is how the identity was made, ID and Name it as the vendor names it.
	Way      IdentityWay
	ID, Name string

	// Detail is the handed-in detail kept with the identity
	// ([IdentityRecord.Detail]), "" for none.
	Detail string

	// Retire revokes what this keep replaced, called once the new one is
	// sealed; nil, or one that fails, asks a person instead ([Retiring]).
	Retire func(ctx context.Context) error `json:"-"`
}

// IdentityDropped is what giving up an identity did.
type IdentityDropped struct {
	// Revoked are the fields whose sealed values are now dead, which the
	// sync clears.
	Revoked []string

	// Waiting is the step a person takes at the vendor (revoke the key at
	// Apple), nil where Crewlet undid everything.
	Waiting *Waiting

	// Minted is the given-up credential to keep sealed under its
	// [RetiringField] while a person revokes it, so a pass can read the
	// vendor refuse it; empty where nothing waits.
	Minted []Minted
}

// IdentityAsk is a person's ask about one seat's identity, carried by [Ask].
type IdentityAsk struct {
	// Keep is an identity to check and keep, with how it was made; Drop gives
	// up the kept one.
	Keep bool
	Drop bool
	Way  IdentityWay

	// Value, FileName, ID and Detail are what was handed in
	// ([IdentityInput]). Value is never logged or marshalled.
	Value    string `json:"-"`
	FileName string
	ID       string
	Detail   string
}

// Signer is who signs one seat's calls on a lane.
type Signer struct {
	// Own is the seat's own identity, Record its kept identity; the
	// company's identity otherwise.
	Own    bool
	Record *IdentityRecord
}

// IdentityRefusal is a seat whose own identity the vendor refused, or whose
// role or team no longer matches: its calls are refused until a person gives
// it a working one or sets it back to the company's. NEVER the company's in
// its place, or revoking one agent's key at the vendor would quietly hand it
// the company's.
type IdentityRefusal struct {
	Kind   integration.Kind
	Handle string
	Status IdentityStatus
	Said   string
}

func (e *IdentityRefusal) Error() string {
	said := e.Said
	if said == "" {
		said = "the vendor refused it"
	}
	return fmt.Sprintf("seataccess: @%s's own %s identity is %s: %s. Set up its identity again in the "+
		"integration's Agents tab, or set it back to the company's", e.Handle, KindName(e.Kind),
		strings.ToLower(e.Status.Label()), said)
}

// ResolveIdentity is who signs a seat's calls on lane: the seat's own where
// it keeps one that works, the company's where the vendor offers none, the
// seat is set to the company's, or it keeps none yet, and an
// [*IdentityRefusal] where the identity it keeps no longer works.
func ResolveIdentity(kind integration.Kind, spec IdentitySpec, handle string, want Desired,
	rec *IdentityRecord) (Signer, error) {

	if spec.ModeOf(want) == IdentityCompany {
		return Signer{}, nil
	}
	if !spec.Company {
		// AN IDENTITY OF ITS OWN BY CONSTRUCTION: the vendor holds it to its
		// role, and a refused call names it.
		return Signer{Own: true, Record: rec}, nil
	}
	if !rec.Kept() {
		return Signer{}, nil
	}
	if status := rec.Status(); !status.works() {
		var said string
		if rec.Check != nil {
			said = rec.Check.Said
		}
		return Signer{}, &IdentityRefusal{Kind: kind, Handle: handle, Status: status, Said: said}
	}
	return Signer{Own: true, Record: rec}, nil
}

// SeatRecords is the one ledger read the resolver needs.
type SeatRecords interface {
	SeatRecord(ctx context.Context, kind integration.Kind, handle string) (Record, bool, error)
}

// ResolveSeat is [ResolveIdentity] for seat handle on lane, its setting read
// from c and its record from the ledger: the call a growth product makes
// before each vendor call it signs for the seat.
func ResolveSeat(ctx context.Context, ledger SeatRecords, c *config.Company, lane Lane,
	handle string) (Signer, error) {

	kind := lane.Kind()
	v, ok := Lookup(kind)
	if !ok {
		return Signer{}, nil
	}
	spec, ok := v.Catalog().Lane(lane)
	if !ok || !spec.Identity.Offered() {
		return Signer{}, nil
	}
	var want Desired
	for _, seat := range SeatsOf(c) {
		if seat.Handle == handle {
			want, _ = DesiredOf(seat.Role, lane)
		}
	}
	if spec.Identity.ModeOf(want) == IdentityCompany {
		return Signer{}, nil
	}
	rec, held, err := ledger.SeatRecord(ctx, kind, handle)
	if err != nil {
		return Signer{}, fmt.Errorf("seataccess: read @%s's %s identity: %w", handle, kind, err)
	}
	var ir *IdentityRecord
	if held {
		ir = rec.Lanes[lane].Identity
	}
	return ResolveIdentity(kind, spec.Identity, handle, want, ir)
}

// ErrNoIdentity refuses an identity ask on a lane whose vendor offers none.
var ErrNoIdentity = errors.New("seataccess: this lane offers no identity per agent")
