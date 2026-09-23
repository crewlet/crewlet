package iamdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
)

// AN OIDC SESSION'S REFRESH TOKEN, and the deactivation probe's view of this
// estate.
//
// # Custody is the fleet secret store, and never the log
//
// A refresh token is a credential at somebody else's identity provider, so it
// is sealed in the company's secret store under the session's own lineage
// ([SessionRefreshName]) and nowhere else: not on a record, not in a row, not
// in a snapshot. Nothing on the request path reads it — the readers are the
// probe and the key duty's collection ([Refreshes.CollectEnded]), one node
// each, once an interval. The design this replaced re-sealed the
// token into the log on every hourly cookie rotation, an eleven-fold amplifier
// on the log's bytes for a value that changes only at an exchange.
//
// # A grant knows which session it belongs to, and where that session began
//
// The value carries the lineage, the person and the START POSITION of the
// session it was obtained for. The position is what makes collecting a stale
// grant safe on a fleet: a login publishes its session record without waiting
// for its own apply, so the probe can meet a grant whose session this node has
// not applied yet — and "no such session here" means "gone" only once this
// node has applied past the position the session began at. Below it, it means
// "not arrived", and deleting the grant then would blind the probe to a live
// session until it expired.

// RefreshStore is the fleet secret store as custody uses it: a key store that
// can also say which names exist.
type RefreshStore interface {
	Keys
	KeyIndex
}

// RefreshGrant is what the probe asks an identity provider with.
type RefreshGrant struct {
	V int `json:"v"`

	// Lineage and Person name the session it belongs to.
	Lineage string `json:"lineage"`
	Person  string `json:"person"`

	// Issuer is the provider that issued it. A deployment that changes
	// its provider does not present the old one's tokens to the new one:
	// the answer would be `invalid_grant`, read as a deactivation, and
	// every session opened before the change would end for no reason
	// anybody could find.
	Issuer string `json:"issuer"`

	// Token is the refresh token itself.
	Token string `json:"token"`

	// Start is the composed position the session's own record landed at.
	Start uint64 `json:"start"`
}

// refreshGrantVersion is the shape this build writes and reads.
const refreshGrantVersion = 1

// Refreshes keeps the refresh tokens the deactivation probe asks with.
type Refreshes struct {
	keys RefreshStore
	by   string
}

// NewRefreshes builds custody over the company's secret store, writing as by.
func NewRefreshes(keys RefreshStore, by string) (*Refreshes, error) {
	switch {
	case keys == nil:
		return nil, errors.New("iamdomain: refresh custody needs the company's " +
			"secret store — without one there is nowhere a refresh token may " +
			"live that is not the log")
	case by == "":
		return nil, errors.New("iamdomain: refresh custody needs to say who " +
			"wrote a grant, which is what a secret listing shows beside it")
	}
	return &Refreshes{keys: keys, by: by}, nil
}

// Hold takes custody of one session's refresh token.
func (r *Refreshes) Hold(ctx context.Context, grant RefreshGrant, now time.Time) error {
	switch {
	case grant.Lineage == "" || grant.Person == "":
		return errors.New("iamdomain: a refresh grant names its session and its person")
	case grant.Issuer == "" || grant.Token == "":
		return errors.New("iamdomain: a refresh grant carries its issuer and its token")
	case grant.Start == 0:
		return errors.New("iamdomain: a refresh grant carries the position its " +
			"session began at, or a node that has not applied that session " +
			"would collect the grant as belonging to nobody")
	}
	grant.V = refreshGrantVersion
	body, err := json.Marshal(grant)
	if err != nil {
		return fmt.Errorf("iamdomain: encode a refresh grant: %w", err)
	}
	return r.keys.Set(ctx, SessionRefreshName(grant.Lineage), string(body),
		r.by, "iam", now)
}

