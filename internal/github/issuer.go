package github

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// An agent acts on GitHub as its OWN app, with a token issued at use.
//
// What a seat's app ([SeatApp]) acts with is an INSTALLATION TOKEN: minted from
// the app's key, scoped to the seat's tier and repositories, valid one hour,
// and the only thing GitHub accepts from a git push, `gh pr create` or the
// MCP server, so it must reach every place the seat works.
//
// ISSUED, NEVER SEALED: the token expires within the hour and any node holding
// the key can mint it, so a replicated copy would be a live credential in
// every node and backup for nothing a fresh mint does not give. It lives in an
// [Issuer]'s memory only.

// The fields a seat's app keeps under its own name ([SeatVar]).
//
// Configuration references each as a `${VAR}` answered by the seat's OWN
// resolution: its token is laid in front of the company's sources for it and
// no other seat, so a seat naming another's variable names something nothing
// answers, which keeps an app's identity with its agent.
const (
	// FieldAppKey is the app's PEM private key, sealed once at creation.
	FieldAppKey = "APP_KEY"

	// FieldAppWebhookSecret is the secret GitHub generated for the app's
	// own webhook, sealed once at creation when GitHub returned one.
	FieldAppWebhookSecret = "APP_WEBHOOK_SECRET"

	// FieldAppToken is the installation token the engine issues at use
	// ([Issuer]). Never sealed: see the file comment.
	FieldAppToken = "APP_TOKEN"
)

// SeatVar is the variable one seat's app value lives under, for field.
//
// ONE NAMING, for the values the app's creation seals and for the token the
// engine issues, so the three cannot drift into two grammars. It is
// [seataccess.SinkName]'s, the name the access sync seals under, which puts
// the vendor first: a handle beginning
// with a digit (`7th-engineer`, which the org model accepts) would
// otherwise make a name no `${VAR}` can reference (see that function).
// `senior-engineer`'s token is GITHUB_APP_TOKEN_SENIOR_ENGINEER.
func SeatVar(handle, field string) string {
	return seataccess.SinkName(integration.KindGitHub, field, handle)
}

// Token lifetimes a caller asks the [Issuer] for.
//
// GitHub issues an installation token for exactly one hour and offers no way
// to ask for longer, so every lifetime here is a share of that hour, and the
// question each answers is how long the token has to stay good AFTER the
// caller has it.
const (
	// ToolTokenLife is what a tool server on this node is started with.
	// [RenewBefore]: the server is restarted with a fresh token when the
	// one it holds reaches this much life ([Issuer.RenewAt]), so it never
	// carries one past its expiry.
	ToolTokenLife = RenewBefore

	// RunTokenLife is what an executor's environment is given at launch.
	//
	// FIFTY MINUTES, the most an hour-long token can promise while still
	// letting the issuer hand one token to the launches of the next ten
	// minutes rather than minting per launch. A run's environment is fixed
	// when its box starts, so this is how long `$GITHUB_TOKEN` in that box
	// stays good. A run that lasts longer reads the token through a file
	// the completion poll keeps current ([RefreshTokenLife]).
	RunTokenLife = 50 * time.Minute

	// RefreshTokenLife is what a running box's token file is kept at.
	//
	// [RenewBefore], plus nothing for the poll's own cadence: the poll
	// runs every fifteen seconds, so a file is rewritten within seconds of
	// the token behind it being renewed, and a ten-minute margin is forty
	// polls wide.
	RefreshTokenLife = RenewBefore
)

// ErrNotInstalled reports a seat whose app cannot issue a token yet: it has
// not been created, or not installed, or its key does not resolve. Each is a
// step a person takes at GitHub or in the dashboard, which the access sync
// reports on the integration's Agents tab.
var ErrNotInstalled = errors.New("github: this agent's app is not created and installed yet")

// ErrSeatOff reports a seat switched off on GitHub (D1): it is issued no
// token until it is turned back on from the integration's Agents tab.
var ErrSeatOff = errors.New("github: this agent is turned off on GitHub")

// Issuer holds the installation tokens this process has minted, one per
// installation and scope, and mints again when one runs short.
//
// ONE PER PROCESS, shared by everything that reads a seat's token, so one
// installation costs one mint an hour. MEMORY ONLY: never logged, stored or
// put in an error ([Issuer.Token]). A REFUSAL IS HELD TOO, for
// [Issuer.RetryWait]: an installation GitHub refuses until a person acts costs
// one request per wait, not one per reader.
//
// Safe for concurrent use. Callers missing at once may both mint; the later
// token replaces the earlier and both stay valid, cheaper than a per-key lock
// holding every caller behind GitHub's slowest answer.
type Issuer struct {
	// Now is the clock tokens and refusals are judged against. Nil takes the
	// wall clock.
	Now func() time.Time

	// RetryWait is how long a refused mint is held after the nth refusal in
	// a row for one installation and scope, counting from one, before GitHub
	// is asked again. NIL HOLDS NOTHING, and every read asks GitHub, which
	// suits only an issuer each refusal reaches once; the engine's is held on
	// the restart schedule its seats' tool servers are retried on.
	RetryWait func(n int) time.Duration

	mu      sync.Mutex
	held    map[issueKey]InstallationToken
	refused map[issueKey]refusal
}

