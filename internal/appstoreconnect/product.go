package appstoreconnect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	// Base is Apple's address; empty is [BaseURL].
	Base string
	// Audit records each step of an act.
	Audit Audit
	// Leases hold one public act per app at a time, fleet-wide.
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

// Signals is the node's record of a vendor refusing the tokens it signs
// because its clock is off (growth.Signals).
type Signals interface {
	ClockRefused(product, vendor string, offset time.Duration)
	ClockAgreed(product string)
}

// Leaser holds one lease on a resource at a time, fleet-wide.
type Leaser interface {
	Hold(ctx context.Context, resource string) (release func(), err error)
}

// Product is App Store Connect for one engine.
type Product struct {
	deps   Deps
	owners *owners
	// applyBound cuts an act under its lease: growth.ApplyBound.
	applyBound time.Duration

	mu    sync.Mutex
	stamp string
	teams []*team
}

// team is one connection: its client, its connected key slots, its apps.
type team struct {
	id, vendor string
	client     *Client
	slots      []string

	// mu guards the held list alone, never across a read of Apple, so the
	// card and the Agents tab answer at once while a read is in flight.
	mu     sync.Mutex
	apps   []appInfo
	appsAt time.Time
	// unread is why the last read of apps failed, "" where it did not.
	unread string
	// listed counts the reads that listed the apps.
	listed int
	// reading admits one read of the team's apps at a time: a caller that
	// waits for it gives up when its context ends.
	reading chan struct{}
}

// appInfo is one app a team reaches.
type appInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name" untrusted:"name"`
	BundleID string `json:"bundle_id"`
	SKU      string `json:"sku"`
}

// New is the product over deps.
func New(deps Deps) *Product {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Base == "" {
		deps.Base = BaseURL
	}
	return &Product{deps: deps, owners: newOwners(maxOwners), applyBound: growth.ApplyBound}
}

// Catalog is the product's catalog.
func (p *Product) Catalog() growth.Catalog { return Catalog() }

// Level is the seat's level now, "" for a seat the lane is off for.
func (p *Product) Level(handle string) growth.Level {
	b := p.deps.Company().AppStoreConnectSeat(handle)
	if b == nil {
		return ""
	}
	return growth.LevelOf(b.Level)
}

// seat is a seat's block, or a refusal of a call by a seat the lane is off for.
func (p *Product) seat(handle string) (*config.RoleAppStoreConnect, error) {
	b := p.deps.Company().AppStoreConnectSeat(handle)
	if b == nil {
		return nil, growth.Refuse(growth.RefusedForbidden, "this agent's App Store Connect access is off. No call was made.")
	}
	return b, nil
}

// clockSkew is how far Apple's clock may be from the node's before a refused
// token is laid to the node's clock: the token starts a minute back.
const clockSkew = time.Minute

// clock hears Apple's answer: a token refused while Apple's clock is far from
// the node's is the node's clock, which the notice says; an accepted one
// closes it.
func (p *Product) clock(status int, date time.Time) {
	if p.deps.Signals == nil {
		return
	}
	offset := p.deps.Now().Sub(date)
	switch {
	case status == http.StatusUnauthorized && (offset > clockSkew || offset < -clockSkew):
		p.deps.Signals.ClockRefused(Kind, "Apple", offset)
	case status < 400:
		p.deps.Signals.ClockAgreed(Kind)
	}
}