// Held reads every grant in custody.
//
// PARTIAL ON FAILURE, and the three answers come back apart: the grants that
// read; one error per grant that would not open or decode, each of which is
// one session the probe cannot ask about this interval; and an error for a
// listing that failed whole. They are apart rather than joined because a
// joined error beside a list reads, to every caller that checks `err != nil`
// first, as "nothing here" — which is how one unreadable grant used to stop
// every session in the company being asked about.
func (r *Refreshes) Held(ctx context.Context) (grants []RefreshGrant,
	unreadable []error, err error) {

	listed, err := r.keys.Keys(ctx, sessionRefreshPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("iamdomain: list the refresh grants: %w", err)
	}
	for _, key := range listed {
		name := key.Name
		if !strings.HasSuffix(name, sessionRefreshSuffix) {
			continue
		}
		grant, err := r.read(ctx, name)
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			// DESTROYED BETWEEN THE LISTING AND THE READ, by a
			// sign-out or a peer's pass: a grant that is gone.
			continue
		case err != nil:
			unreadable = append(unreadable, err)
			continue
		}
		grants = append(grants, grant)
	}
	return grants, unreadable, nil
}

// read opens one grant by its name.
func (r *Refreshes) read(ctx context.Context, name string) (RefreshGrant, error) {
	value, err := r.keys.Get(ctx, name)
	if err != nil {
		return RefreshGrant{}, err
	}
	var grant RefreshGrant
	if err := json.Unmarshal([]byte(value), &grant); err != nil {
		return RefreshGrant{}, fmt.Errorf("iamdomain: %s is not a refresh grant: %w",
			name, err)
	}
	if grant.V != refreshGrantVersion || grant.Lineage == "" ||
		SessionRefreshName(grant.Lineage) != name {
		// THE NAME AND THE CONTENT MUST AGREE: a grant filed under one
		// session and claiming another would have the probe end the
		// wrong person's session on the first invalid_grant.
		return RefreshGrant{}, fmt.Errorf("iamdomain: %s holds a refresh grant "+
			"this build cannot read (version %d, lineage %q)", name, grant.V,
			grant.Lineage)
	}
	return grant, nil
}

// Rotate records the token a provider replaced the old one with.
//
// A PROVIDER THAT ROTATES HAS RETIRED THE OLD TOKEN, so a rotation that is not
// recorded makes the next probe present a token the provider refuses — which
// reads as a deactivation, and ends the session of somebody perfectly employed.
func (r *Refreshes) Rotate(ctx context.Context, lineage, token string, now time.Time) error {
	if token == "" {
		return errors.New("iamdomain: a rotation carries the new token")
	}
	grant, err := r.read(ctx, SessionRefreshName(lineage))
	if err != nil {
		return fmt.Errorf("iamdomain: read session %s's grant to rotate it: %w",
			lineage, err)
	}
	grant.Token = token
	return r.Hold(ctx, grant, now)
}

// Drop destroys one session's grant, reporting whether it was there.
func (r *Refreshes) Drop(ctx context.Context, lineage string) (bool, error) {
	return r.keys.Unset(ctx, SessionRefreshName(lineage))
}

// CollectEnded destroys every grant whose session is over, by whichever lever
// ended it, and reports what it collected and every grant it could not judge
// or drop — one error each, beside the rest.
//
// # One collector, and it is the key duty rather than the probe
//
// A sign-out ends one lineage, but a revocation, a company-wide invalidation,
// an expiry and a removal end sessions with no per-lineage write at all, so
// only a pass over every grant sees all of them end. That pass used to be the
// deactivation PROBE's — and the probe is armed only while a provider is
// configured, so a deployment that dropped its `oidc` block kept every
// session's refresh token, a live credential at that provider, for ever. The
// key duty is armed wherever the company's secret store is, which is wherever a
// grant can exist.
//
// AN UNSEEN SESSION IS KEPT: its row is missing because it has not arrived on
// this node, and collecting its grant would blind the probe to a live session
// until it expired. See the file's header.
func (r *Refreshes) CollectEnded(ctx context.Context, reader *Reader,
	now time.Time) (collected []string, skipped []error, err error) {

	if reader == nil {
		return nil, nil, errors.New("iamdomain: collecting ended grants needs " +
			"the directory their sessions are judged in")
	}
	grants, skipped, err := r.Held(ctx)
	if err != nil || len(grants) == 0 {
		return nil, skipped, err
	}
	states, err := reader.SessionStates(ctx, grants, now)
	if err != nil {
		return nil, skipped, err
	}
	for _, grant := range grants {
		if states[grant.Lineage] != SessionOver {
			continue
		}
		if _, err := r.Drop(ctx, grant.Lineage); err != nil {
			skipped = append(skipped, fmt.Errorf("iamdomain: collect session "+
				"%s's grant: %w", grant.Lineage, err))
			continue
		}
		collected = append(collected, grant.Lineage)
	}
	return collected, skipped, nil
}

