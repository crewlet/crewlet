package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// A COMPANY'S OAUTH GRANT, and a sign-in in progress that makes one: one per
// name in each of the [GrantFamilies] (internal/engine's oauthgrant.go). Its
// SECRET half (refresh token, client) is sealed under `<owner>/<name>/refresh`,
// which no operator surface reaches (internal/fleetsecrets' Estate); its
// PUBLIC half, here, holds no token. The
// whole company agrees on both: every node sends under the grant, and a
// sign-in's callback may reach any node, so a new sign-in replaces the one in
// progress rather than racing it to one grant (as devicesignin.go's does).
// Neither ages by a bucket, which an update would restart (internal/coord/kv's
// package doc): both live in the positions register, a grant until the
// decision that ends it, a sign-in until [OAuthSignIn.Lapsed].

// GrantFamily is one family of OAuth grants: where its sealed rows, records
// and renewal leases are filed. The families share every rule and one
// implementation; their records never mix.
type GrantFamily struct {
	// Owner is the estate owner its sealed rows are named under
	// (internal/secrets' estate owners).
	Owner string
	// GrantClass and SignInClass file its grants and sign-ins in the
	// positions register, and Renewal is the lease class a renewal holds.
	GrantClass, SignInClass string
	Renewal                 Class
	// Resourced is whether a grant is bound to the resource it was issued
	// for (RFC 8707), which its record then names.
	Resourced bool
}

var (
	// MCPGrantFamily is the remote MCP servers' grants (ADR-0075), one per
	// server, named by the server.
	MCPGrantFamily = GrantFamily{Owner: "mcp", GrantClass: MCPGrantClass, SignInClass: MCPSignInClass,
		Renewal: "mcp_oauth_renewal", Resourced: true}
	// IntegrationGrantFamily is the integrations' consent grants (ADR-0178),
	// one per integration kind, named by the kind.
	IntegrationGrantFamily = GrantFamily{Owner: "integration", GrantClass: IntegrationGrantClass,
		SignInClass: IntegrationSignInClass, Renewal: "integration_grant_renewal"}
)

// The positions register's key classes of the two families' grants and
// sign-ins.
const (
	MCPGrantClass          = "mcp_oauth"
	MCPSignInClass         = "mcp_sign_in"
	IntegrationGrantClass  = "integration_grant"
	IntegrationSignInClass = "integration_sign_in"
)

// GrantFamilies is every family this build names.
var GrantFamilies = []GrantFamily{MCPGrantFamily, IntegrationGrantFamily}

// Valid reports whether f is a family this build names.
func (f GrantFamily) Valid() bool { return slices.Contains(GrantFamilies, f) }

// GrantKey is a grant's key in the register.
func (f GrantFamily) GrantKey(name string) string { return DocumentKey(f.GrantClass, name) }

// GrantNameOf recovers a grant's name from a key of the family's grant class.
func (f GrantFamily) GrantNameOf(key string) (string, bool) { return serverOf(f.GrantClass, key) }

// SignInKey is a sign-in's key in the register.
func (f GrantFamily) SignInKey(name string) string { return DocumentKey(f.SignInClass, name) }

// SignInNameOf recovers a sign-in's name from a key of the family's sign-in
// class.
func (f GrantFamily) SignInNameOf(key string) (string, bool) { return serverOf(f.SignInClass, key) }

// ErrUnknownGrantFamily is a grant read or write naming a family this build
// does not.
var ErrUnknownGrantFamily = errors.New("coord: an OAuth grant names a family this build does not know")

// MaxGrantWords bounds each sentence a grant record carries: the vendor's
// words for a renewal it refused, which a person reads beside the grant.
// REASONED: an authorization server's error code and description, both cut
// to 512 bytes where they are read (internal/mcp/oauth), and the sentence
// around them.
const MaxGrantWords = 1 << 10

