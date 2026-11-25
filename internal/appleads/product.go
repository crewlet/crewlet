package appleads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/es256"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// Deps are what the product reaches the company and Apple through.
type Deps struct {
	// Company is the running document.
	Company func() *config.Company
	// Resolve reads the value a ${VAR} names, from the secret store.
	Resolve func(string) string
	// HTTP is every call's client; nil is httpx's guarded client.
	HTTP *http.Client
	// Base and TokenURL are Apple's addresses; empty is [BaseURL] and [TokenURL].
	Base, TokenURL string
	// Audit records each step of an act.
	Audit Audit
	// Leases hold one write per ad account at a time, fleet-wide, and the
	// spend guard's apply lease.
	Leases Leaser
	// Signals keeps what Apple's answers say of this node's clock.
	Signals Signals
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
}

// Signals is the node's record of a vendor refusing the secrets it signs
// because its clock is off (growth.Signals).
type Signals interface {
	ClockRefused(product, vendor string, offset time.Duration)
	ClockAgreed(product string)
}

// Leaser holds one lease on a resource at a time, fleet-wide. Hold's error
// wraps [growth.ErrActHeld] where another holder has the lease; any other is
// the lease store's failure or ctx's end, and nothing was taken.
type Leaser interface {
	Hold(ctx context.Context, resource string) (release func(), err error)
}

// Product is Apple Ads for one engine.
type Product struct {
	deps Deps
	// applyBound cuts everything an apply does under its lease
	// (growth.ApplyBound).
	applyBound time.Duration

	mu    sync.Mutex
	stamp string
	conns []*conn
}

// conn is one connection: its API user's client and the ad accounts it
// reaches, with its roles on each.
type conn struct {
	id, org string
	client  *Client

	mu     sync.Mutex
	acls   []adAccount
	aclsAt time.Time
}

// adAccount is one ad account a connection reaches, as `GET /v1/acls`
// answers it: the roles are the catalog's.
type adAccount struct {
	ID    string   `json:"id"`
	Name  string   `json:"name" untrusted:"name"`
	OrgID string   `json:"org_id"`
	Roles []string `json:"roles"`
}

// New is the product over deps.
func New(deps Deps) *Product {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Base == "" {
		deps.Base = BaseURL
	}
	if deps.TokenURL == "" {
		deps.TokenURL = TokenURL
	}
	return &Product{deps: deps, applyBound: growth.ApplyBound}
}

// Catalog is the product's catalog.
func (p *Product) Catalog() growth.Catalog { return Catalog() }

// Level is the seat's level now, "" for a seat the lane is off for.
func (p *Product) Level(handle string) growth.Level {
	b := p.deps.Company().AppleAdsSeat(handle)
	if b == nil {
		return ""
	}
	return growth.LevelOf(b.Level)
}

// seat is a seat's block, or a refusal of a call by a seat the lane is off for.
func (p *Product) seat(handle string) (*config.RoleAppleAds, error) {
	b := p.deps.Company().AppleAdsSeat(handle)
	if b == nil {
		return nil, growth.Refuse(growth.RefusedForbidden, "this agent's Apple Ads access is off. No call was made.")
	}
	return b, nil
}

// clockSkew is how far Apple's clock may be from the node's before a refused
// secret is laid to the node's clock: the secret starts a minute back.
const clockSkew = time.Minute

// clock hears the token endpoint: a secret refused while Apple's clock is far
// from the node's is the node's clock, which the notice says; an accepted one
// closes it.
func (p *Product) clock(refused bool, date time.Time) {
	if p.deps.Signals == nil {
		return
	}
	offset := p.deps.Now().Sub(date)
	switch {
	case refused && (offset > clockSkew || offset < -clockSkew):
		p.deps.Signals.ClockRefused(Kind, "Apple", offset)
	case !refused:
		p.deps.Signals.ClockAgreed(Kind)
	}
}

