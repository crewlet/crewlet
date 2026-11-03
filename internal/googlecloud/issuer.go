package googlecloud

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// SeatIssuer holds the access tokens this process has issued its agents, one per
// account and connection, and asks Google again only when one runs short.
//
// ONE PER PROCESS, shared by every reader of a seat's token (its tool servers,
// its box's launches and the poll that keeps a long run's token file), so an
// account costs one mint an hour however many ask. MEMORY ONLY: a token is
// never logged, sealed or put in an error.
//
// A REFUSAL IS HELD TOO, for [SeatIssuer.RetryWait]: Google's answer for one
// account is handed to every reader asking within the wait, and the first to
// ask after it asks Google again. A refusal is forgotten the moment a mint
// succeeds.
//
// Safe for concurrent use. Two callers that miss at once may both mint; the
// later token replaces the earlier, and both are good until they expire.
type SeatIssuer struct {
	// Now is the clock tokens and refusals are judged against. Nil takes the
	// wall clock.
	Now func() time.Time

	// RetryWait is how long a refused mint is held after the nth refusal in
	// a row for one account, counting from one. Nil holds nothing.
	RetryWait func(n int) time.Duration

	mu      sync.Mutex
	held    map[string]SeatToken
	refused map[string]tokenRefusal
}

// SeatToken is one agent's access token and when Google expires it.
type SeatToken struct {
	Token     string `json:"-"`
	ExpiresAt time.Time
}

// tokenRefusal is the last mint Google refused for one key, the count of
// refusals in a row, and when it is asked again.
type tokenRefusal struct {
	err       error
	n         int
	at, retry time.Time
}

// refusalForgotten is how long past its wait a refusal nobody asks about is
// kept: the hour a token lives, past which its count has nothing to double.
const refusalForgotten = time.Hour

func (i *SeatIssuer) now() time.Time {
	if i.Now != nil {
		return i.Now().UTC()
	}
	return time.Now().UTC()
}

// Token is account's token through c that stays good for at least life,
// from the cache where one does and minted for [MaxTokenLifetime] otherwise.
// account is the service account's unique ID, as the ledger records it.
func (i *SeatIssuer) Token(ctx context.Context, c *Client, account string, life time.Duration) (SeatToken, error) {
	key := c.connection() + "|" + account
	now := i.now()
	i.mu.Lock()
	held, ok := i.held[key]
	refused, wasRefused := i.refused[key]
	// SWEPT on the path every caller takes, so the cache is bounded by what
	// was asked for within the hour.
	for k, t := range i.held {
		if !t.ExpiresAt.After(now) {
			delete(i.held, k)
		}
	}
	for k, r := range i.refused {
		if now.Sub(r.retry) > refusalForgotten {
			delete(i.refused, k)
		}
	}
	i.mu.Unlock()
	if ok && held.ExpiresAt.After(now.Add(life)) {
		return held, nil
	}
	if wasRefused && now.Before(refused.retry) {
		return SeatToken{}, refused.err
	}
	value, expires, err := c.SeatToken(ctx, account, MaxTokenLifetime)
	if err != nil {
		i.refuse(ctx, key, now, err)
		return SeatToken{}, err
	}
	minted := SeatToken{Token: value, ExpiresAt: expires}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.held == nil {
		i.held = map[string]SeatToken{}
	}
	i.held[key] = minted
	delete(i.refused, key)
	return minted, nil
}

// refuse holds err as Google's answer for key for the wait its place in a run
// of refusals earns. asked is when the caller began. A mint the caller's own
// context ended is no answer from Google, and one another caller recorded
// meanwhile is the same run, so neither is counted.
func (i *SeatIssuer) refuse(ctx context.Context, key string, asked time.Time, err error) {
	if i.RetryWait == nil || ctx.Err() != nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	prev, seen := i.refused[key]
	if seen && prev.at.After(asked) {
		return
	}
	n := 1
	if seen {
		n = prev.n + 1
	}
	at := i.now()
	if i.refused == nil {
		i.refused = map[string]tokenRefusal{}
	}
	i.refused[key] = tokenRefusal{err: err, n: n, at: at, retry: at.Add(i.RetryWait(n))}
}

// GrantSettled is when Google has surely applied Crewlet's grant on rec's
// account: [GrantSettle] after the account was made or after the lane's last
// recorded grant, whichever is later. Before it, Google refusing the token is
// Google still applying the grant ([StillGranting]).
func GrantSettled(rec seataccess.Record) time.Time {
	var at time.Time
	if rec.Account != nil {
		at = rec.Account.CreatedAt.Add(GrantSettle)
	}
	if s := rec.Lanes[seataccess.LaneGoogleCloud].Settling; s != nil && s.Reason == ReasonGranting && s.Until.After(at) {
		at = s.Until
	}
	return at
}

// StillGranting reports a seat token Google refused as it refuses one whose
// grant it has not applied yet: 403 or 404 from the mint itself, never from
// reaching Google as the provisioner, which is the connection's fault.
func StillGranting(err error) bool {
	if isStage(err) {
		return false
	}
	status := Status(err)
	return status == http.StatusForbidden || status == http.StatusNotFound
}

// TokenRefusal is why Google would not issue a seat's token, as a clause that
// says what to fix, and whether a wait clears it: Google not reached, its rate
// limit or a 5xx. Never the token: an [*APIError] carries none.
func TokenRefusal(err error) (string, bool) {
	if st, ok := errors.AsType[*stageError](err); ok {
		if passes(st.err) {
			return "Google did not answer Crewlet signing in as the provisioner: " + st.err.Error(), true
		}
		said := "Google refused Crewlet signing in as the provisioner at " + string(st.stage) + " (" +
			st.err.Error() + "): run the Cloud Shell block on the Google Cloud connection again, then press Test"
		return said, false
	}
	if passes(err) {
		return err.Error(), true
	}
	api, ok := errors.AsType[*APIError](err)
	switch {
	case !ok:
		return err.Error(), false
	case api.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(api.Message), "disabled"):
		return "Google says the agent's service account is disabled: turn the agent back on from the " +
			"Google Cloud Agents tab", false
	case StillGranting(err):
		return "Google refused the agent's token (" + api.answer() + "): Crewlet's grant on its service " +
			"account is missing, or the account is gone; Retry the agent on the Google Cloud Agents tab", false
	}
	return "Google refused the agent's token: " + api.answer(), false
}

// passes reports a failure a wait clears: Google not reached, its rate limit,
// or a 5xx.
func passes(err error) bool {
	if api, ok := errors.AsType[*APIError](err); ok {
		return api.Status == http.StatusTooManyRequests || api.Status >= http.StatusInternalServerError
	}
	return errors.Is(err, errUnreached)
}