// MaxGrantScopes bounds the scopes a grant record names, as a sign-in asks
// for at most this many (internal/config's MaxMCPOAuthScopes).
const MaxGrantScopes = 50

// MaxGrantAccounts bounds the vendor accounts a grant record names, and
// MaxGrantAccount each one. REASONED: a person's consent reaches the few
// publisher accounts they hold, an agency's a few dozen; a vendor's account
// ID is a short token.
const (
	MaxGrantAccounts = 100
	MaxGrantAccount  = 128
)

// OAuthGrant is the public half of one OAuth grant.
type OAuthGrant struct {
	// Name is the grant's name in its family (an MCP server's, an
	// integration kind), and the record's key.
	Name string `json:"name"`

	// Grant names this sign-in: a sign-in that replaces the grant is a new
	// one, which the estate's sealed half carries too, so a node holding an
	// access token under an older grant stops sending it.
	Grant uuid.UUID `json:"grant"`

	// Client is how the engine registered (`cimd`, `dcr`, `own`), ClientID
	// the id it signed in as (public in every kind), Issuer the
	// authorization server, and Scopes what it granted.
	Client   string   `json:"client"`
	ClientID string   `json:"client_id"`
	Issuer   string   `json:"issuer"`
	Scopes   []string `json:"scopes"`

	// Resource is the server the grant was issued for (RFC 8707), as its
	// protected-resource metadata named it, in a [GrantFamily.Resourced]
	// family: the one address the grant's tokens are sent to.
	Resource string `json:"resource,omitempty"`

	// Accounts are the vendor accounts the grant reached when its product
	// last looked, nil before it has.
	Accounts []string `json:"accounts,omitempty"`

	// By, ByKind and OperatorID are who signed in on Crewlet, as
	// [SeatPause] records them, and At when.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`
	At         time.Time     `json:"at"`

	// Expiry is when the grant runs out where its authorization server said
	// (its refresh token's own life), nil where it did not: a grant renewed
	// as it is used, until somebody revokes it.
	Expiry *time.Time `json:"expiry,omitempty"`

	// Failure is the authorization server's words for the renewal it last
	// refused for good, bounded and redacted, and FailedAt when: "" while
	// the grant renews. A failed grant is one a person signs in to again.
	Failure  string     `json:"failure,omitempty"`
	FailedAt *time.Time `json:"failed_at,omitempty"`

	// Version is the store's version of the record as it was read. Not on
	// the wire.
	Version uint64 `json:"-"`
}

// Validate reports why a grant of family f cannot be written.
func (g OAuthGrant) Validate(f GrantFamily) error {
	switch {
	case !f.Valid():
		return ErrUnknownGrantFamily
	case g.Name == "":
		return errors.New("coord: an OAuth grant needs its name")
	case g.Grant == uuid.Nil:
		return errors.New("coord: an OAuth grant needs its sign-in's id")
	case g.Client == "" || g.ClientID == "" || g.Issuer == "":
		return errors.New("coord: an OAuth grant needs its client, client id and issuer")
	case f.Resourced && g.Resource == "":
		return errors.New("coord: an MCP grant needs the resource it was issued for")
	case g.By == "" || !g.ByKind.Valid():
		return errors.New("coord: an OAuth grant needs who signed in")
	case g.At.IsZero():
		return errors.New("coord: an OAuth grant needs when it was signed in")
	case len(g.Scopes) > MaxGrantScopes:
		return fmt.Errorf("coord: an OAuth grant names %d scopes, more than the %d a record holds",
			len(g.Scopes), MaxGrantScopes)
	case len(g.Accounts) > MaxGrantAccounts:
		return fmt.Errorf("coord: an OAuth grant names %d accounts, more than the %d a record holds",
			len(g.Accounts), MaxGrantAccounts)
	case slices.ContainsFunc(g.Accounts, func(a string) bool { return a == "" || len(a) > MaxGrantAccount }):
		return fmt.Errorf("coord: an OAuth grant names an empty account, or one over %d bytes", MaxGrantAccount)
	case len(g.Failure) > MaxGrantWords:
		return fmt.Errorf("coord: an OAuth grant's failure is over %d bytes", MaxGrantWords)
	case (g.Failure == "") != (g.FailedAt == nil):
		return errors.New("coord: an OAuth grant's failure and when it failed go together")
	case !plainWithin(g.Name, MaxGrantNameBytes) || !plainWithin(g.Client, maxGrantClientBytes):
		return fmt.Errorf("coord: an OAuth grant names a grant or a client kind past %d plain bytes",
			MaxGrantNameBytes)
	case len(g.ClientID) > MaxGrantAddressBytes || len(g.Issuer) > MaxGrantAddressBytes ||
		len(g.Resource) > MaxGrantAddressBytes:
		return fmt.Errorf("coord: an OAuth grant names a client id, issuer or resource past %d bytes",
			MaxGrantAddressBytes)
	case slices.ContainsFunc(g.Scopes, func(s string) bool { return len(s) > MaxGrantScopeBytes }):
		return fmt.Errorf("coord: an OAuth grant names a scope past %d bytes", MaxGrantScopeBytes)
	}
	return checkActor("an OAuth grant", g.By, g.OperatorID)
}

