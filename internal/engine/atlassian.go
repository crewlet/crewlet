package engine

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/notify"
)

// Seat identity for both Atlassian products.
//
// # It is the whole of the integration, and it is DERIVED
//
// A Jira webhook and a Confluence page event both name people by account id,
// and nothing in the org model says which account a seat holds. Without that
// mapping every event names a stranger, the routing gate drops every target,
// and the integration is silently inert — the deliveries arrive, the parser
// runs, and nobody is woken.
//
// So the engine asks: it calls each product's own identity endpoint with the
// seat's OWN credential and registers whatever account answers. A declared
// account id beside the token would be cheaper and is the wrong shape — a
// declaration that disagrees with the credential is a misroute nothing can
// detect.
//
// # Both products, and that is a fix rather than a generalisation
//
// This resolver used to serve the tracker alone. `confluence.Backend` was
// registered nowhere in the engine, so the wiki's party namespace was
// permanently EMPTY for agent seats: a page mentioning an agent resolved to
// nobody, the page-subscription ledger was never written, an agent was never
// suppressed as the actor of its own edit, and every page event fell through
// to the space lead. The parser was correct the whole time; it was asking a
// registry nothing had ever written to.
//
// # Resolved per PRODUCT, even though one account serves both
//
// On Cloud both endpoints answer the same account id, so the second call
// confirms the first. It is made anyway, because on Data Center they answer
// DIFFERENT things — Jira's `name` against Confluence's `userKey` — and
// registering one product's answer under the other's namespace is exactly the
// misroute this file exists to prevent. The cost is one extra request per
// distinct credential, once, at boot.

// atlassianProduct is which product an identity was resolved against.
type atlassianProduct = atlassian.Product

// identityKey is one credential's identity in one product.
//
// COMPARABLE, and it must be: this is a map key, and it is what makes an
// apply free. Identity is a function of the credential, credentials change
// rarely, and a config revision that touched something else must not spend a
// request per seat to re-learn what it already knows. A rotated token is a
// cache miss and costs exactly one request, which is correct — it may well be
// a different account.
//
// The EMAIL is part of the key, not just the token: Cloud authenticates
// base64(email:token), so the same token under a different address is a
// different credential and may resolve to a different account.
//
// The SITE is not in the key, and is handled by emptying the product's cache
// when the address changes — see [atlassianIdentities.resolve]. Keying on it
// would answer correctly and never forget: every instance a company has ever
// pointed at would keep its entries for the life of the process.
type identityKey struct {
	cred    atlassian.Credential
	product atlassianProduct
}

// atlassianIdentities remembers which account each seat credential
// authenticates as, per product.
type atlassianIdentities struct {
	mu     sync.Mutex
	byCred map[identityKey]string
	// sites is where each product was last resolved, and its keys are the
	// products that HAVE been resolved. One map rather than a set beside
	// it, because the two would be updated in different places and the
	// pair that disagrees registers a product against an address nobody
	// checked.
	sites map[atlassianProduct]productSite
}

// productSite is where one product is reached, as the engine resolved it.
type productSite struct {
	base   string
	deploy jira.Deployment
}

