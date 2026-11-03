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

// AN MCP SERVER'S OAUTH GRANT, and a sign-in in progress that makes one.
//
// A remote MCP server signed in to with OAuth has one grant for the company
// (internal/engine's mcpoauth.go). Its SECRET half (the refresh token, and the
// client a registration issued) is sealed in the secret store under the
// engine's own name for it, `mcp/<server>/refresh`, which no operator surface
// reaches (internal/fleetsecrets' Estate). Its PUBLIC half is here: who signed
// in and when, the authorization server and the client, the scopes granted,
// when it runs out, and why its last renewal failed. Nothing here is a token.
//
// # Who has to agree on them
//
// The whole company, now. Every node that runs the server sends its requests
// under the grant, and every node's status answer says whether it is signed
// in, so a sign-in completed through one node, a renewal refused on another
// and a sign-out on a third are each one record every node watches. A
// sign-in in progress is one record per server for the reason a device
// sign-in is (devicesignin.go): the callback that ends it may arrive at any
// node, and a second sign-in started for the server meanwhile must replace
// the first rather than race it to one grant.
//
// # In the positions register, aged by their own fields
//
// Neither may age by a bucket: a grant is good until somebody signs out or
// removes the server, and a sign-in lapses at its own expiry, which an update
// would restart under a bucket's age (internal/coord/kv's package doc). So
// both live in the register with no age: a grant removed by the decision that
// ends it, and a sign-in read as absent once [MCPSignIn.Lapsed], and replaced
// by the next one started.

// MaxMCPGrantWords bounds each sentence a grant record carries: the vendor's
// words for a renewal it refused, which a person reads beside the server.
// REASONED: an authorization server's error code and description, both cut
// to 512 bytes where they are read (internal/mcp/oauth), and the sentence
// around them.
const MaxMCPGrantWords = 1 << 10

// MaxMCPGrantScopes bounds the scopes a grant record names, as a sign-in asks
// for at most this many (internal/config's MaxMCPOAuthScopes).
const MaxMCPGrantScopes = 50