// stampOf digests the block, so connections are rebuilt when it changes.
func stampOf(block *config.AppleAds) string {
	data, _ := json.Marshal(block)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// connsNow are the connections as the running document names them, each key
// read from the secret store and parsed once per document.
func (p *Product) connsNow() ([]*conn, error) {
	block := p.deps.Company().Integrations.AppleAds
	if !block.GrowthEnabled() {
		return nil, growth.Refuse(growth.RefusedUnavailable, "Apple Ads is not connected. No call was made.")
	}
	stamp := stampOf(block)
	p.mu.Lock()
	defer p.mu.Unlock()
	if stamp == p.stamp {
		return p.conns, nil
	}
	var conns []*conn
	for _, id := range block.ConnectionIDs() {
		c := block.Connections[id]
		key, err := es256.ParseKey(p.deps.Resolve(c.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("appleads: the %s connection's private key does not read as a P-256 key: %w", id, err)
		}
		creds := Credentials{ClientID: strings.TrimSpace(p.deps.Resolve(c.ClientID)),
			TeamID: strings.TrimSpace(p.deps.Resolve(c.TeamID)), KeyID: strings.TrimSpace(p.deps.Resolve(c.KeyID)), Key: key}
		client := NewClient(p.deps.Base, p.deps.TokenURL, creds, p.deps.HTTP)
		client.now, client.answered = p.deps.Now, p.clock
		conns = append(conns, &conn{id: id, org: strings.TrimSpace(p.deps.Resolve(c.OrgID)), client: client})
	}
	p.stamp, p.conns = stamp, conns
	return conns, nil
}

// aclTTL is how long a connection's ad accounts and roles are reused. Apple
// enforces a role on every call itself; the engine reads it to refuse early,
// naming the roles that can, so a role changed in Apple Ads applies here
// within five minutes.
const aclTTL = 5 * time.Minute

// aclsOf are the ad accounts c reaches, with its roles on each.
func (p *Product) aclsOf(ctx context.Context, c *conn) ([]adAccount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.acls != nil && p.deps.Now().Sub(c.aclsAt) < aclTTL {
		return c.acls, nil
	}
	resp, err := c.client.Do(ctx, Request{Method: http.MethodGet, Path: "/v1/acls", Read: true, Reserved: true})
	if err != nil {
		return nil, err
	}
	accounts, err := readACLs(resp.Body)
	if err != nil {
		return nil, err
	}
	c.acls, c.aclsAt = accounts, p.deps.Now()
	return accounts, nil
}

// readACLs reads `GET /v1/acls`; a role this build does not know grants nothing.
func readACLs(body []byte) ([]adAccount, error) {
	var doc struct {
		Result struct {
			ACLs []struct {
				AdAccount struct {
					ID    json.Number `json:"id"`
					Name  string      `json:"name"`
					OrgID json.Number `json:"orgId"`
				} `json:"adAccount"`
				Roles []string `json:"roles"`
			} `json:"acls"`
		} `json:"result"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("appleads: read the API user's ad accounts: %w", err)
	}
	accounts := []adAccount{}
	for _, a := range doc.Result.ACLs {
		acct := adAccount{ID: a.AdAccount.ID.String(), Name: a.AdAccount.Name, OrgID: a.AdAccount.OrgID.String(),
			Roles: []string{}}
		for _, name := range a.Roles {
			if id, ok := roleOf(strings.TrimSpace(name)); ok && !slices.Contains(acct.Roles, id) {
				acct.Roles = append(acct.Roles, id)
			}
		}
		accounts = append(accounts, acct)
	}
	return accounts, nil
}

// reach is the connection reaching account, and its roles there.
func (p *Product) reach(ctx context.Context, account string) (*conn, adAccount, error) {
	conns, err := p.connsNow()
	if err != nil {
		return nil, adAccount{}, err
	}
	for _, c := range conns {
		accounts, err := p.aclsOf(ctx, c)
		if err != nil {
			return nil, adAccount{}, err
		}
		if i := slices.IndexFunc(accounts, func(a adAccount) bool { return a.ID == account }); i >= 0 {
			return c, accounts[i], nil
		}
	}
	return nil, adAccount{}, growth.Refuse(growth.RefusedNotFound, fmt.Sprintf("ad account %s is reached by none of "+
		"the connected API users. A person gives one access in Apple Ads. No call was made.", account))
}

// connNamed is the connection an organization call names, or the only one.
func (p *Product) connNamed(name string) (*conn, error) {
	conns, err := p.connsNow()
	if err != nil {
		return nil, err
	}
	if name == "" && len(conns) == 1 {
		return conns[0], nil
	}
	ids := make([]string, 0, len(conns))
	for _, c := range conns {
		if c.id == name {
			return c, nil
		}
		ids = append(ids, c.id)
	}
	return nil, growth.Refuse(growth.RefusedInvalid, fmt.Sprintf("name the connection in `connection`: %s. No call "+
		"was made.", words.Or(ids)))
}

// accountInfo is an ad account as `GET /v1/ad-accounts/{id}` answers it.
type accountInfo struct {
	ID           string
	Name         string
	TimeZone     string
	Currency     string
	PaymentModel string
	Status       string
	Reasons      []string
	Features     []string
}

// serving reports whether Apple serves the account: ACTIVE.
func (a accountInfo) serving() bool { return a.Status == "ACTIVE" }

// state is the account's state as the growth pages show it.
func (a accountInfo) state() growth.AccountState {
	switch a.Status {
	case "ACTIVE":
		return growth.AccountServing
	case "INACTIVE":
		return growth.AccountSuspended
	}
	return growth.AccountUnknown
}

// accountOf reads account live through c, held to the account it names.
func (p *Product) accountOf(ctx context.Context, c *conn, account string) (accountInfo, error) {
	resp, err := c.client.Do(ctx, Request{Method: http.MethodGet, Path: "/v1/ad-accounts/" + account,
		Account: account, Read: true, Reserved: true})
	if err != nil {
		return accountInfo{}, err
	}
	var doc struct {
		Result struct {
			ID                  json.Number `json:"id"`
			Name                string      `json:"name"`
			Timezone            string      `json:"timezone"`
			Currency            string      `json:"currency"`
			PaymentModel        string      `json:"paymentModel"`
			SystemStatus        string      `json:"systemStatus"`
			SystemStatusReasons []string    `json:"systemStatusReasons"`
			ProductFeatures     []string    `json:"productFeatures"`
		} `json:"result"`
	}
	dec := json.NewDecoder(bytes.NewReader(resp.Body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return accountInfo{}, fmt.Errorf("appleads: read ad account %s: %w", account, err)
	}
	r := doc.Result
	if r.ID.String() != account {
		return accountInfo{}, fmt.Errorf("appleads: Apple answered ad account %q for %s", r.ID.String(), account)
	}
	return accountInfo{ID: account, Name: r.Name, TimeZone: r.Timezone, Currency: r.Currency,
		PaymentModel: r.PaymentModel, Status: r.SystemStatus, Reasons: r.SystemStatusReasons,
		Features: r.ProductFeatures}, nil
}
