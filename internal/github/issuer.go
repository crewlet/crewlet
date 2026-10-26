package github

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// An agent acts on GitHub as its OWN app, with a token issued at use.
//
// A seat's app ([SeatApp]) is one bot identity, and what it acts with is an
// INSTALLATION TOKEN: minted from the app's private key against its
// installation, scoped down to the seat's tier and repositories, and valid
// for one hour. Nothing else carries the app's identity (a token is the only
// thing GitHub accepts from a git push, a `gh pr create` or the GitHub MCP
// server), so the same token has to reach every place the seat works: its
// tool servers on this node, its coding box, and the tool servers inside
// that box.
//
// # Issued, never sealed
//
// The token is NOT a secret this engine stores. It expires within the hour,
// it can be minted again by any node holding the app's key at any time, and a
// copy in the replicated store would be a live credential written to every
// node and every backup, re-written every few minutes, for nothing a fresh
// mint does not already give. So it is held in this process's memory only,
// by an [Issuer], and handed to exactly the places that read it.
//
// # One name per seat, and a seat reads only its own
//
// Configuration points at it the way it points at every credential, as a
// `${VAR}`: [SeatVar] with [FieldAppToken], beside the sealed key and webhook
// secret the app's creation wrote ([FieldAppKey], [FieldAppWebhookSecret]).
// What answers that reference is the seat's OWN resolution: a seat's values
// are resolved with its token laid in front of the company's sources, and no
// other seat's. One seat naming another's variable therefore names something
// nothing answers, which is what keeps an app's identity with the agent it was
// created for.

// The fields a seat's app keeps under its own name ([SeatVar]).
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
// [setup.SecretNameFor]'s, which puts the vendor first: a handle beginning
// with a digit (`7th-engineer`, which the org model accepts) would
// otherwise make a name no `${VAR}` can reference (see that function).
// `senior-engineer`'s token is GITHUB_APP_TOKEN_SENIOR_ENGINEER.
func SeatVar(handle, field string) string {
	return setup.SecretNameFor(integration.KindGitHub, setup.Requirement{
		Field: field, Seat: handle,
	})
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

	// RunTokenLife is what a coding run's environment is given at launch.
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
// step a person takes at GitHub or in the dashboard, which the seat-app
// reconcile reports on the integration's card ([ReconcileSeatApps]).
var ErrNotInstalled = errors.New("github: this agent's app is not created and installed yet")

// Issuer holds the installation tokens this process has minted, one per
// installation and scope, and mints again when one runs short.
//
// ONE PER PROCESS, shared by everything that reads a seat's token: the
// participant lookup on the inbound path ([SeatLookup]), a seat's tool
// servers, a coding run's environment and the completion poll that keeps a
// long run's token current. Shared so that one installation costs one mint
// an hour however many of those ask, and so a token minted for one is the
// token the others are handed while it is good.
//
// MEMORY ONLY. A token is never logged, never written to the store and never
// put in an error: see the file comment, and [Issuer.Token].
//
// Safe for concurrent use. Two callers that miss at once may both mint; the
// later token replaces the earlier in the cache and both are valid until
// they expire, so the race costs a request rather than a correctness
// property, and a per-key lock would hold every caller of one installation
// behind GitHub's slowest answer.
type Issuer struct {
	// Now is the clock tokens are judged against. Nil takes the wall clock.
	Now func() time.Time

	mu   sync.Mutex
	held map[issueKey]InstallationToken
}

// issueKey is what one token is valid FOR: the app and installation it was
// minted against, and the scope it was narrowed to. Two seats can share an
// installation only by sharing an app, which the engine never creates, but
// the scope is in the key regardless: a token narrowed to one tier or one set
// of repositories must never be handed to a caller asking for another.
type issueKey struct {
	app, installation int64
	tier              Tier
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
		tier: seat.Tier, repos: strings.Join(slices.Compact(repos), ","),
	}
}

func (i *Issuer) now() time.Time {
	if i.Now != nil {
		return i.Now().UTC()
	}
	return time.Now().UTC()
}

// Issuable reports whether seat's app can issue a token at all: created,
// installed, and holding a key. A seat for which this is false is waiting on
// a person, never on GitHub.
func Issuable(seat SeatApp) bool {
	return seat.AppID != 0 && seat.InstallationID != 0 && strings.TrimSpace(seat.Key) != ""
}

// Token is a token for seat's app that stays good for at least life, from
// the cache where one does and minted otherwise.
//
// A MINTED TOKEN IS RETURNED WHATEVER LIFE IT HAS. GitHub decides how long a
// token lives, and a fresh one is the best there is; a caller asking for more
// than an hour is handed the hour.
//
// The error never carries the token, and never carries GitHub's response
// body, which on the token endpoint is the token.
func (i *Issuer) Token(
	ctx context.Context, opts SeatAppOptions, seat SeatApp, life time.Duration,
) (InstallationToken, error) {
	if !Issuable(seat) {
		return InstallationToken{}, ErrNotInstalled
	}
	key := keyOf(seat)
	now := i.now()

	i.mu.Lock()
	held, ok := i.held[key]
	// SWEPT HERE, on the path every caller takes, so a cache that only
	// ever grows by an installation replaced or a scope changed is bounded
	// by what has been asked for within the hour.
	for k, t := range i.held {
		if !t.ExpiresAt.After(now) {
			delete(i.held, k)
		}
	}
	i.mu.Unlock()
	if ok && strings.TrimSpace(held.Token) != "" && held.ExpiresAt.After(now.Add(life)) {
		return held, nil
	}

	minted, err := TokenFor(ctx, opts, seat)
	if err != nil {
		return InstallationToken{}, err
	}
	i.mu.Lock()
	if i.held == nil {
		i.held = map[issueKey]InstallationToken{}
	}
	i.held[key] = *minted
	i.mu.Unlock()
	return *minted, nil
}

// RenewAt is when a holder of token should ask again: the moment it has only
// [RenewBefore] left, which is the moment the issuer stops handing it out.
func RenewAt(token InstallationToken) time.Time {
	return token.ExpiresAt.Add(-RenewBefore)
}