// MCPGrant is the public half of one server's OAuth grant.
type MCPGrant struct {
	// Server is the configured server's name, and the record's key.
	Server string `json:"server"`

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
	// protected-resource metadata named it: the one address the grant's
	// tokens are sent to, so a server whose configured address moves off it
	// needs a sign-in again rather than taking the grant with it.
	Resource string `json:"resource"`

	// By, ByKind and OperatorID are who signed in, as [SeatPause] records
	// them, and At when.
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

// Validate reports why a grant cannot be written.
func (g MCPGrant) Validate() error {
	switch {
	case g.Server == "":
		return errors.New("coord: an MCP grant needs the server it signs in to")
	case g.Grant == uuid.Nil:
		return errors.New("coord: an MCP grant needs its sign-in's id")
	case g.Client == "" || g.ClientID == "" || g.Issuer == "" || g.Resource == "":
		return errors.New("coord: an MCP grant needs its client, client id, issuer and resource")
	case g.By == "" || !g.ByKind.Valid():
		return errors.New("coord: an MCP grant needs who signed in")
	case g.At.IsZero():
		return errors.New("coord: an MCP grant needs when it was signed in")
	case len(g.Scopes) > MaxMCPGrantScopes:
		return fmt.Errorf("coord: an MCP grant names %d scopes, more than the %d a record holds",
			len(g.Scopes), MaxMCPGrantScopes)
	case len(g.Failure) > MaxMCPGrantWords:
		return fmt.Errorf("coord: an MCP grant's failure is over %d bytes", MaxMCPGrantWords)
	case (g.Failure == "") != (g.FailedAt == nil):
		return errors.New("coord: an MCP grant's failure and when it failed go together")
	}
	return nil
}

// Failed reports whether the grant's last renewal was refused for good.
func (g MCPGrant) Failed() bool { return g.Failure != "" }

// MCPGrantClass is the key class a grant is filed under in the positions
// register.
const MCPGrantClass = "mcp_oauth"

// MCPGrantKey is a server's grant's key in the register.
func MCPGrantKey(server string) string { return DocumentKey(MCPGrantClass, server) }

// MCPGrantServerOf recovers the server from a key of the grant class.
func MCPGrantServerOf(key string) (string, bool) { return serverOf(MCPGrantClass, key) }

// serverOf recovers the server from a key of class, reporting false for a key
// of another class or one this grammar did not write.
func serverOf(class, key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != class || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}

// MCPGrantUpdate is one change a grant watch delivers.
type MCPGrantUpdate struct {
	// Grant is the record the server now holds, nil when it was removed.
	Grant *MCPGrant
	// Server names the server the update is about. Empty on the marker.
	Server string
	// Current marks the end of the records that existed when the watch
	// started, as [SeatPauseUpdate.Current] does.
	Current bool
}

// MCPGrants is the fleet's record of each server's OAuth grant.
//
// Every read RAISES rather than answering empty when the store cannot be
// read: "nobody signed in" and "could not read" are two facts, and a node that
// took the second for the first would stop a server that is signed in.
type MCPGrants interface {
	// MCPGrant reads one server's grant.
	MCPGrant(ctx context.Context, server string) (MCPGrant, bool, error)

	// ListMCPGrants returns every grant, ordered by server.
	ListMCPGrants(ctx context.Context) ([]MCPGrant, error)

	// CreateMCPGrant writes a grant for a server that has none, returning it
	// as stored; one that has a grant is left alone and reports false.
	CreateMCPGrant(ctx context.Context, g MCPGrant) (MCPGrant, bool, error)

	// UpdateMCPGrant writes g at g.Version, reporting false when that
	// version no longer holds, including when the record is gone.
	UpdateMCPGrant(ctx context.Context, g MCPGrant) (MCPGrant, bool, error)

	// DeleteMCPGrant removes a server's grant at a version, reporting
	// whether that version still held. A version of zero never holds.
	DeleteMCPGrant(ctx context.Context, server string, version uint64) (bool, error)

	// WatchMCPGrants streams every grant, then the
	// [MCPGrantUpdate.Current] marker, then every change after it.
	WatchMCPGrants(ctx context.Context) (<-chan MCPGrantUpdate, error)
}

// MCPSignInStatus is where a sign-in is.
type MCPSignInStatus string

const (
	// MCPSignInPending is a sign-in waiting for the person to consent at
	// the vendor.
	MCPSignInPending MCPSignInStatus = "pending"
	// MCPSignInCompleting is a sign-in whose callback arrived and is being
	// exchanged: the state it was started with is spent.
	MCPSignInCompleting MCPSignInStatus = "completing"
	// MCPSignInApproved is a sign-in whose grant was sealed.
	MCPSignInApproved MCPSignInStatus = "approved"
	// MCPSignInFailed is a sign-in that ended without a grant.
	MCPSignInFailed MCPSignInStatus = "failed"
)

// MCPSignInStatuses is every status this build names, in a sign-in's order.
var MCPSignInStatuses = []MCPSignInStatus{
	MCPSignInPending, MCPSignInCompleting, MCPSignInApproved, MCPSignInFailed,
}

// Valid reports whether the status is one this build names.
func (s MCPSignInStatus) Valid() bool { return slices.Contains(MCPSignInStatuses, s) }

// Ended reports whether the sign-in is over.
func (s MCPSignInStatus) Ended() bool { return s == MCPSignInApproved || s == MCPSignInFailed }

// MCPSignInLinger is how long a sign-in's record answers after it expires:
// ten minutes, for [DeviceSignInLinger]'s reason, so a page that polls, or a
// person who looked away, still reads how it ended.
const MCPSignInLinger = 10 * time.Minute

// MCPSignIn is one server's sign-in in progress.
type MCPSignIn struct {
	// ID is the sign-in's id, what the status read names and the state the
	// person's browser carries is signed over.
	ID uuid.UUID `json:"id"`

	// Server is the configured server's name, and the record's key.
	Server string `json:"server"`

	// Status is where the sign-in is, and Detail why it failed.
	Status MCPSignInStatus `json:"status"`
	Detail string          `json:"detail,omitempty"`

	// StartedAt is when it started and ExpiresAt when its state stops being
	// accepted.
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`

	// Sealed is what the callback needs to finish it and nobody else may
	// read (the PKCE verifier, the client and its secret, the endpoints),
	// sealed under the fleet keyring with the sign-in's id bound in. The
	// store holds an envelope whose key it does not have.
	Sealed string `json:"sealed"`

	// By, ByKind and OperatorID are who started it.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Version is the store's version of the record as it was read. Not on
	// the wire.
	Version uint64 `json:"-"`
}

// Validate reports why a sign-in cannot be written.
func (s MCPSignIn) Validate() error {
	switch {
	case s.ID == uuid.Nil:
		return errors.New("coord: an MCP sign-in needs its id")
	case s.Server == "":
		return errors.New("coord: an MCP sign-in needs the server it signs in to")
	case !s.Status.Valid():
		return errors.New("coord: an MCP sign-in needs a status this build names")
	case s.StartedAt.IsZero() || s.ExpiresAt.IsZero():
		return errors.New("coord: an MCP sign-in needs when it started and when it expires")
	case s.Sealed == "" && !s.Status.Ended():
		return errors.New("coord: an MCP sign-in in progress needs what its callback finishes it with")
	case s.By == "" || !s.ByKind.Valid():
		return errors.New("coord: an MCP sign-in needs who started it")
	case len(s.Detail) > MaxMCPGrantWords:
		return fmt.Errorf("coord: an MCP sign-in's detail is over %d bytes", MaxMCPGrantWords)
	}
	return nil
}

// Lapsed reports whether the record is past its lifetime at now: it expired
// more than [MCPSignInLinger] ago. A lapsed record is read as absent.
func (s MCPSignIn) Lapsed(now time.Time) bool {
	return now.After(s.ExpiresAt.Add(MCPSignInLinger))
}

// MCPSignInClass is the key class a sign-in is filed under in the positions
// register.
const MCPSignInClass = "mcp_sign_in"

// MCPSignInKey is a server's sign-in's key in the register.
func MCPSignInKey(server string) string { return DocumentKey(MCPSignInClass, server) }

// MCPSignInServerOf recovers the server from a key of the sign-in class.
func MCPSignInServerOf(key string) (string, bool) { return serverOf(MCPSignInClass, key) }

// MCPSignIns is the fleet's record of the MCP sign-ins in progress.
//
// The store answers what it holds; whether a record has [MCPSignIn.Lapsed] is
// the reader's to judge against its own clock.
type MCPSignIns interface {
	// MCPSignIn reads one server's sign-in.
	MCPSignIn(ctx context.Context, server string) (MCPSignIn, bool, error)

	// ListMCPSignIns returns every sign-in, ordered by server.
	ListMCPSignIns(ctx context.Context) ([]MCPSignIn, error)

	// CreateMCPSignIn writes a sign-in for a server that has no record,
	// returning it as stored; one that has a record is left alone and
	// reports false.
	CreateMCPSignIn(ctx context.Context, s MCPSignIn) (MCPSignIn, bool, error)

	// UpdateMCPSignIn writes s at s.Version, reporting false when that
	// version no longer holds, including when the record is gone. The one
	// way a state is spent: a callback moves a pending sign-in on at the
	// version it read, and a second callback with the same state finds the
	// version gone.
	UpdateMCPSignIn(ctx context.Context, s MCPSignIn) (MCPSignIn, bool, error)

	// DeleteMCPSignIn removes a server's sign-in at a version, reporting
	// whether that version still held. A version of zero never holds.
	DeleteMCPSignIn(ctx context.Context, server string, version uint64) (bool, error)
}