// refusal is the last mint GitHub refused for one key, and when to ask again.
type refusal struct {
	err error
	// signer is a digest of the private key the refused mint was signed
	// with. A refusal answers that key alone: an operator who seals a new
	// key after a revoked one is asking a different question, which is
	// asked at once rather than after the wait the old key earned.
	signer [sha256.Size]byte
	// n counts the refusals in a row, which doubles the wait.
	n int
	// at is when GitHub refused, and retry when it is asked again.
	at, retry time.Time
}

// refusalForgotten is how long past its wait a refusal no reader has asked
// about is kept: an hour, the whole life GitHub issues a token for. A key no
// reader asks about for that long is retried by nobody, so its count of
// refusals in a row has nothing left to double, and the cache stays bounded by
// what readers ask for.
const refusalForgotten = time.Hour

// issueKey is what one token is valid FOR: the app and installation it was
// minted against, and the scope it was narrowed to. Two seats can share an
// installation only by sharing an app, which the engine never creates, but
// the scope is in the key regardless: a token narrowed to one access or one
// set of repositories must never be handed to a caller asking for another.
type issueKey struct {
	app, installation int64
	perms             string
	repos             string
}

func keyOf(seat SeatApp) issueKey {
	repos := slices.Clone(seat.Repos)
	for i := range repos {
		repos[i] = strings.ToLower(strings.TrimSpace(repos[i]))
	}
	slices.Sort(repos)
	return issueKey{
		app: seat.AppID, installation: seat.InstallationID,
		perms: seat.Permissions.key(), repos: strings.Join(slices.Compact(repos), ","),
	}
}

func (i *Issuer) now() time.Time {
	if i.Now != nil {
		return i.Now().UTC()
	}
	return time.Now().UTC()
}

// Issuable reports whether seat's app can issue a token at all: on, created,
// installed, and holding a key. A seat for which this is false is waiting on
// a person, never on GitHub.
func Issuable(seat SeatApp) bool {
	return !seat.Off && seat.AppID != 0 && seat.InstallationID != 0 && strings.TrimSpace(seat.Key) != ""
}

// Token is a token for seat's app that stays good for at least life, from
// the cache where one does and minted otherwise.
//
// A MINTED TOKEN IS RETURNED WHATEVER LIFE IT HAS: GitHub decides how long
// one lives, so a caller asking for more than an hour gets the hour.
//
// A REFUSAL HELD FOR THE KEY is returned without a request until its wait
// ends (see the type); a held token good for life is returned before that, so
// a refused renewal costs nothing to a reader the held token still serves.
//
// The error never carries the token, nor GitHub's response body, which on the
// token endpoint is the token.
func (i *Issuer) Token(
	ctx context.Context, opts SeatAppOptions, seat SeatApp, life time.Duration,
) (InstallationToken, error) {
	if seat.Off {
		return InstallationToken{}, ErrSeatOff
	}
	if !Issuable(seat) {
		return InstallationToken{}, ErrNotInstalled
	}
	key := keyOf(seat)
	signer := sha256.Sum256([]byte(seat.Key))
	now := i.now()

	i.mu.Lock()
	held, ok := i.held[key]
	refused, wasRefused := i.refused[key]
	// SWEPT HERE, on the path every caller takes, so a cache that only
	// ever grows by an installation replaced or a scope changed is bounded
	// by what has been asked for within the hour.
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
	if ok && strings.TrimSpace(held.Token) != "" && held.ExpiresAt.After(now.Add(life)) {
		return held, nil
	}
	if wasRefused && refused.signer == signer && now.Before(refused.retry) {
		return InstallationToken{}, refused.err
	}

	minted, err := TokenFor(ctx, opts, seat)
	if err != nil {
		i.refuse(ctx, key, signer, now, err)
		return InstallationToken{}, err
	}
	i.mu.Lock()
	if i.held == nil {
		i.held = map[issueKey]InstallationToken{}
	}
	i.held[key] = *minted
	delete(i.refused, key)
	i.mu.Unlock()
	return *minted, nil
}

// refuse holds err as GitHub's answer for key, signed by signer, for the wait
// its place in a run of refusals earns. asked is when the caller began.
//
// NOT A CALLER'S OWN END: a mint cut short by the caller's context is no
// answer from GitHub, and holding it would hand the next reader a failure
// nobody met. And a refusal another caller recorded while this one's request
// was in flight is the same run of refusals, not the next in it, so it is left
// as it is rather than doubling the wait for one answer GitHub gave twice.
func (i *Issuer) refuse(ctx context.Context, key issueKey, signer [sha256.Size]byte, asked time.Time, err error) {
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
	if seen && prev.signer == signer {
		n = prev.n + 1
	}
	at := i.now()
	if i.refused == nil {
		i.refused = map[issueKey]refusal{}
	}
	i.refused[key] = refusal{err: err, signer: signer, n: n, at: at, retry: at.Add(i.RetryWait(n))}
}

// RenewAt is when a holder of token should ask again: the moment it has only
// [RenewBefore] left, which is the moment the issuer stops handing it out.
func RenewAt(token InstallationToken) time.Time {
	return token.ExpiresAt.Add(-RenewBefore)
}