// SessionState is what this node can say about one session, three ways.
type SessionState string

const (
	// SessionLive is open, inside its deadline, at its person's current
	// revocation epoch and after the company's last invalidation.
	SessionLive SessionState = "live"

	// SessionOver is a session nothing can present any more: ended,
	// expired, revoked, invalidated, or swept and removed — its grant is
	// custody of nothing.
	SessionOver SessionState = "over"

	// SessionUnseen is a session this node has not applied yet. NOT
	// "over": its row is missing because it has not arrived.
	SessionUnseen SessionState = "unseen"
)

// Valid reports whether a state is one of the three.
func (s SessionState) Valid() bool {
	switch s {
	case SessionLive, SessionOver, SessionUnseen:
		return true
	}
	return false
}

// SessionStates answers the three-valued state of each grant's session, in
// ONE snapshot, against this node's rows at `now`.
func (r *Reader) SessionStates(ctx context.Context, grants []RefreshGrant,
	now time.Time) (map[string]SessionState, error) {

	out := make(map[string]SessionState, len(grants))
	applied := uint64(r.committed().Packed())
	err := r.scan(ctx, func(tx *sql.Tx) error {
		var invalidated int64
		err := tx.QueryRowContext(ctx,
			`SELECT version FROM iam_session_generation WHERE singleton = 0`).
			Scan(&invalidated)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("iamdomain: read the session generation: %w", err)
		}
		for _, grant := range grants {
			state, err := sessionState(ctx, tx, grant, applied,
				uint64(invalidated), now)
			if err != nil {
				return err
			}
			out[grant.Lineage] = state
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sessionState is one grant's session, read inside the caller's snapshot.
func sessionState(ctx context.Context, tx *sql.Tx, grant RefreshGrant,
	applied, invalidated uint64, now time.Time) (SessionState, error) {

	var (
		person                       string
		epoch, start, expires, ended int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT person_id, epoch, start_position, absolute_expires_at, ended_at
		FROM iam_sessions WHERE lineage = ?`, grant.Lineage).
		Scan(&person, &epoch, &start, &expires, &ended)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// ABSENT MEANS GONE ONLY PAST WHERE THE SESSION BEGAN — see the
		// file's header.
		if applied >= grant.Start {
			return SessionOver, nil
		}
		return SessionUnseen, nil
	case err != nil:
		return "", fmt.Errorf("iamdomain: read session %s: %w", grant.Lineage, err)
	}
	switch {
	case ended != 0,
		expires > 0 && expires <= now.UnixMilli(),
		// A SESSION OPENED BEFORE THE COMPANY'S LAST INVALIDATION carries
		// the generation that invalidation ended.
		invalidated > 0 && uint64(start) < invalidated:
		return SessionOver, nil
	}
	var current int64
	err = tx.QueryRowContext(ctx,
		`SELECT epoch FROM iam_revocation_epochs WHERE person_id = ?`, person).
		Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", fmt.Errorf("iamdomain: read person %s's epoch: %w", person, err)
	case epoch < current:
		return SessionOver, nil
	}
	return SessionLive, nil
}

// ProbeSessions is the identity estate as the deactivation probe asks it.
//
// It satisfies [oidc.Sessions], and it is where a probe's three verbs become
// this estate's: the live sessions are custody's grants whose sessions this
// node holds open, an ended session is a close record on its subject, and a
// rotation is custody's.
type ProbeSessions struct {
	reader  *Reader
	writer  *Writer
	custody *Refreshes
	issuer  string
	now     func() time.Time
}

// NewProbeSessions builds the probe's view for one provider.
func NewProbeSessions(reader *Reader, writer *Writer, custody *Refreshes,
	issuer string, now func() time.Time) (*ProbeSessions, error) {

	switch {
	case reader == nil || writer == nil || custody == nil:
		return nil, errors.New("iamdomain: the deactivation probe needs the " +
			"directory, a writer and refresh custody")
	case issuer == "":
		return nil, errors.New("iamdomain: the deactivation probe asks ONE " +
			"provider, and was not told which")
	}
	if now == nil {
		now = time.Now
	}
	return &ProbeSessions{reader: reader, writer: writer, custody: custody,
		issuer: issuer, now: now}, nil
}

var _ oidc.Sessions = (*ProbeSessions)(nil)

// LiveOIDC lists the sessions the probe may ask about: open, this provider's,
// and holding a refresh token.
//
// IT COLLECTS NOTHING. A grant whose session is over is left for the key duty
// ([Refreshes.CollectEnded]), which is armed wherever a grant can exist — the
// probe is armed only while a provider is configured, and a collector that
// lived here stopped collecting the day a deployment dropped its `oidc` block.
//
// ONE GRANT'S FAILURE IS ONE SESSION'S: a grant that will not read is carried
// in [oidc.Listing.Skipped], and the rest are handed over. The error is for a
// pass that could establish nothing — no listing, or no snapshot to judge the
// sessions in.
func (p *ProbeSessions) LiveOIDC(ctx context.Context) (oidc.Listing, error) {
	grants, unreadable, err := p.custody.Held(ctx)
	if err != nil {
		return oidc.Listing{}, err
	}
	listing := oidc.Listing{Skipped: unreadable}
	if len(grants) == 0 {
		return listing, nil
	}
	states, err := p.reader.SessionStates(ctx, grants, p.now())
	if err != nil {
		return oidc.Listing{}, err
	}
	for _, grant := range grants {
		if states[grant.Lineage] != SessionLive {
			continue
		}
		if grant.Issuer != p.issuer {
			// ANOTHER PROVIDER'S TOKEN, from before the deployment
			// changed issuer: asking this one would answer
			// invalid_grant and end a session nobody deactivated.
			continue
		}
		listing.Sessions = append(listing.Sessions, oidc.LiveSession{
			Lineage: grant.Lineage, Person: grant.Person,
			Refresh: grant.Token,
		})
	}
	return listing, nil
}

// End closes one session for the probe, and destroys its grant.
//
// THE CLOSE IS THE NODE'S OWN RECORD on the session's subject, under an
// operation id derived from the lineage and the reason, so a retry of an
// ambiguous close is the same operation. AN UNKNOWN OUTCOME IS AN ERROR: the
// probe counts what it ended, and a close nothing can establish is not one.
func (p *ProbeSessions) End(ctx context.Context, session oidc.LiveSession,
	reason string) error {

	lineage := session.Lineage
	at, err := p.writer.CloseSession(ctx, lineage, session.Person, reason,
		"probe:"+reason+":"+lineage)
	if err != nil {
		return err
	}
	if at.Seq == 0 {
		return fmt.Errorf("iamdomain: the close of session %s as %s has an "+
			"unknown outcome; the next pass asks again", lineage, reason)
	}
	// THE GRANT GOES AFTER THE CLOSE, and a failure here is SAID rather
	// than returned: the session did end, which is what the probe counts,
	// and the key duty's next pass collects the grant because it will find
	// the session over ([Refreshes.CollectEnded]).
	if _, err := p.custody.Drop(ctx, lineage); err != nil {
		dutyLog.WarnContext(ctx, "iam_refresh_grant_kept",
			"lineage", lineage, "error", err.Error(),
			"detail", "the session ended and its refresh token is still in "+
				"custody; the key duty's next pass collects it")
	}
	return nil
}

// dutyLog is the identity duties' own voice: the probe's custody, the key
// duty and the claim report say what they found under one name an operator
// can filter on.
var dutyLog = logging.Get("iam.duty")

// Rotated records a token the provider replaced.
func (p *ProbeSessions) Rotated(ctx context.Context, lineage, refresh string) error {
	return p.custody.Rotate(ctx, lineage, refresh, p.now())
}
