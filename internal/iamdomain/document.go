package iamdomain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jsoncarry"
)

// The typed payloads, one per (subject kind, op) that writes rows. Each is
// FULL POST-STATE, never a patch: a person is submitted whole, as a form, so a
// patch would be a diff every data node reassembles, and the objects are small.
//
// A name or an address is ALWAYS SEALED, at the writer under the fleet keyring
// ([Sealer]), because the record lives on the broker and in every deferred
// table, snapshot and backup. A blind is always the matched form, never the
// address it came from, which the estate never holds. Every document carries
// its own version and an Extra map, so a newer build's field round-trips
// ([jsoncarry]).

// DocumentVersion is the document shape THIS BUILD writes.
const DocumentVersion = 1

// ErrUnknownVersion reports a document a newer build wrote.
type ErrUnknownVersion struct{ Got, Want int }

func (e ErrUnknownVersion) Error() string {
	return fmt.Sprintf("iamdomain: document version %d is above this build's "+
		"%d", e.Got, e.Want)
}

// Person is one person's or machine's full state.
type Person struct {
	V int `json:"v"`

	// Kind is person or machine, in internal/iam's vocabulary.
	Kind iam.Kind `json:"kind"`

	// Stage is how far through enrolment they are. Only `active` may act: an
	// allowlist of one, so a stage this build does not know answers false.
	Stage iam.Stage `json:"stage"`

	// NameSealed and EmailSealed are sealed under the fleet keyring with
	// (id, field) as the associated data. See [Sealer].
	NameSealed  string `json:"name_sealed,omitempty"`
	EmailSealed string `json:"email_sealed,omitempty"`

	// Credentials are every way this person can prove themselves. On the
	// person, not a subject of their own: unlike an address, login or seat, a
	// credential is never raced for, so the person's subject already
	// serialises its writes. Changing a password is a read-modify-write of
	// the whole (small) person, safe inside the write authority's one snapshot.
	Credentials []Credential `json:"credentials,omitempty"`

	// Grants are the permissions this person carries, in internal/iam's
	// names: admin, and the apps they use. No roles: a role would be a second
	// vocabulary to keep in step with the permissions internal/authz decides
	// with. An unknown name is kept and grants nothing ([iam.Grant]); a writer
	// may remove it but never add it ([Writer.MayConfer]).
	Grants []iam.Grant `json:"grants,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Enrolled is an enrolment's payload: a person created whole, on the
// directory subject ([OpEnrol]). It carries the document (the person's own
// subject owns it from here on) and the unique values (the directory owns
// them) together, because this record creates the row; every later record
// touches one half or the other.
type Enrolled struct {
	V int `json:"v"`

	// Person is the document the row stores. Its first credential is a
	// created person's first password link, a redeemer's chosen password,
	// or none for a service account.
	Person Person `json:"person"`

	// Holds is the login, the address blind and the seat the person is
	// enrolled holding, each decided free in the record's own snapshot.
	Holds Identifiers `json:"holds"`

	// Invitation is the invitation this enrolment REDEEMS, or empty for an
	// administrator's create. The apply marks it spent in the transaction
	// that writes the person, so a redeemed link never still opens.
	Invitation string `json:"invitation,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// IdentityChange is an identity change's payload ([OpIdentity]): the login and
// the seat an enrolled person holds from this record on. Both are always
// stated, the decide filling the one the caller left alone from its snapshot,
// so the record frees what was held before and applies from itself alone.
type IdentityChange struct {
	V int `json:"v"`

	// Login is never empty: every principal holds one.
	Login string `json:"login"`

	// SeatID is the seat's handle (ADR-0013), empty only for a service
	// account bound to none; a person's seat is moved, never cleared.
	SeatID string `json:"seat_id"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Invitation is an address and a human seat spoken for by somebody who has no
// person yet.
type Invitation struct {
	V int `json:"v"`

	// ID is the invitation's own id, which is what a redemption names.
	ID string `json:"id"`

	// EmailBlind is the keyed blind of the address it was issued to, which a
	// directory decide compares; never the address itself.
	EmailBlind string `json:"email_blind"`

	// Sealed is the address, sealed as the invitation's own (there is no
	// person yet), so it opens as nothing else ([Sealer.SealInvitation]).
	Sealed string `json:"sealed,omitempty"`

	// InvitedBy is the actor who issued it, and Grants what redeeming it
	// confers: decided once by the issuer, not by whoever processes the
	// redemption.
	InvitedBy string      `json:"invited_by,omitempty"`
	Grants    []iam.Grant `json:"grants,omitempty"`

	// Verifier is [InvitationVerifier] of the secret the link carries beside
	// the id ([Blinder.InvitationSecret]), never the secret, because the id
	// alone is in every snapshot, backup and proxy log. An invitation without
	// one is redeemable by nobody.
	Verifier string `json:"verifier,omitempty"`

	// Seat is the HANDLE of the human seat redeeming this binds (ADR-0013),
	// held by the invitation until then: chosen unheld by the issuer and
	// bound by the redemption's own record. Every invitation names one
	// ([Writer.Invite]).
	Seat string `json:"seat,omitempty"`

	// ExpiresAt is when it stops being redeemable. The redemption compares
	// it against the BROKER's instant, never the writer's clock, so two
	// nodes reach the same verdict.
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Session is one signed-in session's lineage.
type Session struct {
	V int `json:"v"`

	// Person is whose session it is.
	Person string `json:"person"`

	// Epoch is the person's revocation epoch when it opened. A bearer below
	// the person's current epoch is over, so "sign out everywhere" is ONE
	// write, not N deletes.
	Epoch uint64 `json:"epoch"`

	// AbsoluteExpiresAt is the deadline no re-issue moves. The idle deadline
	// rides in the bearer's own signature, so moving it costs no store write.
	AbsoluteExpiresAt time.Time `json:"absolute_expires_at,omitzero"`

	// ProvedAt is when the holder proved who they are to open this session (a
	// password and second factor, or an invitation), which a step-up surface
	// asks to be recent. On the session, not the person: proof is a fact
	// about one sign-in. A step-up opens a new session, so this is written
	// once and the log's volume follows sign-ins, not requests. Zero is
	// "nothing was proved" (a Tier A token's exchange), never "long ago".
	ProvedAt time.Time `json:"proved_at,omitzero"`

	// EnrolmentOnly marks a session that may do nothing but ENROL A SECOND
	// FACTOR: opened by a password alone (a sign-in, step-up or redemption)
	// for a person who holds none, where the sign-in policy requires one.
	// Decided once by that sign-in, not per request: the restriction is what
	// the sign-in proved (completing enrolment REPLACES the session, as a
	// step-up does), and a per-request answer would move with each node's
	// setting. Zero is unrestricted. The bearer carries the same decision,
	// signed ([session.Bearer.EnrolmentOnly]), because a sign-in answers
	// before any node applies this row; the session listing reads the row,
	// the request path both.
	EnrolmentOnly bool `json:"enrolment_only,omitempty"`

	// UserAgent is the opening browser's User-Agent header, clipped to
	// [MaxSessionUserAgent] bytes, and Remote the address it opened from,
	// resolved through `api.trusted_proxies` ([auth.Clients]) as the
	// sign-in's audit row names it; both written once, at the start. They let
	// a person tell their sessions apart, to end the one on a lost laptop
	// alone. Empty on a session a Tier A token's exchange opened.
	UserAgent string `json:"user_agent,omitempty"`
	Remote    string `json:"remote,omitempty"`

	// EndedReason is why a session stopped, on the CLOSE record: signed out,
	// revoked or expired. It is what an investigation looks for, and what a
	// plain delete would not leave behind.
	EndedReason string `json:"ended_reason,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Credential is one way somebody proves themselves.
type Credential struct {
	V int `json:"v"`

	// ID is the credential's own id, so a person may hold several.
	ID string `json:"id"`

	// Method is one of [CredentialMethods].
	Method CredentialMethod `json:"method"`

	// Verifier is what a presented secret is checked AGAINST, never the
	// secret: an argon2id digest with its parameters, or a hash of a machine
	// token or recovery code; for a second factor, the sealed seed
	// ([MethodTOTP] says why that one is a secret).
	Verifier string `json:"verifier,omitempty"`

	ExpiresAt time.Time `json:"expires_at,omitzero"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`

	// Label is what the mint called this credential, so somebody holding
	// four tokens knows which to revoke. Free text, not an identity: labels
	// may repeat and every revocation names the id. In the clear like the
	// login, because the directory prints it on every row.
	Label string `json:"label,omitempty"`

	// Grants are what a MACHINE TOKEN was minted carrying: a subset of its
	// owner's grants, decided in the mint's snapshot. A CEILING, not a grant
	// of its own: a request carries only what the owner still holds, so
	// demoting a person demotes their tokens. Empty on every other method.
	Grants []iam.Grant `json:"grants,omitempty"`

	// Epoch is the owner's revocation epoch when a machine token was minted
	// or a reset link issued; either is refused once that epoch moves past
	// it, so signing out everywhere, suspending or offboarding ends every
	// token and link too ([Credential.EndedBy]).
	Epoch uint64 `json:"epoch,omitempty"`

	// Generation is the company's session generation when the token was
	// minted or the link issued, refused once it moves past, as a session is: a backup restores a
	// revoked token or a spent link live, and the restore runbook's
	// invalidate-all is the one gesture that ends every such credential.
	Generation uint64 `json:"generation,omitempty"`

	// Spent marks a reset link its person USED, beside the revocation every
	// link takes when a password is set, so it does not read as one an
	// administrator withdrew.
	Spent bool `json:"spent,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// The bounds on a person's document, each refused `ErrInvalid` at the write
// that would cross it, never cut to fit. They bound this log's largest record
// ([IamMaxRecordBytes]), which sizes its gate reserve: the document is full
// post-state, so an unbounded field is an unbounded record, and an HTTP body
// limit bounds nothing a CLI, duty or test publishes.
const (
	// MaxName bounds a person's display name before it is sealed: 256 bytes,
	// to fit a roster row and beside every change they make.
	MaxName = 256

	// MaxAddress bounds a person's address before it is sealed: 320 bytes,
	// the longest RFC 5321 permits.
	MaxAddress = 320

	// MaxHeldCredentials is the most credentials one person's document
	// holds, lapsed ones included, until the retention sweep collects them
	// ([SessionRowGrace] after they lapse) or a write needs their room
	// ([fitHeld]). Sixty-four: a password, an authenticator and a recovery
	// set, plus sixty-one hand-minted machine tokens, far past any working
	// set, and at the widest token the document fits the log's declared
	// record.
	MaxHeldCredentials = 64
)

// Counters are the two numbers that end a credential stamped with them, read in
// one snapshot: the person's revocation epoch and the company's session
// generation. Every gesture that ends a person's sessions and machine tokens
// moves one of them and writes no `revoked_at`.
type Counters struct{ Epoch, Generation uint64 }

// EndedBy reports a credential a COUNTER ended: a machine token minted, or a
// reset link issued, at an epoch or generation since moved on (the comparison
// [credential.CheckToken] refuses a token on). Counters only move forward, so
// it never verifies again. A reset link is ended like a token: one refused
// only while a suspension stood would let whoever kept an out-of-band copy set
// the password after reactivation.
func (c Credential) EndedBy(now Counters) bool {
	return (c.Method == MethodToken || c.Method == MethodReset) &&
		(now.Epoch > c.Epoch || now.Generation > c.Generation)
}

// stampIssued is after with every reset link not in before stamped with the
// issuing snapshot's counters, so the link ends with whatever moves one next,
// as a token minted there does ([Credential.EndedBy]). The record stamps,
// never its caller, which could read the counters only in another transaction.
// Callers: an administrator's reset ([Writer.UpdatePerson]) and a created
// person's first password link ([Writer.Create]), where before is nil.
func stampIssued(before, after []Credential, counters Counters) []Credential {
	held := make(map[string]bool, len(before))
	for _, c := range before {
		held[c.ID] = true
	}
	out := slices.Clone(after)
	for i, c := range out {
		if c.Method == MethodReset && !held[c.ID] {
			out[i].Epoch, out[i].Generation = counters.Epoch, counters.Generation
		}
	}
	return out
}

// fitHeld is set as a write lands it: unchanged within [MaxHeldCredentials],
// otherwise without the credentials that LAPSED EARLIEST (revoked, expired, or
// ended by a counter at counters, [Credential.EndedBy]) until it fits. It
// refuses with cause only when the live credentials alone are past the cap.
//
// A lapsed credential verifies nothing and is kept only for the listing until
// the sweep collects it; counted as live, a person at the cap was refused a
// mint, a second factor or new recovery codes over dead tokens. Earliest first,
// by when each stopped verifying (set order breaking ties), drops what the
// sweep would collect first. A counter-ended one writes no `revoked_at` and
// the sweep keeps it until expiry (up to a year), so it counts as earliest.
func fitHeld(set []Credential, now time.Time, counters Counters,
	cause error) ([]Credential, error) {

	over := len(set) - MaxHeldCredentials
	if over <= 0 {
		return set, nil
	}
	type lapse struct {
		index int
		at    time.Time
	}
	var lapsed []lapse
	for i, c := range set {
		at, gone := lapsedAt(c, now, counters)
		if gone {
			lapsed = append(lapsed, lapse{index: i, at: at})
		}
	}
	if len(lapsed) < over {
		return nil, fmt.Errorf("%w: this would leave %d live credentials on one "+
			"person and the cap is %d — a revoked, expired or ended one makes room when "+
			"a change needs it, so revoke a token nothing uses and ask again",
			cause, len(set)-len(lapsed), MaxHeldCredentials)
	}
	slices.SortStableFunc(lapsed, func(a, b lapse) int { return a.at.Compare(b.at) })
	drop := make(map[int]bool, over)
	for _, l := range lapsed[:over] {
		drop[l.index] = true
	}
	kept := make([]Credential, 0, MaxHeldCredentials)
	for i, c := range set {
		if !drop[i] {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// lapsedAt is when c stopped verifying — its revocation or its expiry,
// whichever came first, and the zero instant for one a counter ended — and
// false for a credential that still verifies at now and counters.
func lapsedAt(c Credential, now time.Time, counters Counters) (time.Time, bool) {
	if c.EndedBy(counters) {
		return time.Time{}, true
	}
	expired := !c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now)
	switch revoked := !c.RevokedAt.IsZero(); {
	case revoked && expired:
		if c.ExpiresAt.Before(c.RevokedAt) {
			return c.ExpiresAt, true
		}
		return c.RevokedAt, true
	case revoked:
		return c.RevokedAt, true
	case expired:
		return c.ExpiresAt, true
	}
	return time.Time{}, false
}

// CredentialMethod is how somebody proves themselves.
type CredentialMethod string

const (
	// MethodPassword is a secret the person knows, held as an argon2id
	// verifier.
	MethodPassword CredentialMethod = "password"

	// MethodToken is a machine's bearer token, held as a hash.
	MethodToken CredentialMethod = "token"

	// MethodTOTP is a time-based code from an authenticator app, the one
	// method whose stored value is the SECRET, not a verifier: TOTP is
	// symmetric, so a digest could verify nothing. A leaked estate would
	// yield second factors, so the seed is sealed under the fleet keyring
	// before it reaches a payload, as a name and address are; the applier
	// writing it cannot read it.
	//
	// The last accepted step rides in [Credential.Extra], not a column,
	// because a newer build may need fields beside it and a column added
	// later never runs (schema_migrations keys on the filename). Comparing
	// against it refuses a replay inside the same 30-second window.
	MethodTOTP CredentialMethod = "totp"

	// MethodRecovery is the set of ten single-use codes issued beside a
	// second factor, each held as a SHA-256 hash, since a code is presented,
	// not derived. Its own method, not a field on the TOTP one: spending a
	// code writes the person, and the two rotate independently (re-enrol the
	// app and keep the codes, or regenerate the codes and keep the app).
	MethodRecovery CredentialMethod = "recovery"

	// MethodReset is a ONE-TIME PASSWORD LINK, a reset or a create's first
	// ([Writer.Create]): a 256-bit secret (crypto/rand, or derived under the
	// company's key) held as a SHA-256 verifier ([credential.ResetVerifier])
	// with an expiry. One spend path serves both; a link is a first one when
	// its person holds no password ([ResetRow.First]). A credential row, so
	// it is listed, revoked and swept like any other.
	//
	// It authenticates nothing but its own spend ([Writer.SetPassword]):
	// sign-in finds a password by method, the machine-token read refuses
	// other methods, and its verifier's prefix matches no token. The spend
	// revokes it, as does a grant its person gains ([Writer.UpdatePerson]),
	// since the issue judged it against its issuer's grants.
	MethodReset CredentialMethod = "reset"
)

// CredentialMethods are the five.
var CredentialMethods = []CredentialMethod{
	MethodPassword, MethodToken, MethodTOTP, MethodRecovery, MethodReset,
}

// SecondFactorMethods are the methods that satisfy a second factor rather
// than a first: a set, so a caller asking it of a list of credentials never
// spells the two names itself.
var SecondFactorMethods = []CredentialMethod{MethodTOTP, MethodRecovery}

// SecondFactor reports whether a method satisfies a second factor.
func (m CredentialMethod) SecondFactor() bool {
	for _, known := range SecondFactorMethods {
		if m == known {
			return true
		}
	}
	return false
}

// Valid reports whether a method off the wire is one this build knows.
func (m CredentialMethod) Valid() bool {
	for _, known := range CredentialMethods {
		if m == known {
			return true
		}
	}
	return false
}

// Cancellation is an invitation withdrawn before anybody redeemed it
// ([OpCancel]). It names the invitation, which the apply deletes by id, and
// its address's blind, which files the trail row and buckets the record as the
// issue's did; never the address.
type Cancellation struct {
	V int `json:"v"`

	Invitation string `json:"invitation"`
	EmailBlind string `json:"email_blind"`

	Extra map[string]json.RawMessage `json:"-"`
}

// PasswordChange is a person's password replaced and their revocation epoch
// moved, in one record ([OpPassword]). The document is full post-state (the
// new password in, every outstanding reset link revoked) and the epoch is
// stated, never incremented by the applier, for [Revocation]'s reason.
type PasswordChange struct {
	V int `json:"v"`

	Person Person `json:"person"`
	Epoch  uint64 `json:"epoch"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Revocation is a bump of somebody's revocation epoch.
type Revocation struct {
	V int `json:"v"`

	// Epoch is the NEW value, stated: an applier incrementing a column would
	// fold over arrival order, which two nodes at one checkpoint may have
	// seen differently.
	Epoch uint64 `json:"epoch"`

	Extra map[string]json.RawMessage `json:"-"`
}

// StatusChange moves a person between enrolment stages.
type StatusChange struct {
	V int `json:"v"`

	Stage iam.Stage `json:"stage"`

	// Epoch is the revocation epoch a stage that may not act moves the
	// person to, stated for [Revocation]'s reason, and zero for one that may.
	// A suspension ENDS every session and machine token: merely refused
	// while the stage stood, they would come back at reactivation, and the
	// one copy revived would be one somebody else kept.
	Epoch uint64 `json:"epoch,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Removal is a person the company no longer has. A GATE: its record stays at
// the base version for every build ([MutationRecord.readByEveryBuild]) and its
// shape, pinned at [GateRecordVersion], only grows by addition, because a
// deferred removal is an off-boarded person still signing in on that node with
// no later record to correct it.
type Removal struct {
	V int `json:"v"`

	// Released are the identifiers this removal gives back, as blinds and
	// logins, never addresses: the row recording them outlives the person.
	Released Identifiers `json:"released,omitzero"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Identifiers is the set of unique values one person holds, by class: a
// struct, not a map, so a new class is a named field the releasing removal
// cannot miss.
//
// The JSON keys are STORED: a removal's tombstone holds them as
// `iam_removed.claims_json`, and the released seat is also its `seat_id`
// column, written from SeatID by the apply and re-derived from these keys by
// [Applier.Rederive], so a seat's removals are found by a seek.
type Identifiers struct {
	EmailBlind string `json:"email_blind,omitempty"`
	Login      string `json:"login,omitempty"`
	SeatID     string `json:"seat_id,omitempty"`
}

// Empty reports a person holding no identifiers at all.
func (c Identifiers) Empty() bool {
	return c.EmailBlind == "" && c.Login == "" && c.SeatID == ""
}

// Sweep deletes one bucket's expired rows below a POSITION, never a clock,
// which is why it is a record rather than a local delete: these rows are
// identity-claimed, and nodes sweeping on their own clocks would diverge. The
// publisher resolves each horizon to a position once and every data node
// deletes the same rows.
type Sweep struct {
	V int `json:"v"`

	// Bucket is which sixty-fourth this record sweeps, on the payload too so
	// an applier need not parse the subject.
	Bucket Bucket `json:"bucket"`

	// Changes and Sessions are the two authentication-trail horizons, as
	// composed positions. Zero means nothing to delete for that class, not
	// everything: a zero position is below every row's version.
	Changes  uint64 `json:"changes,omitempty"`
	Sessions uint64 `json:"sessions,omitempty"`

	// Expired is the instant over sessions, invitations and credentials
	// (ended, expired, redeemed or revoked) are collected against, already
	// net of [SessionRowGrace], so the applier compares and never subtracts.
	// The publisher read it once: every node compares this carried value
	// with the broker instants its rows hold, whatever its own clock says.
	Expired time.Time `json:"expired,omitzero"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Invalidation ends every session in the company at once. A GATE, its shape
// pinned at [GateRecordVersion]: a node that deferred it would keep honouring
// every bearer just invalidated, and no later record repairs that. Its payload
// stays two fields; anything more (which sessions, whose authority, what
// policy) belongs on a record allowed to evolve, since a field added here is
// read only by builds that already have it.
type Invalidation struct {
	V int `json:"v"`

	// Generation is the NEW value, stated for [Revocation.Epoch]'s reason.
	Generation uint64 `json:"generation"`

	// By is who decided, carried on the record because everybody asks
	// afterwards.
	By string `json:"by,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Eviction gates a node's records on this log, or readmits it. The node is the
// record's SUBJECT ([EvictionSubject]). A GATE, its shape pinned at
// [GateRecordVersion] like a removal's.
//
// It states no position: the gate drops the node's records above the
// eviction's own position and counts them again from the readmission's, and
// only the broker knows where a record lands, after the payload is formed. The
// applier takes both from the record's position, as every sibling log does.
type Eviction struct {
	V int `json:"v"`

	// Readmit makes the record the INVERSE COMMIT rather than a delete, so
	// the whole history survives a replay and a node evicted, readmitted
	// and evicted again does not read as one long absence.
	Readmit bool `json:"readmit,omitempty"`

	// By is who decided, the first thing an operator reading an unexpected
	// eviction needs.
	By string `json:"by,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Generation is a reanchor's own record, its shape pinned at
// [GateRecordVersion]: an older node retains one it cannot read, and a
// retained generation is a transition that node never makes.
type Generation struct {
	V int `json:"v"`

	Generation         uint32    `json:"generation"`
	PrevLastSeqSeen    uint64    `json:"prev_last_seq_seen,omitempty"`
	NewStreamCreatedAt time.Time `json:"new_stream_created_at,omitzero"`
	By                 string    `json:"by,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// The encode/decode pairs follow [jsoncarry]'s rule: a decode keeps what it
// has no home for, an encode folds it back, and a carried field loses to a
// known one.

// Person and Credential carry at ANY DEPTH through codecs of their own:
// encoding/json marshals a nested struct by its fields, where Extra is `-`,
// and a credential's carried fields hold the TOTP last accepted step and the
// recovery codes' verifiers (lost, a TOTP code replays and every recovery code
// is refused).

// MarshalJSON folds what a person's document carries back in, at any depth.
func (p Person) MarshalJSON() ([]byte, error) {
	type fields Person // the same fields with no methods, or this recurses
	return jsoncarry.Encode(fields(p), p.Extra)
}

// UnmarshalJSON keeps what a person's document carries that this build has no
// home for, at any depth.
func (p *Person) UnmarshalJSON(data []byte) error {
	type fields Person
	var known fields
	extra, err := jsoncarry.Decode(data, &known, personFields)
	if err != nil {
		return err
	}
	*p = Person(known)
	p.Extra = extra
	return nil
}

// MarshalJSON folds what a credential carries back in, at any depth.
func (c Credential) MarshalJSON() ([]byte, error) {
	type fields Credential
	return jsoncarry.Encode(fields(c), c.Extra)
}

// UnmarshalJSON keeps what a credential carries that this build has no home
// for, at any depth.
func (c *Credential) UnmarshalJSON(data []byte) error {
	type fields Credential
	var known fields
	extra, err := jsoncarry.Decode(data, &known, credentialFields)
	if err != nil {
		return err
	}
	*c = Credential(known)
	c.Extra = extra
	return nil
}

// EncodePerson is the bytes a person's document travels as, with every field a
// newer build wrote folded back in.
func EncodePerson(p Person) ([]byte, error) { return json.Marshal(p) }

// DecodePerson reads a person's document, keeping every field this build has
// no home for, and refuses one written at a version above [DocumentVersion].
func DecodePerson(data []byte) (Person, error) {
	var p Person
	if err := json.Unmarshal(data, &p); err != nil {
		return Person{}, fmt.Errorf("iamdomain: decode a person: %w", err)
	}
	if err := checkVersion(p.V); err != nil {
		return Person{}, err
	}
	return p, nil
}

// EncodeEnrolled is the bytes an enrolment's payload travels as, with every
// field a newer build wrote folded back in.
func EncodeEnrolled(e Enrolled) ([]byte, error) { return jsoncarry.Encode(e, e.Extra) }

// DecodeEnrolled reads an enrolment's payload, keeping every field this build
// has no home for, and refuses one written at a version above
// [DocumentVersion]. The person document inside it is held to the same rule.
func DecodeEnrolled(data []byte) (Enrolled, error) {
	var e Enrolled
	extra, err := jsoncarry.Decode(data, &e, enrolledFields)
	if err != nil {
		return Enrolled{}, fmt.Errorf("iamdomain: decode an enrolment: %w", err)
	}
	if err := checkVersion(e.V); err != nil {
		return Enrolled{}, err
	}
	if err := checkVersion(e.Person.V); err != nil {
		return Enrolled{}, err
	}
	e.Extra = extra
	return e, nil
}

// EncodeIdentity is the bytes an identity change's payload travels as, with
// every field a newer build wrote folded back in.
func EncodeIdentity(c IdentityChange) ([]byte, error) { return jsoncarry.Encode(c, c.Extra) }

// DecodeIdentity reads an identity change's payload, keeping every field this
// build has no home for, and refuses one written at a version above
// [DocumentVersion].
func DecodeIdentity(data []byte) (IdentityChange, error) {
	var c IdentityChange
	extra, err := jsoncarry.Decode(data, &c, identityFields)
	if err != nil {
		return IdentityChange{}, fmt.Errorf("iamdomain: decode an identity "+
			"change: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return IdentityChange{}, err
	}
	c.Extra = extra
	return c, nil
}

// EncodeInvitation is the bytes an invitation's document travels as, with
// every field a newer build wrote folded back in.
func EncodeInvitation(i Invitation) ([]byte, error) { return jsoncarry.Encode(i, i.Extra) }

// DecodeInvitation reads an invitation's document, keeping every field this
// build has no home for, and refuses one written at a version above
// [DocumentVersion].
func DecodeInvitation(data []byte) (Invitation, error) {
	var i Invitation
	extra, err := jsoncarry.Decode(data, &i, invitationFields)
	if err != nil {
		return Invitation{}, fmt.Errorf("iamdomain: decode an invitation: %w", err)
	}
	if err := checkVersion(i.V); err != nil {
		return Invitation{}, err
	}
	i.Extra = extra
	return i, nil
}

// EncodeSession is the bytes a session record's payload travels as, with every
// field a newer build wrote folded back in.
func EncodeSession(s Session) ([]byte, error) { return jsoncarry.Encode(s, s.Extra) }

// DecodeSession reads a session record's payload, keeping every field this
// build has no home for, and refuses one written at a version above
// [DocumentVersion].
func DecodeSession(data []byte) (Session, error) {
	var s Session
	extra, err := jsoncarry.Decode(data, &s, sessionFields)
	if err != nil {
		return Session{}, fmt.Errorf("iamdomain: decode a session: %w", err)
	}
	if err := checkVersion(s.V); err != nil {
		return Session{}, err
	}
	s.Extra = extra
	return s, nil
}

// EncodeCancellation is the bytes an invitation's cancellation travels as,
// with every field a newer build wrote folded back in.
func EncodeCancellation(c Cancellation) ([]byte, error) { return jsoncarry.Encode(c, c.Extra) }

// DecodeCancellation reads an invitation's cancellation, keeping every field
// this build has no home for, and refuses one written at a version above
// [DocumentVersion].
func DecodeCancellation(data []byte) (Cancellation, error) {
	var c Cancellation
	extra, err := jsoncarry.Decode(data, &c, cancellationFields)
	if err != nil {
		return Cancellation{}, fmt.Errorf("iamdomain: decode a cancellation: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Cancellation{}, err
	}
	c.Extra = extra
	return c, nil
}

// EncodePasswordChange is the bytes a password change travels as, with every
// field a newer build wrote folded back in.
func EncodePasswordChange(c PasswordChange) ([]byte, error) {
	return jsoncarry.Encode(c, c.Extra)
}

// DecodePasswordChange reads a password change, keeping every field this build
// has no home for, and refuses one written at a version above
// [DocumentVersion]. The person document inside it is held to the same rule.
func DecodePasswordChange(data []byte) (PasswordChange, error) {
	var c PasswordChange
	extra, err := jsoncarry.Decode(data, &c, passwordChangeFields)
	if err != nil {
		return PasswordChange{}, fmt.Errorf("iamdomain: decode a password "+
			"change: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return PasswordChange{}, err
	}
	if err := checkVersion(c.Person.V); err != nil {
		return PasswordChange{}, err
	}
	c.Extra = extra
	return c, nil
}

// EncodeRevocation is the bytes a revocation's payload travels as, with every
// field a newer build wrote folded back in.
func EncodeRevocation(r Revocation) ([]byte, error) { return jsoncarry.Encode(r, r.Extra) }

// DecodeRevocation reads a revocation's payload, keeping every field this
// build has no home for, and refuses one written at a version above
// [DocumentVersion].
func DecodeRevocation(data []byte) (Revocation, error) {
	var r Revocation
	extra, err := jsoncarry.Decode(data, &r, revocationFields)
	if err != nil {
		return Revocation{}, fmt.Errorf("iamdomain: decode a revocation: %w", err)
	}
	if err := checkVersion(r.V); err != nil {
		return Revocation{}, err
	}
	r.Extra = extra
	return r, nil
}

// EncodeStatus is the bytes a stage change's payload travels as, with every
// field a newer build wrote folded back in.
func EncodeStatus(s StatusChange) ([]byte, error) { return jsoncarry.Encode(s, s.Extra) }

// DecodeStatus reads a stage change's payload, keeping every field this build
// has no home for, and refuses one written at a version above
// [DocumentVersion].
func DecodeStatus(data []byte) (StatusChange, error) {
	var s StatusChange
	extra, err := jsoncarry.Decode(data, &s, statusFields)
	if err != nil {
		return StatusChange{}, fmt.Errorf("iamdomain: decode a status change: %w", err)
	}
	if err := checkVersion(s.V); err != nil {
		return StatusChange{}, err
	}
	s.Extra = extra
	return s, nil
}

// EncodeRemoval is the bytes a removal's payload travels as, with every field
// a newer build wrote folded back in.
func EncodeRemoval(r Removal) ([]byte, error) { return jsoncarry.Encode(r, r.Extra) }

// DecodeRemoval reads a removal's payload, keeping every field this build has
// no home for, and checks no version: a gate is pinned at [GateRecordVersion],
// so a higher version is no newer shape but a record that must not exist, and
// the envelope pass has already stopped the applier on it.
func DecodeRemoval(data []byte) (Removal, error) {
	var r Removal
	extra, err := jsoncarry.Decode(data, &r, removalFields)
	if err != nil {
		return Removal{}, fmt.Errorf("iamdomain: decode a removal: %w", err)
	}
	r.Extra = extra
	return r, nil
}

// EncodeSweep is the bytes a retention sweep's payload travels as, with every
// field a newer build wrote folded back in.
func EncodeSweep(s Sweep) ([]byte, error) { return jsoncarry.Encode(s, s.Extra) }

// DecodeSweep reads a retention sweep's payload, keeping every field this
// build has no home for, and refuses one written at a version above
// [DocumentVersion].
func DecodeSweep(data []byte) (Sweep, error) {
	var s Sweep
	extra, err := jsoncarry.Decode(data, &s, sweepFields)
	if err != nil {
		return Sweep{}, fmt.Errorf("iamdomain: decode a sweep: %w", err)
	}
	if err := checkVersion(s.V); err != nil {
		return Sweep{}, err
	}
	s.Extra = extra
	return s, nil
}

// EncodeInvalidation is the bytes a company-wide invalidation's payload
// travels as, with every field a newer build wrote folded back in.
func EncodeInvalidation(i Invalidation) ([]byte, error) {
	return jsoncarry.Encode(i, i.Extra)
}

// DecodeInvalidation reads a company-wide invalidation's payload, keeping
// every field this build has no home for, and checks no version: it is a gate,
// so the envelope pass has already refused any other.
func DecodeInvalidation(data []byte) (Invalidation, error) {
	var i Invalidation
	extra, err := jsoncarry.Decode(data, &i, invalidationFields)
	if err != nil {
		return Invalidation{}, fmt.Errorf("iamdomain: decode an invalidation: %w", err)
	}
	i.Extra = extra
	return i, nil
}

// EncodeEviction is the bytes an eviction's or a readmission's payload travels
// as, with every field a newer build wrote folded back in.
func EncodeEviction(e Eviction) ([]byte, error) { return jsoncarry.Encode(e, e.Extra) }

// DecodeEviction reads an eviction's or a readmission's payload, keeping every
// field this build has no home for, and checks no version: it is a gate, so
// the envelope pass has already refused any other.
func DecodeEviction(data []byte) (Eviction, error) {
	var e Eviction
	extra, err := jsoncarry.Decode(data, &e, evictionFields)
	if err != nil {
		return Eviction{}, fmt.Errorf("iamdomain: decode an eviction: %w", err)
	}
	// A gate: a node that deferred an eviction would apply records every
	// peer drops.
	e.Extra = extra
	return e, nil
}

// EncodeGeneration is the bytes a reanchor's payload travels as, with every
// field a newer build wrote folded back in.
func EncodeGeneration(g Generation) ([]byte, error) { return jsoncarry.Encode(g, g.Extra) }

// DecodeGeneration reads a reanchor's payload, keeping every field this build
// has no home for, and refuses one written at a version above
// [GateRecordVersion], which it is pinned at for ever.
func DecodeGeneration(data []byte) (Generation, error) {
	var g Generation
	extra, err := jsoncarry.Decode(data, &g, generationFields)
	if err != nil {
		return Generation{}, fmt.Errorf("iamdomain: decode a generation: %w", err)
	}
	if g.V > GateRecordVersion {
		return Generation{}, ErrUnknownVersion{Got: g.V, Want: GateRecordVersion}
	}
	g.Extra = extra
	return g, nil
}

// EncodeCredential is the bytes one credential's document travels as, with
// every field a newer build wrote folded back in.
func EncodeCredential(c Credential) ([]byte, error) { return json.Marshal(c) }

// DecodeCredential reads one credential's document, keeping every field this
// build has no home for, and refuses one written at a version above
// [DocumentVersion].
func DecodeCredential(data []byte) (Credential, error) {
	var c Credential
	if err := json.Unmarshal(data, &c); err != nil {
		return Credential{}, fmt.Errorf("iamdomain: decode a credential: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Credential{}, err
	}
	return c, nil
}

func checkVersion(got int) error {
	if got > DocumentVersion {
		return ErrUnknownVersion{Got: got, Want: DocumentVersion}
	}
	return nil
}

// The field sets, derived from each struct's own tags, never hand-kept
// ([jsoncarry.Names]).
var (
	personFields         = jsoncarry.Names(Person{})
	enrolledFields       = jsoncarry.Names(Enrolled{})
	identityFields       = jsoncarry.Names(IdentityChange{})
	invitationFields     = jsoncarry.Names(Invitation{})
	sessionFields        = jsoncarry.Names(Session{})
	cancellationFields   = jsoncarry.Names(Cancellation{})
	passwordChangeFields = jsoncarry.Names(PasswordChange{})
	revocationFields     = jsoncarry.Names(Revocation{})
	statusFields         = jsoncarry.Names(StatusChange{})
	removalFields        = jsoncarry.Names(Removal{})
	sweepFields          = jsoncarry.Names(Sweep{})
	invalidationFields   = jsoncarry.Names(Invalidation{})
	evictionFields       = jsoncarry.Names(Eviction{})
	generationFields     = jsoncarry.Names(Generation{})
	credentialFields     = jsoncarry.Names(Credential{})
)