// stampOf digests the block's connections, so teams are rebuilt when one
// changes; naming apps or approvers keeps every team and the apps it read.
func stampOf(block *config.AppStoreConnect) string {
	data, _ := json.Marshal(block.Connections)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// teamsNow are the connections as the running document names them, each key
// read from the secret store and parsed once per document.
func (p *Product) teamsNow() ([]*team, error) {
	block := p.deps.Company().Integrations.AppStoreConnect
	if !block.GrowthEnabled() {
		return nil, growth.Refuse(growth.RefusedUnavailable, "App Store Connect is not connected. No call was made.")
	}
	stamp := stampOf(block)
	p.mu.Lock()
	defer p.mu.Unlock()
	if stamp == p.stamp {
		return p.teams, nil
	}
	var teams []*team
	for _, id := range block.ConnectionIDs() {
		c := block.Connections[id]
		keys := map[string]Key{}
		var slots []string
		for _, s := range c.Keys.Slots() {
			key, err := es256.ParseKey(p.deps.Resolve(s.Key.PrivateKey))
			if err != nil {
				return nil, fmt.Errorf("appstoreconnect: the %s connection's %s key does not read as a .p8 key: %w",
					id, s.Name, err)
			}
			keys[s.Name] = Key{ID: strings.TrimSpace(p.deps.Resolve(s.Key.KeyID)), Key: key}
			slots = append(slots, s.Name)
		}
		client := NewClient(p.deps.Base, strings.TrimSpace(p.deps.Resolve(c.IssuerID)), keys, p.deps.HTTP)
		client.now, client.answered = p.deps.Now, p.clock
		teams = append(teams, &team{id: id, vendor: strings.TrimSpace(p.deps.Resolve(c.VendorNumber)),
			client: client, slots: slots, reading: make(chan struct{}, 1)})
	}
	p.stamp, p.teams = stamp, teams
	return teams, nil
}

// reachTTL is how long a team's app list is reused: apps join a team rarely,
// and ten minutes keeps a new one reachable within a working session.
const reachTTL = 10 * time.Minute

// appsOf are the apps t reaches, read with its least-privileged key.
func (p *Product) appsOf(ctx context.Context, t *team) ([]appInfo, error) {
	return p.readApps(ctx, t, false)
}

// readApps is [Product.appsOf], read again where fresh; a failed read is
// recorded on t for [Product.HeldReach] and keeps the list read before. One
// read of a team runs at a time, and one that ends while a caller waited for
// it answers that caller.
func (p *Product) readApps(ctx context.Context, t *team, fresh bool) ([]appInfo, error) {
	t.mu.Lock()
	seen := t.listed
	if !fresh && t.current(p.deps.Now()) {
		defer t.mu.Unlock()
		return t.apps, nil
	}
	t.mu.Unlock()
	select {
	case t.reading <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-t.reading }()
	t.mu.Lock()
	if t.listed != seen || (!fresh && t.current(p.deps.Now())) {
		defer t.mu.Unlock()
		return t.apps, nil
	}
	t.mu.Unlock()
	apps, err := p.listApps(ctx, t)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		// CREWLET'S OWN DEADLINE IS NOT APPLE'S ANSWER: a caller that gave up
		// leaves the team's last word as it was.
		if ctx.Err() == nil {
			t.unread = unreadWords(err)
		}
		return nil, err
	}
	t.apps, t.appsAt, t.unread = apps, p.deps.Now(), ""
	t.listed++
	return apps, nil
}

// current reports t's list read within reachTTL of now; t.mu is held.
func (t *team) current(now time.Time) bool {
	return !t.appsAt.IsZero() && now.Sub(t.appsAt) < reachTTL
}

// listApps reads every app t reaches from Apple, through the least-privileged
// key that may list them, never the Admin key.
func (p *Product) listApps(ctx context.Context, t *team) ([]appInfo, error) {
	op, _ := Catalog().Operation("apps_getCollection")
	slot, ok := keyFor(op, t.slots, false)
	if !ok {
		return nil, fmt.Errorf("%w: listing apps", ErrNoKey)
	}
	apps := []appInfo{}
	cursor := ""
	for range maxAppPages {
		q := url.Values{"limit": {"200"}, "fields[apps]": {"name,bundleId,sku"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		resp, err := t.client.Do(ctx, slot, Request{Method: "GET", Path: "/v1/apps", Query: q, Read: true, Reserved: true})
		if err != nil {
			return nil, err
		}
		var doc struct {
			Data []struct {
				ID         string `json:"id"`
				Attributes struct {
					Name, BundleID, SKU string
				} `json:"attributes"`
			} `json:"data"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := json.Unmarshal(resp.Body, &doc); err != nil {
			return nil, fmt.Errorf("appstoreconnect: read the team's apps: %w", err)
		}
		for _, d := range doc.Data {
			apps = append(apps, appInfo{ID: d.ID, Name: d.Attributes.Name, BundleID: d.Attributes.BundleID, SKU: d.Attributes.SKU})
		}
		if cursor = cursorOf(doc.Links.Next); cursor == "" {
			break
		}
	}
	return apps, nil
}

// maxAppPages bounds a team's app list: 40 pages of 200 is 8,000 apps.
const maxAppPages = 40

// cursorOf is the cursor of a next link, "" for none.
func cursorOf(next string) string {
	if next == "" {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil {
		return ""
	}
	return u.Query().Get("cursor")
}

// teamOf is the team reaching app, among the seat's allowed apps.
func (p *Product) teamOf(ctx context.Context, app string) (*team, error) {
	teams, err := p.teamsNow()
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		apps, err := p.appsOf(ctx, t)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(apps, func(a appInfo) bool { return a.ID == app }) {
			return t, nil
		}
	}
	return nil, growth.Refuse(growth.RefusedNotFound, fmt.Sprintf("app %s is in none of the connected teams. No call was made.", app))
}

// teamNamed is the connection a team-wide call names, or the only one.
func (p *Product) teamNamed(name string) (*team, error) {
	teams, err := p.teamsNow()
	if err != nil {
		return nil, err
	}
	if name == "" && len(teams) == 1 {
		return teams[0], nil
	}
	for _, t := range teams {
		if t.id == name {
			return t, nil
		}
	}
	ids := make([]string, 0, len(teams))
	for _, t := range teams {
		ids = append(ids, t.id)
	}
	return nil, growth.Refuse(growth.RefusedInvalid, fmt.Sprintf("name the team's connection in `connection`: %s. No call was made.",
		words.Or(ids)))
}
