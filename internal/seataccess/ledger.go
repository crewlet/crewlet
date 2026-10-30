package seataccess

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
)

// AccountState is where an account the engine created stands, as the ledger
// records it.
type AccountState string

const (
	// AccountActive is an account the engine created and has not disabled.
	AccountActive AccountState = "active"

	// AccountDisabled is an account THIS ENGINE disabled (a seat turned off).
	// It is what tells a disable the engine performed from one somebody
	// performed at the vendor (D3).
	AccountDisabled AccountState = "disabled"

	// AccountClosed is an account the vendor shows deleted. The record stays
	// for the seat's history until the next create replaces it.
	AccountClosed AccountState = "closed"
)

// Valid reports whether s is a state this build writes.
func (s AccountState) Valid() bool {
	return s == AccountActive || s == AccountDisabled || s == AccountClosed
}

// Account is one ledger row: an account this engine created at a vendor.
//
// THE LEDGER IS WHAT LETS A PASS REACH A SEAT THAT LEFT THE DOCUMENT. Every
// teardown before it walked the current plan, so a seat removed from the
// document orphaned a live account nothing would ever disable.
type Account struct {
	// ID is the vendor's identifier for the account (a Datadog user id, a
	// GitLab user id, an Atlassian account id, a Mattermost user id, a Slack
	// app id, a GitHub App id).
	ID string `json:"vendor_account_id"`

	// Name is the vendor's own name for it, for a person to find it there.
	Name string `json:"name,omitempty"`

	// CreatedAt is when the engine created it, and CreatedByPass the sync
	// that did, for an audit to find the run.
	CreatedAt     time.Time `json:"created_at"`
	CreatedByPass string    `json:"created_by_pass"`

	// State is where it stands.
	State AccountState `json:"state"`

	// Adopted marks an account this engine found under its own naming with
	// no ledger row (one made before the ledger existed) and recorded rather
	// than creating a second; CreatedByPass is then the sync that adopted
	// it, and CreatedAt the vendor's own date where it gave one.
	Adopted bool `json:"adopted,omitempty"`
}

// LaneRecord is what the ledger keeps for one seat's lane between passes.
type LaneRecord struct {
	// Applied is the document's setting as of the last sync that converged
	// the lane. It is what tells drift from an edit (D2): the vendor moving
	// away from Applied while the document still asks for it is drift, and
	// the document moving away from Applied is an edit.
	Applied *Desired `json:"applied,omitempty"`

	// Observed is what the vendor showed on the last sync.
	Observed Observed `json:"observed"`

	// Sync is what the last sync did.
	Sync *SyncResult `json:"sync,omitempty"`
}

// Record is the ledger's record for one seat at one kind: the account, if the
// engine created one, and each lane's last sync. A seat with no account still
// has a record once a sync has run for it, so a failed create is reported.
type Record struct {
	Kind   integration.Kind `json:"kind"`
	Handle string           `json:"handle"`

	// Name is the seat's display name when the record was last written, for
	// a seat that has left the document.
	Name string `json:"name,omitempty"`

	// Account is the ledger row; nil where no account exists.
	Account *Account `json:"account,omitempty"`

	// Credential is the seat's sealed credential for this kind; nil where
	// nothing is sealed.
	Credential *Credential `json:"credential,omitempty"`

	// Lanes are the kind's lanes the seat has a record on.
	Lanes map[Lane]LaneRecord `json:"lanes,omitempty"`

	// SharedCredential is a company credential this seat's own credential
	// was kept in before per-seat access (the incident D12 repairs),
	// recorded by the migration: the integration's Agents tab warns that
	// the company credential may hold the seat's value until it is
	// replaced. Nil for every seat that never shared one.
	SharedCredential *SharedCredential `json:"shared_credential,omitempty"`
}

// SharedCredential is a company credential a seat once kept its own
// credential in, by name, and when the migration found it.
type SharedCredential struct {
	Variable string    `json:"variable"`
	RaisedAt time.Time `json:"raised_at"`
}

// Held reports a record whose account exists and has not been closed.
func (r Record) Held() bool {
	return r.Account != nil && r.Account.State != AccountClosed
}

// Ledger is the fleet's record of the accounts the engine created and each
// seat's last sync, per kind.
//
// Every writer holds the kind's provisioning lease while it writes (the
// reconcile pass, and an access route's seat action), so a plain put cannot
// lose an update: the writes are the tail of work the lease already
// serializes, which is coord.Integrations's argument.
type Ledger interface {
	// Records is every record for kind, by handle. RAISES on an unreachable
	// store: "no accounts" and "cannot tell" send a pass down opposite paths.
	Records(ctx context.Context, kind integration.Kind) (map[string]Record, error)

	// Put records one seat's record, replacing any prior one.
	Put(ctx context.Context, rec Record) error

	// Drop removes one seat's record: after a remove, or for a seat whose
	// account is gone and that the document no longer has.
	Drop(ctx context.Context, kind integration.Kind, handle string) error
}