// MaxGrantNameBytes bounds a grant's name: a configured MCP server's
// (internal/config's MaxMCPServerName) or an integration kind.
const MaxGrantNameBytes = 64

// maxGrantClientBytes bounds how a grant registered: a word of the catalogue's
// table (`cimd`, `dcr`, `own`).
const maxGrantClientBytes = 16

// MaxGrantAddressBytes bounds a grant's client id, issuer and resource: each
// an address (a client metadata document's, for `cimd`) or an id, which
// internal/config refuses past it where it is configured and internal/engine
// where the vendor gives it, before a sign-in sends anybody to consent.
const MaxGrantAddressBytes = 2 << 10

// MaxGrantScopeBytes bounds one scope a grant names: refused past it where it
// is configured (internal/config) or asked for (internal/engine), and a
// longer one a vendor grants unasked is left out of the record.
const MaxGrantScopeBytes = 256

// MaxOAuthGrantBytes bounds a grant's record as stored: every field at its
// bound and every byte JSON may escape six-fold (three addresses, fifty
// scopes, a hundred accounts and a failure), about 198 KiB. Every read of it
// decides a renewal or a sign-in a person waits on, so it crosses a leaf's
// link at the slowest pace in 7.9 s (ADR-0175).
const MaxOAuthGrantBytes = 200 << 10

// Failed reports whether the grant's last renewal was refused for good.
func (g OAuthGrant) Failed() bool { return g.Failure != "" }

// serverOf recovers the entry from a key of class, reporting false for a key
// of another class or one this grammar did not write.
func serverOf(class, key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != class || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}

// OAuthGrantUpdate is one change a grant watch delivers.
type OAuthGrantUpdate struct {
	// Grant is the record the name now holds, nil when it was removed.
	Grant *OAuthGrant
	// Name names the grant the update is about. Empty on the marker.
	Name string
	// Current marks the end of the records that existed when the watch
	// started, as [SeatPauseUpdate.Current] does.
	Current bool
}

