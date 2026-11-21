package googleauth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// SeatIssuer holds the access tokens this process has issued its agents, one per
// connection, account and scope set, and asks Google again only when one runs
// short.
//
// ONE PER PROCESS, shared by every reader of a seat's token, so an account
// costs one mint an hour however many ask. MEMORY ONLY: a token is never
// logged, sealed or put in an error.
//
// A REFUSAL IS HELD TOO, for [SeatIssuer.RetryWait]; a successful mint
// forgets it. Safe for concurrent use: two callers that miss at once may both
// mint, the later token replaces the earlier, and both are good until expiry.
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

// Token is account's token with scopes through c that stays good for at least
// life, from the cache where one does and minted for [MaxTokenLifetime]
// otherwise. account is the service account's unique ID, as its owner records
// it.
func (i *SeatIssuer) Token(
	ctx context.Context, c *Connection, account string, scopes []string, life time.Duration,
) (SeatToken, error) {
	// A CALLER'S OWN MISTAKE IS NO REFUSAL OF GOOGLE'S, so it is answered at
	// once and never held for a wait.
	if strings.TrimSpace(account) == "" || len(scopes) == 0 {
		return SeatToken{}, errors.New("googleauth: a seat token names its account and its scopes")
	}
	key := c.ID() + "|" + account + "|" + scopeKey(scopes)
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
	value, expires, err := c.SeatToken(ctx, account, scopes, MaxTokenLifetime)
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
