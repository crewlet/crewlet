package iamdomain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jsoncarry"
)

// THE TYPED PAYLOADS, one per (subject kind, op) that writes rows.
//
// # Every one of them is FULL POST-STATE
//
// Not a patch, which is the opposite of the tracker's choice for its largest
// objects and the same as the org chart's, for the org chart's reason: a
// person is authored as a FORM and submitted whole, so the writer always holds
// the complete new value and a patch would be a diff it computed in order to
// be reassembled by every node. The objects are also small — a person is a
// name, an address, a stage and a handful of grants.
//
// # What a payload may and may not carry
//
// A NAME OR AN ADDRESS IS ALWAYS SEALED. Never the cleartext, on any payload,
// at any version: the record is on the broker, in every node's deferred table,
// in every snapshot and in every backup, and sealing at the WRITER is what
// makes all of those unreadable the moment a removal destroys the key.
//
// A BLIND IS ALWAYS THE MATCHED FORM. Never the address it was derived from —
// a blind is what a claim arbitrates on and what a lookup compares, and the
// address it came from is the one value the estate is built to not hold.
//
// EVERY DOCUMENT CARRIES ITS OWN VERSION and an Extra map, so a field a newer
// build wrote round-trips through a node that cannot interpret it. The rule
// and its one load-bearing clause are [jsoncarry]'s.

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

	// Stage is how far through enrolment they are. Only `active` may act,
	// which is an allowlist of one: a stage this build does not know
	// answers false, and a denylist would have admitted it.
	Stage iam.Stage `json:"stage"`

	// NameSealed and EmailSealed are sealed under this person's own key
	// with (id, field) as the associated data. See [Sealer].
	NameSealed  string `json:"name_sealed,omitempty"`
	EmailSealed string `json:"email_sealed,omitempty"`

	// Credentials are every way this person can prove themselves, as FULL
	// POST-STATE like the rest of the document.
	//
	// ON THE PERSON RATHER THAN ON A SUBJECT OF THEIR OWN, which is the
	// one place this domain does NOT give something its own arbitration
	// unit — and the reason is that a credential has no address. An
	// address, a login and a seat are tokens two writers can RACE FOR;
	// a credential belongs to exactly one person from the moment it
	// exists, so the only contention it can have is with that person's
	// other edits, and their subject already serialises those.
	//
	// The cost is stated rather than glossed: changing a password is a
	// read-modify-write of the whole person. That is the write authority's
	// own shape — take ONE snapshot, decide and form the expectation
	// inside it — so it is safe, and a person is a name, a stage and a
	// handful of credentials rather than anything large.
	Credentials []Credential `json:"credentials,omitempty"`

	// Grants are the capabilities this person carries, as internal/iam
	// names them.
	//
	// ON THE PERSON AND NOT DERIVED FROM A ROLE, because this engine has
	// no roles: a grant is the unit internal/authz decides with, and a
	// role would be a second vocabulary that has to be kept in step with
	// it. A grant this build does not know is RETAINED and refused rather
	// than dropped — see [iam.Principal.UnknownGrants].
	Grants []iam.Grant `json:"grants,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Claim is one address, login or seat binding, and who holds it.
//
// THE SAME SHAPE FOR ALL THREE, because what differs between them is the
// SUBJECT they arbitrate on rather than anything about the claim itself: an
// address contends on its blind, a login on the login, a seat on the seat id,
// and each is "this token belongs to this person from now".
type Claim struct {
	V int `json:"v"`

	// Person is the id the claim binds the token to. Empty on a release,
	// which is what makes a release readable as one without an op lookup.
	Person string `json:"person,omitempty"`

	// Sealed is the cleartext form of the claimed token, sealed under the
	// person's key, for the ONE claim whose token is not readable: an
	// address. A login and a seat id are their own subject and are not
	// secret, so they carry nothing here.
	//
	// IT IS WHAT LETS A PERSON READ BACK THE ADDRESS THEY ENROLLED WITH.
	// The blind is one-way by construction, so without this the company
	// could authenticate somebody and never show them which address it
	// authenticated.
	Sealed string `json:"sealed,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Invitation is an address spoken for by somebody who has no person yet.