// OAuthGrants is the fleet's record of each OAuth grant, by family.
//
// Every read RAISES rather than answering empty when the store cannot be
// read: "nobody signed in" and "could not read" are two facts, and a node that
// took the second for the first would stop a server that is signed in.
type OAuthGrants interface {
	// OAuthGrant reads one grant.
	OAuthGrant(ctx context.Context, f GrantFamily, name string) (OAuthGrant, bool, error)

	// ListOAuthGrants returns every grant of f, ordered by name.
	ListOAuthGrants(ctx context.Context, f GrantFamily) ([]OAuthGrant, error)

	// CreateOAuthGrant writes a grant for a name that has none, returning
	// it as stored; one that has a grant is left alone and reports false.
	CreateOAuthGrant(ctx context.Context, f GrantFamily, g OAuthGrant) (OAuthGrant, bool, error)

	// UpdateOAuthGrant writes g at g.Version, reporting false when that
	// version no longer holds, including when the record is gone.
	UpdateOAuthGrant(ctx context.Context, f GrantFamily, g OAuthGrant) (OAuthGrant, bool, error)

	// DeleteOAuthGrant removes a grant at a version, reporting whether that
	// version still held. A version of zero never holds.
	DeleteOAuthGrant(ctx context.Context, f GrantFamily, name string, version uint64) (bool, error)

	// WatchOAuthGrants streams every grant of f, then the
	// [OAuthGrantUpdate.Current] marker, then every change after it.
	WatchOAuthGrants(ctx context.Context, f GrantFamily) (<-chan OAuthGrantUpdate, error)
}

// OAuthSignInStatus is where a sign-in is.
type OAuthSignInStatus string

const (
	// OAuthSignInPending is a sign-in waiting for the person to consent at
	// the vendor.
	OAuthSignInPending OAuthSignInStatus = "pending"
	// OAuthSignInCompleting is a sign-in whose callback arrived and is
	// being exchanged: the state it was started with is spent.
	OAuthSignInCompleting OAuthSignInStatus = "completing"
	// OAuthSignInApproved is a sign-in whose grant was sealed.
	OAuthSignInApproved OAuthSignInStatus = "approved"
	// OAuthSignInFailed is a sign-in that ended without a grant.
	OAuthSignInFailed OAuthSignInStatus = "failed"
)

// OAuthSignInStatuses is every status this build names, in a sign-in's order.
var OAuthSignInStatuses = []OAuthSignInStatus{
	OAuthSignInPending, OAuthSignInCompleting, OAuthSignInApproved, OAuthSignInFailed,
}

// Valid reports whether the status is one this build names.
func (s OAuthSignInStatus) Valid() bool { return slices.Contains(OAuthSignInStatuses, s) }

// Ended reports whether the sign-in is over.
func (s OAuthSignInStatus) Ended() bool { return s == OAuthSignInApproved || s == OAuthSignInFailed }

// SignInLinger is how long a sign-in's record answers after it expires: ten
// minutes, for [DeviceSignInLinger]'s reason, so a page that polls, or a
// person who looked away, still reads how it ended.
const SignInLinger = 10 * time.Minute

