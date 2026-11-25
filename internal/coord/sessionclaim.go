package coord

import (
	"context"
	"errors"
)

// SessionClaims hold, fleet-wide, the one run each minted session id is bound
// to (internal/sandbox's PendingStore.BindSession), so no two runs bind one id
// whatever their nodes: a box may report an id another live box's harness
// minted. One record per id, beside the runs' records. A CLAIM CARRIES ITS
// RUN'S RECORD'S LIFETIME: it goes with the record (the memory twin at once,
// the KV backend off the marker pass, as a run's parts do), and one whose run
// has no record binds nothing, which the claimant decides on, never this store.
type SessionClaims interface {
	// SessionClaim reads the claim of a session id with its revision, false
	// where none stands.
	SessionClaim(ctx context.Context, sessionID string) (SessionClaim, bool, error)
	// CreateSessionClaim writes c, false where its session is claimed already.
	CreateSessionClaim(ctx context.Context, c SessionClaim) (bool, error)
	// MoveSessionClaim rewrites the claim read at revision to c, false where
	// it moved or went.
	MoveSessionClaim(ctx context.Context, c SessionClaim, revision uint64) (bool, error)
	// DeleteSessionClaim removes the claim read at revision, false where it
	// moved or went.
	DeleteSessionClaim(ctx context.Context, sessionID string, revision uint64) (bool, error)
}

// SessionClaim is one minted session id bound to one job of one run's turn.
type SessionClaim struct {
	Session string `json:"session"`
	Turn    string `json:"turn"`
	Launch  string `json:"launch"`
	// Revision is the store's revision of the claim as it was read; not on
	// the wire.
	Revision uint64 `json:"-"`
}

// Validate reports why a claim cannot be written.
func (c SessionClaim) Validate() error {
	if c.Session == "" || c.Turn == "" || c.Launch == "" {
		return errors.New("coord: a session claim names its session, its run's turn and its job")
	}
	for _, f := range []struct{ field, value string }{
		{"session", c.Session}, {"turn", c.Turn}, {"job", c.Launch},
	} {
		if err := holdField("a session claim", f.field, f.value, MaxSessionClaimPartBytes); err != nil {
			return err
		}
	}
	return nil
}

// SessionClaimClass is the key class a claim is filed under, beside the runs'
// records.
const SessionClaimClass = "session_claim"

// SessionClaimKey is a claim's key.
func SessionClaimKey(sessionID string) string { return DocumentKey(SessionClaimClass, sessionID) }
