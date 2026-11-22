package memory

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the minted sessions' claims ---------------------------------------- //

// SessionClaim reads the claim of a session id with its revision.
func (f *Fleet) SessionClaim(_ context.Context, sessionID string) (coord.SessionClaim, bool, error) {
	if sessionID == "" {
		return coord.SessionClaim{}, false, errors.New("coord/memory: a session claim is read by its session id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, held := f.sessionClaims[sessionID]
	return c, held, nil
}

// CreateSessionClaim writes c, false where its session is claimed already.
func (f *Fleet) CreateSessionClaim(_ context.Context, c coord.SessionClaim) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, held := f.sessionClaims[c.Session]; held {
		return false, nil
	}
	f.sessionClaimRevision++
	c.Revision = f.sessionClaimRevision
	f.sessionClaims[c.Session] = c
	return true, nil
}

// MoveSessionClaim rewrites the claim read at revision to c, false where it
// moved or went.
func (f *Fleet) MoveSessionClaim(_ context.Context, c coord.SessionClaim, revision uint64) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.sessionClaims[c.Session]; !ok || held.Revision != revision {
		return false, nil
	}
	f.sessionClaimRevision++
	c.Revision = f.sessionClaimRevision
	f.sessionClaims[c.Session] = c
	return true, nil
}

// DeleteSessionClaim removes the claim read at revision, false where it moved
// or went.
func (f *Fleet) DeleteSessionClaim(_ context.Context, sessionID string, revision uint64) (bool, error) {
	if sessionID == "" || revision == 0 {
		return false, errors.New("coord/memory: a session claim is removed at the revision it was read at")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.sessionClaims[sessionID]; !ok || held.Revision != revision {
		return false, nil
	}
	delete(f.sessionClaims, sessionID)
	return true, nil
}
