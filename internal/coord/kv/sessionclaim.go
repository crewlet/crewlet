package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the minted sessions' claims ---------------------------------------- //

// SessionClaim reads the claim of a session id with its revision, from the
// runs bucket the claims are kept in beside the records whose lifetime they
// carry.
func (f *FleetStore) SessionClaim(ctx context.Context, sessionID string) (coord.SessionClaim, bool, error) {
	if sessionID == "" {
		return coord.SessionClaim{}, false, errors.New("coord/kv: a session claim is read by its session id")
	}
	entry, err := f.get(ctx, f.runs, coord.SessionClaimKey(sessionID))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.SessionClaim{}, false, nil
	case err != nil:
		return coord.SessionClaim{}, false, unavailable("read the claim of session "+sessionID, err)
	}
	c, err := decodeSessionClaim(entry)
	return c, err == nil, err
}

// decodeSessionClaim is the claim entry holds, with its revision.
func decodeSessionClaim(entry jetstream.KeyValueEntry) (coord.SessionClaim, error) {
	var c coord.SessionClaim
	if err := json.Unmarshal(entry.Value(), &c); err != nil {
		return coord.SessionClaim{}, fmt.Errorf("coord/kv: decode the session claim %s: %w", entry.Key(), err)
	}
	c.Revision = entry.Revision()
	return c, nil
}

// CreateSessionClaim writes c, false where its session is claimed already.
func (f *FleetStore) CreateSessionClaim(ctx context.Context, c coord.SessionClaim) (bool, error) {
	raw, err := encodeSessionClaim(c)
	if err != nil {
		return false, err
	}
	_, err = f.create(ctx, f.runs, coord.SessionClaimKey(c.Session), raw)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	case err != nil:
		return false, unavailable("claim session "+c.Session, err)
	}
	return true, nil
}

// MoveSessionClaim rewrites the claim read at revision to c, false where it
// moved or went.
func (f *FleetStore) MoveSessionClaim(ctx context.Context, c coord.SessionClaim, revision uint64) (bool, error) {
	raw, err := encodeSessionClaim(c)
	if err != nil {
		return false, err
	}
	_, err = f.runs.Update(ctx, coord.SessionClaimKey(c.Session), raw, revision)
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	}
	return false, unavailable("move the claim of session "+c.Session, err)
}

// DeleteSessionClaim removes the claim read at revision, false where it moved
// or went.
func (f *FleetStore) DeleteSessionClaim(ctx context.Context, sessionID string, revision uint64) (bool, error) {
	if sessionID == "" || revision == 0 {
		return false, errors.New("coord/kv: a session claim is removed at the revision it was read at")
	}
	err := f.runs.Purge(ctx, coord.SessionClaimKey(sessionID), jetstream.LastRevision(revision))
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	}
	return false, unavailable("remove the claim of session "+sessionID, err)
}

// encodeSessionClaim is c on the wire, refused where it names no session, turn
// or job.
func encodeSessionClaim(c coord.SessionClaim) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode a session claim: %w", err)
	}
	return raw, nil
}

// sweepSessionClaims removes every claim written before cutoff whose run has no
// record, off the runs bucket's marker pass, as a run's parts are swept
// ([FleetStore.sweepRunParts]): the run's absence confirmed from the LEADER,
// the claim purged through the revision the pass saw, sparing one moved since.
// The pass walks metadata alone, so a candidate's value is read to name its run;
// one that does not decode names none, and goes.
func (f *FleetStore) sweepSessionClaims(ctx context.Context, pass sweepPass, cutoff time.Time) (int64, error) {
	what := "the minted sessions' claims"
	keys := make([]string, 0)
	for key, kve := range pass.keys {
		segments, ok := coord.DocumentSegments(key)
		if ok && len(segments) == 2 && segments[0] == coord.SessionClaimClass &&
			kve.Operation() == jetstream.KeyValuePut && kve.Created().Before(cutoff) {
			keys = append(keys, key)
		}
	}
	// KEY ORDER, so a failure part-way names the same key on every retry.
	slices.Sort(keys)
	var swept int64
	for _, key := range keys {
		kve := pass.keys[key]
		entry, err := f.get(ctx, f.runs, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			continue
		case err != nil:
			return swept, unavailable("sweep "+what, fmt.Errorf("read the claim %q: %w", key, err))
		}
		claim, err := decodeSessionClaim(entry)
		if err == nil {
			_, err = f.get(ctx, f.runs, encodeKey(claim.Turn))
			switch {
			case err == nil:
				continue
			case !errors.Is(err, jetstream.ErrKeyNotFound):
				return swept, unavailable("sweep "+what, fmt.Errorf("read the run of %q: %w", key, err))
			}
		}
		n, err := pass.purge.through(ctx, key, kve.Revision())
		if err != nil {
			return swept, unavailable("sweep "+what, fmt.Errorf("remove the claim %q: %w", key, err))
		}
		swept += int64(n)
	}
	return swept, nil
}