// resolve fills in the accounts behind any credentials not already known, for
// one product.
//
// CONCURRENTLY, bounded by the number of distinct credentials. Sequentially
// this is one round trip per seat on the boot path, which on a company of
// thirty seats against a slow instance is thirty timeouts end to end.
//
// A credential whose lookup FAILS is left unresolved rather than failing the
// boot: the instance may be briefly down, and the next apply retries. What
// that costs is those seats' inbound routing until then, which is the honest
// consequence and is logged.
func (a *atlassianIdentities) resolve(ctx context.Context, product atlassianProduct, where productSite, creds []seatCredential) {
	a.mu.Lock()
	if a.byCred == nil {
		a.byCred = map[identityKey]string{}
		a.sites = map[atlassianProduct]productSite{}
	}
	// A CHANGED ADDRESS EMPTIES THIS PRODUCT'S CACHE. An account id is a
	// fact about one instance: the same credential against a different site
	// is a different account, and on Data Center it is a different SHAPE of
	// identifier entirely. An apply that repointed integrations.jira at a
	// new instance would otherwise re-register every seat under the old
	// one's ids, and every webhook from the new instance would name a
	// stranger — with nothing failing and nothing logged.
	if was, known := a.sites[product]; known && was != where {
		maps.DeleteFunc(a.byCred, func(key identityKey, _ string) bool {
			return key.product == product
		})
	}
	a.sites[product] = where
	var missing []seatCredential
	for _, cred := range creds {
		if _, known := a.byCred[identityKey{cred.cred, product}]; !known {
			missing = append(missing, cred)
		}
	}
	a.mu.Unlock()
	if len(missing) == 0 {
		return
	}

	var wg sync.WaitGroup
	found := make([]string, len(missing))
	for i, cred := range missing {
		wg.Add(1)
		go func() {
			defer wg.Done()
			account, err := identityOf(ctx, product, where, cred.cred)
			if err != nil {
				// THE SEATS ARE NAMED. Without them an operator grepping
				// the log for the handle that stopped receiving events
				// finds nothing — which is the exact diagnosis the line
				// exists to provide. One credential can serve several
				// seats, so it is a list.
				log.WarnContext(ctx, "atlassian_seat_identity_unresolved",
					"product", string(product),
					"seats", strings.Join(cred.handles, ","),
					"error", err.Error(),
					"detail", "these seats receive no "+string(product)+
						" events until the next apply re-resolves them")
				return
			}
			found[i] = account
		}()
	}
	wg.Wait()

	a.mu.Lock()
	defer a.mu.Unlock()
	for i, account := range found {
		if account != "" {
			a.byCred[identityKey{missing[i].cred, product}] = account
		}
	}
}

// identityOf asks one product who a credential is.
//
// Each product's own client, deliberately: the two identity endpoints differ
// in path, in the fields they populate and in which REST version serves them,
// and a single call that guessed would come back empty against one of them —
// silently, because "nobody" is a legitimate answer.
func identityOf(ctx context.Context, product atlassianProduct, where productSite, cred atlassian.Credential) (string, error) {
	if product == atlassian.ProductConfluence {
		client, err := confluence.NewClient(confluence.ClientOptions{
			URL: where.base, Email: cred.Email, Token: cred.Token,
		})
		if err != nil {
			return "", err
		}
		return client.Me(ctx)
	}
	client, err := jira.NewClient(jira.ClientOptions{
		URL: where.base, Email: cred.Email, Token: cred.Token,
		Deployment: where.deploy,
	})
	if err != nil {
		return "", err
	}
	return client.Me(ctx)
}

// register binds each resolved seat to its account, in every product this
// company configures.
//
// NO I/O. It takes the registry rather than reading the live one because an
// apply builds a NEW registry from the new company, and a config-derived
// binding has to be rebuilt into it at that moment.
//
// It returns the count PER PRODUCT, because "some seats resolved" is not the
// answer an operator needs: a company whose Jira identities all resolved and
// whose Confluence identities all failed has one integration working and one
// inert, and a single total reports that as healthy.
func (a *atlassianIdentities) register(reg *notify.Registry, c *Company, env *config.Resolver) map[atlassianProduct]int {
	a.mu.Lock()
	known := maps.Clone(a.byCred)
	products := slices.Collect(maps.Keys(a.sites))
	a.mu.Unlock()

	registered := map[atlassianProduct]int{}
	for _, product := range atlassian.Products {
		if !slices.Contains(products, product) {
			continue
		}
		registered[product] = 0
		for seat := range c.Org.AllRoles() {
			cred := atlassian.CredentialOf(seat, env.Value)
			if !cred.Held() {
				continue
			}
			account := known[identityKey{cred, product}]
			if account == "" {
				continue
			}
			if err := reg.Register(backendOf(product), account, seat.Handle()); err != nil {
				// Two seats sharing one account, or a seat that is not in
				// this org. Both are faults an operator has to fix, and
				// both are silent otherwise: that account's events go to
				// whichever seat won.
				log.Warn("atlassian_seat_identity_refused", "seat", seat.Handle(),
					"product", string(product), "account", account,
					"error", err.Error())
				continue
			}
			registered[product]++
		}
	}
	return registered
}