type Invitation struct {
	V int `json:"v"`

	// ID is the invitation's own id, which is what a redemption names.
	ID string `json:"id"`

	// Sealed is the address, sealed under the INVITATION's own key rather
	// than a person's — there is no person yet, and minting one for an
	// invitation that may never be redeemed would leave a key behind for
	// every address anybody ever typed.
	Sealed string `json:"sealed,omitempty"`

	// InvitedBy is the actor who issued it, and Grants what redeeming it
	// confers — decided once, by the person who decided it, rather than
	// again by whoever happens to process the redemption.
	InvitedBy string      `json:"invited_by,omitempty"`
	Grants    []iam.Grant `json:"grants,omitempty"`

	// ExpiresAt is when it stops being redeemable. THE WRITER'S CLOCK is
	// not what enforces it: the applier stores the instant and the
	// redemption compares against the BROKER's, so two nodes reach the
	// same verdict.
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	// Person is set by the REDEMPTION record, naming who it created.
	Person string `json:"person,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Session is one signed-in session's lineage.
//
// NO IP ADDRESS, NO USER AGENT, NO DEVICE FINGERPRINT, and the absence is the
// design rather than a field nobody got to. Where a session was opened from is
// a request-scoped observation; putting one on a replicated, retained record
// would make every node's database a location history of everybody who works
// here — and a state log's records outlive the rows derived from them.
type Session struct {
	V int `json:"v"`

	// Person is whose session it is.
	Person string `json:"person"`

	// Epoch is the person's revocation epoch at the moment it opened. A
	// bearer presenting an epoch below the person's current one is over,
	// which is what makes "sign out everywhere" ONE write rather than N
	// deletes.
	Epoch uint64 `json:"epoch"`

	// AbsoluteExpiresAt is the deadline no re-issue moves. The IDLE
	// deadline is deliberately absent: it rides in the bearer's own
	// signature, so moving it costs no store write at all.
	AbsoluteExpiresAt time.Time `json:"absolute_expires_at,omitzero"`

	// EndedReason is why a session stopped, on the CLOSE record: signed
	// out, revoked, expired, or reuse detected. "This session was ended by
	// reuse detection" is the sentence an investigation is looking for,
	// and it is the one an ordinary delete would not have left behind.
	EndedReason string `json:"ended_reason,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Credential is one way somebody proves themselves.
type Credential struct {
	V int `json:"v"`

	// ID is the credential's own id, so a person may hold several.
	ID string `json:"id"`

	// Method is password, oidc or token.
	Method CredentialMethod `json:"method"`

	// Verifier is what a presented secret is checked AGAINST, never the
	// secret: an argon2id digest with its parameters, a hash of a machine
	// token, or — for an identity provider — nothing at all, because
	// nothing is presented to this engine.
	Verifier string `json:"verifier,omitempty"`

	// SubjectBlind is the provider's own subject claim, blinded, for an
	// oidc credential. It identifies a person at a third party, which is
	// the same reason an address is blinded and the only one.
	SubjectBlind string `json:"subject_blind,omitempty"`

	ExpiresAt time.Time `json:"expires_at,omitzero"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`

	Extra map[string]json.RawMessage `json:"-"`
}

// CredentialMethod is how somebody proves themselves.
type CredentialMethod string

const (
	// MethodPassword is a secret the person knows, held as an argon2id
	// verifier.
	MethodPassword CredentialMethod = "password"

	// MethodOIDC is an identity provider's assertion. Nothing is
	// presented to this engine, so there is no verifier at all: what is
	// stored is which subject at which issuer this person is.
	MethodOIDC CredentialMethod = "oidc"

	// MethodToken is a machine's bearer token, held as a hash.
	MethodToken CredentialMethod = "token"
)

// CredentialMethods are the three.
var CredentialMethods = []CredentialMethod{
	MethodPassword, MethodOIDC, MethodToken,
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

// Revocation is a bump of somebody's revocation epoch.
type Revocation struct {
	V int `json:"v"`

	// Epoch is the NEW value, stated rather than derived: an applier that
	// incremented a column would fold over an arrival order, and two nodes
	// at one checkpoint have seen the same set in a different order.
	Epoch uint64 `json:"epoch"`

	Extra map[string]json.RawMessage `json:"-"`
}

// StatusChange moves a person between enrolment stages.
type StatusChange struct {
	V int `json:"v"`

	Stage iam.Stage `json:"stage"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Removal is a person the company no longer has.
//
// PINNED AT [GateRecordVersion] FOR EVER, like every gate-installing record:
// a removal whose version a node could not read would be deferred, and a
// deferred removal here is somebody off-boarded still signing in on one node,
// with no later record that corrects it.
type Removal struct {
	V int `json:"v"`

	// Released are the claim tokens this removal gives back, as BLINDS and
	// logins rather than addresses — the row that records them outlives
	// the person, and writing an address into the one row designed to
	// outlive somebody would be the removal's own promise broken by the
	// mechanism that makes it.
	Released Claims `json:"released,omitzero"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Claims is the set of tokens one person holds, by class.
//
// A STRUCT AND NOT A MAP, so a class added later is a field with a name rather
// than a key every reader has to know to look for — and so the removal that
// releases them cannot silently miss one it has never heard of.
type Claims struct {
	EmailBlind string `json:"email_blind,omitempty"`
	Login      string `json:"login,omitempty"`
	SeatID     string `json:"seat_id,omitempty"`
}

// Empty reports a person holding no claims at all, which is ordinary: an
// invited person has none until they redeem.
func (c Claims) Empty() bool {
	return c.EmailBlind == "" && c.Login == "" && c.SeatID == ""
}

// Bootstrap is the company's own way in before it has anybody.
type Bootstrap struct {
	V int `json:"v"`

	// ID is this code's own id.
	ID string `json:"id"`

	// Verifier is what a presented code is checked against, never the
	// code: a code readable out of a replicated database by anyone who can
	// read a replicated database is not a credential.
	Verifier string `json:"verifier,omitempty"`

	// MintedBy is the node that issued it, which is what an operator
	// reading a code they did not expect needs first.
	MintedBy string `json:"minted_by,omitempty"`

	ExpiresAt time.Time `json:"expires_at,omitzero"`

	// Person is set by the REDEMPTION, naming the administrator it
	// created.
	Person string `json:"person,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Sweep deletes one bucket's expired rows below a POSITION.
//
// A POSITION AND NEVER A CLOCK, which is the whole of why this is a record
// rather than a local delete: these rows are identity-claimed, so two nodes
// sweeping on their own clocks would hold different bytes and "N byte-identical
// copies" would quietly become a claim about how synchronised their clocks
// were. The publisher resolves each horizon to a position ONCE, and every node
// then deletes exactly the same rows.
type Sweep struct {
	V int `json:"v"`

	// Bucket is which sixty-fourth this record sweeps. It is on the
	// payload as well as in the subject so an applier that has the record
	// need not parse a subject back to learn it.
	Bucket Bucket `json:"bucket"`

	// Changes and Sessions are the two authentication-trail horizons, as
	// the composed positions the publisher resolved them to. Zero means
	// "nothing to delete for this class", which is ordinary on a young
	// company and is NOT the same as "delete everything": a zero position
	// is below every row's version.
	Changes  uint64 `json:"changes,omitempty"`
	Sessions uint64 `json:"sessions,omitempty"`

	// Expired is the broker instant sessions, invitations and bootstrap
	// codes are collected against once they are over. It is the BROKER's
	// rather than the publisher's own clock for the reason above.
	Expired time.Time `json:"expired,omitzero"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Eviction gates a node's records on this log, or readmits it.
type Eviction struct {
	V int `json:"v"`

	// From is the position above which this node's records are dropped,
	// and Readmitted the position at or above which they count again.
	// Zero Readmitted is a node still out.
	From       uint64 `json:"from"`
	Readmitted uint64 `json:"readmitted,omitempty"`

	// By is who decided, which is the first thing an operator reading an
	// eviction they did not expect needs.
	By string `json:"by,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Generation is a reanchor's own record.
type Generation struct {
	V int `json:"v"`

	Generation         uint32    `json:"generation"`
	PrevLastSeqSeen    uint64    `json:"prev_last_seq_seen,omitempty"`
	NewStreamCreatedAt time.Time `json:"new_stream_created_at,omitzero"`
	By                 string    `json:"by,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// The encode/decode pairs. Each is the same three lines because the rule is
// [jsoncarry]'s and not this package's: a decode keeps what it has no home
// for, an encode folds it back, and a carried field loses to a known one.

func EncodePerson(p Person) ([]byte, error) { return jsoncarry.Encode(p, p.Extra) }

func DecodePerson(data []byte) (Person, error) {
	var p Person
	extra, err := jsoncarry.Decode(data, &p, personFields)
	if err != nil {
		return Person{}, fmt.Errorf("iamdomain: decode a person: %w", err)
	}
	if err := checkVersion(p.V); err != nil {
		return Person{}, err
	}
	p.Extra = extra
	return p, nil
}

func EncodeClaim(c Claim) ([]byte, error) { return jsoncarry.Encode(c, c.Extra) }

func DecodeClaim(data []byte) (Claim, error) {
	var c Claim
	extra, err := jsoncarry.Decode(data, &c, claimFields)
	if err != nil {
		return Claim{}, fmt.Errorf("iamdomain: decode a claim: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Claim{}, err
	}
	c.Extra = extra
	return c, nil
}

func EncodeInvitation(i Invitation) ([]byte, error) { return jsoncarry.Encode(i, i.Extra) }

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

func EncodeSession(s Session) ([]byte, error) { return jsoncarry.Encode(s, s.Extra) }

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

func EncodeRevocation(r Revocation) ([]byte, error) { return jsoncarry.Encode(r, r.Extra) }

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

func EncodeStatus(s StatusChange) ([]byte, error) { return jsoncarry.Encode(s, s.Extra) }

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

func EncodeRemoval(r Removal) ([]byte, error) { return jsoncarry.Encode(r, r.Extra) }

func DecodeRemoval(data []byte) (Removal, error) {
	var r Removal
	extra, err := jsoncarry.Decode(data, &r, removalFields)
	if err != nil {
		return Removal{}, fmt.Errorf("iamdomain: decode a removal: %w", err)
	}
	// NO VERSION CHECK, and it is the one payload that has none. A removal
	// is pinned at [GateRecordVersion] for ever, so a version above this
	// build's is not a newer shape to defer — it is a record that must not
	// exist, and the envelope pass has already refused it as a gate.
	r.Extra = extra
	return r, nil
}

func EncodeBootstrapDoc(b Bootstrap) ([]byte, error) { return jsoncarry.Encode(b, b.Extra) }

func DecodeBootstrapDoc(data []byte) (Bootstrap, error) {
	var b Bootstrap
	extra, err := jsoncarry.Decode(data, &b, bootstrapFields)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("iamdomain: decode a bootstrap: %w", err)
	}
	if err := checkVersion(b.V); err != nil {
		return Bootstrap{}, err
	}
	b.Extra = extra
	return b, nil
}

func EncodeSweep(s Sweep) ([]byte, error) { return jsoncarry.Encode(s, s.Extra) }

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

func EncodeEviction(e Eviction) ([]byte, error) { return jsoncarry.Encode(e, e.Extra) }

func DecodeEviction(data []byte) (Eviction, error) {
	var e Eviction
	extra, err := jsoncarry.Decode(data, &e, evictionFields)
	if err != nil {
		return Eviction{}, fmt.Errorf("iamdomain: decode an eviction: %w", err)
	}
	// PINNED, like a removal and for the reason one layer up: a node that
	// deferred an eviction goes on applying records every peer is dropping.
	e.Extra = extra
	return e, nil
}

func EncodeGeneration(g Generation) ([]byte, error) { return jsoncarry.Encode(g, g.Extra) }

func DecodeGeneration(data []byte) (Generation, error) {
	var g Generation
	extra, err := jsoncarry.Decode(data, &g, generationFields)
	if err != nil {
		return Generation{}, fmt.Errorf("iamdomain: decode a generation: %w", err)
	}
	if err := checkVersion(g.V); err != nil {
		return Generation{}, err
	}
	g.Extra = extra
	return g, nil
}

func EncodeCredential(c Credential) ([]byte, error) { return jsoncarry.Encode(c, c.Extra) }

func DecodeCredential(data []byte) (Credential, error) {
	var c Credential
	extra, err := jsoncarry.Decode(data, &c, credentialFields)
	if err != nil {
		return Credential{}, fmt.Errorf("iamdomain: decode a credential: %w", err)
	}
	if err := checkVersion(c.V); err != nil {
		return Credential{}, err
	}
	c.Extra = extra
	return c, nil
}

func checkVersion(got int) error {
	if got > DocumentVersion {
		return ErrUnknownVersion{Got: got, Want: DocumentVersion}
	}
	return nil
}

// The field sets, DERIVED from each struct rather than typed again. The
// omitempty names have to be listed because a zero value does not marshal
// them, and a name missing here is decoded into the struct AND carried as
// unknown — so the next encode writes the stale carried copy back over what
// the caller set.
var (
	personFields = jsoncarry.Names(Person{}, "name_sealed", "email_sealed",
		"credentials", "grants")
	claimFields      = jsoncarry.Names(Claim{}, "person", "sealed")
	invitationFields = jsoncarry.Names(Invitation{}, "sealed", "invited_by",
		"grants", "expires_at", "person")
	sessionFields = jsoncarry.Names(Session{}, "absolute_expires_at",
		"ended_reason")
	revocationFields = jsoncarry.Names(Revocation{})
	statusFields     = jsoncarry.Names(StatusChange{})
	removalFields    = jsoncarry.Names(Removal{}, "released")
	bootstrapFields  = jsoncarry.Names(Bootstrap{}, "verifier", "minted_by",
		"expires_at", "person")
	sweepFields      = jsoncarry.Names(Sweep{}, "changes", "sessions", "expired")
	evictionFields   = jsoncarry.Names(Eviction{}, "readmitted", "by")
	generationFields = jsoncarry.Names(Generation{}, "prev_last_seq_seen",
		"new_stream_created_at", "by")
	credentialFields = jsoncarry.Names(Credential{}, "verifier",
		"subject_blind", "expires_at", "revoked_at")
)
