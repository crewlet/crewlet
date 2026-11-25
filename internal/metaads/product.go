package metaads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// Deps are what the product reaches the company and Meta through.
type Deps struct {
	// Company is the running document.
	Company func() *config.Company
	// Resolve reads the value a ${VAR} names, from the secret store.
	Resolve func(string) string
	// HTTP is every call's client; nil is httpx's guarded client.
	HTTP *http.Client
	// Base is the Graph API's address; empty is [BaseURL].
	Base string
	// Audit records each step of an act and counts the changes made.
	Audit Audit
	// Leases hold one write per ad account at a time, fleet-wide, the spend
	// guard's apply lease and the renewal's.
	Leases Leaser
	// Renewals keep the tokens the engine renewed, fleet-wide.
	Renewals Renewals
	// Signals keeps Meta calling the pinned version deprecated.
	Signals Signals
	// Identity is who signs a seat's calls ([seataccess.ResolveSeat]); nil
	// signs every call as the company's system user.
	Identity func(ctx context.Context, handle string) (seataccess.Signer, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Audit records the steps of a growth act on the access log (internal/engine's
// GrowthAudit); a held call's ask is recorded by the engine once it is held,
// from [growth.Hold.Ask]. first is false where the act already holds the step.
type Audit interface {
	Made(ctx context.Context, r growth.Record) (first bool, err error)
	Verdict(ctx context.Context, step growth.Step, r growth.Record) (first bool, err error)
	Applied(ctx context.Context, r growth.Record) (first bool, err error)
	Void(ctx context.Context, r growth.Record) (first bool, err error)
	Failed(ctx context.Context, r growth.Record) (first bool, err error)
	// Changed counts the changes to target that took effect since since.
	Changed(ctx context.Context, product, account, target, handle string, since time.Time) (int, error)
}

// Signals is the node's record of a vendor calling the version a product
// calls deprecated (growth.Signals).
type Signals interface {
	Deprecated(product, version string, retires time.Time)
}

// Leaser holds one lease on a resource at a time, fleet-wide. Hold's error
// wraps [growth.ErrActHeld] where another holder has the lease; any other is
// the lease store's failure or ctx's end, and nothing was taken.
type Leaser interface {
	Hold(ctx context.Context, resource string) (release func(), err error)
}

// Renewals keep each token slot's renewal, sealed, fleet-wide: a value read
// with the version it was read at, written over only at that version (0
// creates it), so two nodes renewing at once keep one renewal.
type Renewals interface {
	Held(ctx context.Context, slot string) (value string, version uint64, found bool, err error)
	Put(ctx context.Context, slot, value string, version uint64) (bool, error)
	Delete(ctx context.Context, slot string) error
}

// Product is Meta Ads for one engine.
type Product struct {
	deps Deps
	// applyBound cuts everything an apply does under its lease
	// (growth.ApplyBound).
	applyBound time.Duration
	owners     *owners
	// forgotten is set once a pass turned off has deleted every renewal.
	forgotten atomic.Bool

	mu    sync.Mutex
	stamp string
	conn  *conn
}

// New is the product over deps.
func New(deps Deps) *Product {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Base == "" {
		deps.Base = BaseURL
	}
	return &Product{deps: deps, applyBound: growth.ApplyBound, owners: newOwners(maxOwners)}
}

// Catalog is the product's catalog.
func (p *Product) Catalog() growth.Catalog { return Catalog() }

// Level is the seat's level now, "" for a seat the lane is off for.
func (p *Product) Level(handle string) growth.Level {
	b := p.deps.Company().MetaAdsSeat(handle)
	if b == nil {
		return ""
	}
	return growth.LevelOf(b.Level)
}

// seat is a seat's block, or a refusal of a call by a seat the lane is off for.
func (p *Product) seat(handle string) (*config.RoleMetaAds, error) {
	b := p.deps.Company().MetaAdsSeat(handle)
	if b == nil {
		return nil, growth.Refuse(growth.RefusedForbidden, "this agent's Meta Ads access is off. No call was made.")
	}
	return b, nil
}

// ownOf is handle's own system user's token where it keeps one that works,
// nil for the company's, and a refusal where its own no longer works, never
// the company's in its place.
func (p *Product) ownOf(ctx context.Context, handle string, seat *config.RoleMetaAds) (*token, error) {
	if p.deps.Identity == nil {
		return nil, nil
	}
	signer, err := p.deps.Identity(ctx, handle)
	if refused, ok := errors.AsType[*seataccess.IdentityRefusal](err); ok {
		return nil, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("this agent's own Meta system user is %s: %s. "+
			"A person sets up its identity again on the integration's Agents tab, or sets it back to the company's. "+
			"No call was made.", strings.ToLower(refused.Status.Label()), strings.TrimSuffix(refused.Said, ".")))
	}
	if err != nil {
		return nil, fmt.Errorf("metaads: read %s's identity: %w", handle, err)
	}
	if !signer.Own {
		return nil, nil
	}
	var value string
	if cred := signer.Record.Credential; cred != nil {
		value = strings.TrimSpace(p.deps.Resolve("${" + cred.Variable + "}"))
	}
	if value == "" {
		return nil, growth.Refuse(growth.RefusedForbidden, "this agent's own Meta system user's token is not in the "+
			"secret store. A person sets up its identity again on the integration's Agents tab. No call was made.")
	}
	c, err := p.connNow()
	if err != nil {
		return nil, err
	}
	level, from := growth.LevelOf(seat.Level), digest(value)
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.own[handle]; t != nil && t.from == from && t.level == level {
		return t, nil
	}
	t := &token{slot: slotOwn, level: level, pasted: value, from: from}
	c.own[handle] = t
	return t, nil
}

// connection is the record's name for the one system user a company connects.
const connection = "main"

// conn is the company's connection as the running document names it: the
// app's client and each token slot.
type conn struct {
	appID  string
	client *Client
	tokens map[string]*token

	mu         sync.Mutex
	accounts   []adAccount
	accountsAt time.Time
	uses       map[string]uses
	// own are the seats' own tokens in use, by handle, each kept while the
	// sealed value and the seat's level stay what they were.
	own map[string]*token
}

// stampOf digests the block and the values its references resolve to, so the
// connection is rebuilt when either changes.
func (p *Product) stampOf(block *config.MetaAds) string {
	data, _ := json.Marshal(block)
	h := sha256.New()
	h.Write(data)
	for _, ref := range []string{block.AppID, block.AppSecret, block.ReadToken, block.OperateToken, block.AdminToken} {
		v := sha256.Sum256([]byte(p.deps.Resolve(ref)))
		h.Write(v[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// connNow is the connection as the running document names it, its tokens
// read from the secret store and the renewals.
func (p *Product) connNow() (*conn, error) {
	block := p.deps.Company().Integrations.MetaAds
	if !block.GrowthEnabled() {
		return nil, growth.Refuse(growth.RefusedUnavailable, "Meta Ads is not connected. No call was made.")
	}
	stamp := p.stampOf(block)
	p.mu.Lock()
	defer p.mu.Unlock()
	if stamp == p.stamp && p.conn != nil {
		return p.conn, nil
	}
	appID := strings.TrimSpace(p.deps.Resolve(block.AppID))
	secret := strings.TrimSpace(p.deps.Resolve(block.AppSecret))
	if appID == "" || secret == "" {
		return nil, growth.Refuse(growth.RefusedUnavailable, "Meta Ads' app ID or app secret is not in the secret "+
			"store, so no call can be proven. A person pastes them on the integration's page. No call was made.")
	}
	client := NewClient(p.deps.Base, secret, p.deps.HTTP)
	client.now = p.deps.Now
	client.deprecated = func() {
		if p.deps.Signals != nil {
			p.deps.Signals.Deprecated(Kind, Version, time.Time{})
		}
	}
	c := &conn{appID: appID, client: client, tokens: map[string]*token{}, uses: map[string]uses{},
		own: map[string]*token{}}
	for _, s := range slots {
		pasted := strings.TrimSpace(p.deps.Resolve(s.ref(block)))
		if pasted == "" {
			continue
		}
		c.tokens[s.name] = &token{slot: s.name, pasted: pasted, from: digest(pasted)}
	}
	if c.tokens[slotRead] == nil {
		return nil, growth.Refuse(growth.RefusedUnavailable, "Meta Ads' read token is not in the secret store. A "+
			"person pastes it on the integration's page. No call was made.")
	}
	p.stamp, p.conn = stamp, c
	return c, nil
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// adAccount is one ad account the system user reaches, as Meta answers it.
type adAccount struct {
	ID            string `json:"id"`
	Name          string `json:"name" untrusted:"name"`
	Currency      string `json:"currency"`
	TimeZone      string `json:"timezone_name"`
	Status        int    `json:"account_status"`
	DisableReason int    `json:"disable_reason"`
	AmountSpent   string `json:"amount_spent"`
	SpendCap      string `json:"spend_cap"`
	MinDaily      int64  `json:"min_daily_budget"`
	Business      struct {
		ID string `json:"id"`
	} `json:"business"`
}

// accountFields are the fields an ad account is read with.
const accountFields = "id,name,currency,timezone_name,account_status,disable_reason,amount_spent,spend_cap," +
	"min_daily_budget,business"

// reachTTL is how long the system user's ad account list is reused: ad
// accounts are assigned rarely, and ten minutes keeps a new one reachable
// within a working session.
const reachTTL = 10 * time.Minute

// maxAccountPages bounds the ad account list: 40 pages of 100 is 4,000.
const maxAccountPages = 40

// accountsOf are the ad accounts c's system user is assigned, read with its
// read token.
func (p *Product) accountsOf(ctx context.Context, c *conn) ([]adAccount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accounts != nil && p.deps.Now().Sub(c.accountsAt) < reachTTL {
		return c.accounts, nil
	}
	tok, err := p.tokenFor(ctx, c, nil, growth.ClassRead, []string{"ads_read"})
	if err != nil {
		return nil, err
	}
	var out []adAccount
	q := map[string][]string{"fields": {accountFields}, "limit": {"100"}}
	err = pages(ctx, c.client, tok.valueNow(), "/me/adaccounts", q, "", true, maxAccountPages, func(raw json.RawMessage) error {
		var a adAccount
		if uerr := json.Unmarshal(raw, &a); uerr != nil {
			return fmt.Errorf("metaads: read an ad account: %w", uerr)
		}
		out = append(out, a)
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.accounts, c.accountsAt = out, p.deps.Now()
	return out, nil
}

// accountOf is ad account id as the system user reaches it, or a refusal
// naming that it does not.
func (p *Product) accountOf(ctx context.Context, c *conn, id string) (adAccount, error) {
	accounts, err := p.accountsOf(ctx, c)
	if err != nil {
		return adAccount{}, err
	}
	i := slices.IndexFunc(accounts, func(a adAccount) bool { return a.ID == id })
	if i < 0 {
		return adAccount{}, growth.Refuse(growth.RefusedNotFound, fmt.Sprintf("ad account %s is not assigned to the "+
			"connected system user. A person assigns it in Business Settings. No call was made.", id))
	}
	return accounts[i], nil
}

// statusWords are Meta's account_status values in words.
var statusWords = map[int]string{1: "active", 2: "disabled", 3: "unsettled: pay the balance", 7: "in risk review",
	8: "pending settlement", 9: "in its grace period", 100: "pending closure", 101: "closed", 201: "active (any)",
	202: "closed (any)"}

// state is the account's serving state as the growth sources show it.
func (a adAccount) state() growth.AccountState {
	switch a.Status {
	case 1:
		return growth.AccountServing
	case 2, 3, 7, 8:
		return growth.AccountSuspended
	case 9:
		return growth.AccountServing
	case 100, 101, 202:
		return growth.AccountClosed
	}
	return growth.AccountUnknown
}

// serving reports whether Meta serves the account's ads.
func (a adAccount) serving() bool { return a.state().Serves() }

// statusText is the account's status in words.
func (a adAccount) statusText() string {
	if w, ok := statusWords[a.Status]; ok {
		return w
	}
	return fmt.Sprintf("status %d", a.Status)
}

// seatAccounts are the seat's ad accounts as Meta writes them.
func seatAccounts(seat *config.RoleMetaAds) []string { return seat.AdAccountIDs() }

// accountNamed is the ad account a call names in `account`, or the seat's only
// one, held to the seat's.
func accountNamed(seat *config.RoleMetaAds, args map[string]any) (string, error) {
	accounts := seatAccounts(seat)
	raw, _ := args["account"].(string)
	account := config.MetaAdAccountID(raw)
	switch {
	case raw == "" && len(accounts) == 1:
		return accounts[0], nil
	case raw == "":
		return "", invalid("name the ad account in `account`: %s.", words.Or(accounts))
	case account == "":
		return "", invalid("%q is not an ad account ID: act_ and its digits.", raw)
	case !slices.Contains(accounts, account):
		return "", outside(account, accounts)
	}
	return account, nil
}

// outside is the refusal of an ad account the seat may not touch.
func outside(account string, accounts []string) error {
	return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("ad account %s is not one of this agent's (%s). A "+
		"person adds it on the integration's Agents tab. No call was made.", account, words.And(accounts)))
}

func invalid(format string, args ...any) error {
	return growth.Refuse(growth.RefusedInvalid, fmt.Sprintf(format, args...)+" No call was made.")
}