// retain forgets the products a company no longer configures.
//
// The cache survives an apply on purpose — identity is a function of the
// credential, and a revision that changed something else must not spend a
// request per seat to re-learn what it already knows. It must NOT survive the
// product being removed: an operator who dropped integrations.confluence would
// go on having every seat registered in the wiki's party namespace from
// accounts nothing has checked since, so a stray page event would still route
// to an agent whose access there was deliberately taken away.
//
// Called on every apply with the products the NEW company has, from the one
// place that sees the whole company — including the apply that leaves it with
// no Atlassian product at all, which is exactly the case a check guarding the
// register call would skip.
func (a *atlassianIdentities) retain(products []atlassianProduct) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for product := range a.sites {
		if slices.Contains(products, product) {
			continue
		}
		delete(a.sites, product)
		maps.DeleteFunc(a.byCred, func(key identityKey, _ string) bool {
			return key.product == product
		})
	}
}

// atlassianProductsOf is the Atlassian products a company configures.
//
// The BLOCK's presence, not its enabled flag: neither product has one, and a
// block that is present is a product the engine resolves identities for.
func atlassianProductsOf(in config.Integrations) []atlassianProduct {
	var out []atlassianProduct
	if in.Jira != nil {
		out = append(out, atlassian.ProductJira)
	}
	if in.Confluence != nil {
		out = append(out, atlassian.ProductConfluence)
	}
	return out
}

// backendOf is the party-registry namespace a product's account ids live in.
//
// TWO NAMESPACES for one account id, and deliberately: the same id arrives on
// two different event sources, and a registry that merged them would make a
// company running only one product resolve the other's events too — against
// seats whose credential was never checked there.
func backendOf(product atlassianProduct) string {
	if product == atlassian.ProductConfluence {
		return confluence.Backend
	}
	return jira.Backend
}

// seatCredential is one distinct credential and the seats that hold it.
//
// The HANDLES travel with it so a failed lookup can name them. A warning that
// says only "a credential did not resolve" sends an operator to grep for a
// handle that is not in the line, and the whole point of the line is that
// those seats have gone quiet.
type seatCredential struct {
	cred    atlassian.Credential
	handles []string
}

// atlassianSeatCredentials are the distinct credentials the company's agent
// seats hold, each with the seats holding it.
//
// DISTINCT because several seats may legitimately share one — a company
// mid-migration, or one that has not provisioned per-seat accounts yet — and
// resolving the same credential once per seat would spend N requests to learn
// one answer.
func atlassianSeatCredentials(c *Company, env *config.Resolver) []seatCredential {
	held := map[atlassian.Credential][]string{}
	for seat := range c.Org.AllRoles() {
		if cred := atlassian.CredentialOf(seat, env.Value); cred.Held() {
			held[cred] = append(held[cred], seat.Handle())
		}
	}
	// Sorted so a boot's log lines are diffable against the next one's.
	creds := slices.Collect(maps.Keys(held))
	slices.SortFunc(creds, func(x, y atlassian.Credential) int {
		if x.Token != y.Token {
			return strings.Compare(x.Token, y.Token)
		}
		return strings.Compare(x.Email, y.Email)
	})
	out := make([]seatCredential, 0, len(creds))
	for _, cred := range creds {
		handles := held[cred]
		slices.Sort(handles)
		out = append(out, seatCredential{cred: cred, handles: handles})
	}
	return out
}