// OAuthSignIn is one grant's sign-in in progress.
type OAuthSignIn struct {
	// ID is the sign-in's id, what the status read names and the state the
	// person's browser carries is signed over.
	ID uuid.UUID `json:"id"`

	// Name is the grant's name in its family, and the record's key.
	Name string `json:"name"`

	// Status is where the sign-in is, and Detail why it failed.
	Status OAuthSignInStatus `json:"status"`
	Detail string            `json:"detail,omitempty"`

	// StartedAt is when it started and ExpiresAt when its state stops being
	// accepted.
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`

	// Sealed is what the callback needs to finish it and nobody else may
	// read (the PKCE verifier, the client, the endpoints), sealed under the
	// fleet keyring with the sign-in's id bound in. The store holds an
	// envelope whose key it does not have.
	Sealed string `json:"sealed"`

	// By, ByKind and OperatorID are who started it on Crewlet, never the
	// vendor account that consented.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Version is the store's version of the record as it was read. Not on
	// the wire.
	Version uint64 `json:"-"`
}

// Validate reports why a sign-in of family f cannot be written.
func (s OAuthSignIn) Validate(f GrantFamily) error {
	switch {
	case !f.Valid():
		return ErrUnknownGrantFamily
	case s.ID == uuid.Nil:
		return errors.New("coord: an OAuth sign-in needs its id")
	case s.Name == "":
		return errors.New("coord: an OAuth sign-in needs the grant it signs in to")
	case !s.Status.Valid():
		return errors.New("coord: an OAuth sign-in needs a status this build names")
	case s.StartedAt.IsZero() || s.ExpiresAt.IsZero():
		return errors.New("coord: an OAuth sign-in needs when it started and when it expires")
	case s.Sealed == "" && !s.Status.Ended():
		return errors.New("coord: an OAuth sign-in in progress needs what its callback finishes it with")
	case s.By == "" || !s.ByKind.Valid():
		return errors.New("coord: an OAuth sign-in needs who started it")
	case len(s.Detail) > MaxGrantWords:
		return fmt.Errorf("coord: an OAuth sign-in's detail is over %d bytes", MaxGrantWords)
	case !plainWithin(s.Name, MaxGrantNameBytes):
		return fmt.Errorf("coord: an OAuth sign-in names a grant past %d plain bytes", MaxGrantNameBytes)
	case !plainWithin(s.Sealed, MaxOAuthSealedBytes):
		return fmt.Errorf("coord: an OAuth sign-in's sealed half is past %d plain bytes: its writer "+
			"refuses to start one whose authorization server's address and lists do not fit",
			MaxOAuthSealedBytes)
	}
	return checkActor("an OAuth sign-in", s.By, s.OperatorID)
}

// MaxOAuthSealedBytes bounds a sign-in's sealed half, an envelope of the
// proof key, the client, the authorization server's endpoints and lists, the
// scopes and the callback address: seven addresses at [MaxGrantAddressBytes]
// and fifty scopes at [MaxGrantScopeBytes] are about 27 KiB, sealed and
// encoded a third more. A sign-in past it is refused where it starts
// (internal/engine), naming the authorization server.
const MaxOAuthSealedBytes = 48 << 10

// MaxOAuthSignInBytes bounds a sign-in's record as stored: its sealed half,
// a detail of [MaxGrantWords] escaped six-fold and its fixed fields. Every
// read of it decides a callback a person waits on (ADR-0175).
const MaxOAuthSignInBytes = MaxOAuthSealedBytes + 8<<10

// Lapsed reports whether the record is past its lifetime at now: it expired
// more than [SignInLinger] ago. A lapsed record is read as absent.
func (s OAuthSignIn) Lapsed(now time.Time) bool {
	return now.After(s.ExpiresAt.Add(SignInLinger))
}

// OAuthSignIns is the fleet's record of the sign-ins in progress, by family.
//
// The store answers what it holds; whether a record has [OAuthSignIn.Lapsed]
// is the reader's to judge against its own clock.
type OAuthSignIns interface {
	// OAuthSignIn reads one grant's sign-in.
	OAuthSignIn(ctx context.Context, f GrantFamily, name string) (OAuthSignIn, bool, error)

	// ListOAuthSignIns returns every sign-in of f, ordered by name.
	ListOAuthSignIns(ctx context.Context, f GrantFamily) ([]OAuthSignIn, error)

	// CreateOAuthSignIn writes a sign-in for a name that has no record,
	// returning it as stored; one that has a record is left alone and
	// reports false.
	CreateOAuthSignIn(ctx context.Context, f GrantFamily, s OAuthSignIn) (OAuthSignIn, bool, error)

	// UpdateOAuthSignIn writes s at s.Version, reporting false when that
	// version no longer holds, including when the record is gone. The one
	// way a state is spent: a callback moves a pending sign-in on at the
	// version it read, and a second callback with the same state finds the
	// version gone.
	UpdateOAuthSignIn(ctx context.Context, f GrantFamily, s OAuthSignIn) (OAuthSignIn, bool, error)

	// DeleteOAuthSignIn removes a sign-in at a version, reporting whether
	// that version still held. A version of zero never holds.
	DeleteOAuthSignIn(ctx context.Context, f GrantFamily, name string, version uint64) (bool, error)
}
