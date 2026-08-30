// Package atlassian is the organization half of Jira and Confluence.
//
// `internal/jira` and `internal/confluence` are each one PRODUCT: a parser, a
// routing tier order, a content format. What they share is not a product at
// all — it is an IDENTITY. One Atlassian account is a person (or an agent) in
// both products at once, authenticates both with one credential, and is named
// by one account id in both payloads. This package owns that identity, and
// the organization the identities live in.
//
// # Two credential planes, and they never mix
//
// The ADMIN plane is the organization's own API key, sent as a bearer against
// api.atlassian.com/admin. It creates service accounts, mints their tokens
// and grants product licences. It must be created WITHOUT SCOPES: the
// account-management service refuses a scoped key with 403 whatever scopes it
// holds, and the api-access service Atlassian's reference documents for
// service accounts does not serve the collection at all.
//
// The PRODUCT plane is an agent's own credential, sent as Basic
// base64(email:token) against api.atlassian.com/ex/{product}/{cloudID}. The
// org key is rejected outright there. Everything this package does on that
// plane is a READ, and deliberately: what an agent may do in a project is the
// tenant's to decide, and this package's job is to say truthfully what it
// ended up with.
//
// # Cloud only, for provisioning
//
// The admin plane does not exist on Data Center, where a personal access
// token can only be minted for the calling user. So `crewlet atlassian
// provision` refuses a Data Center address by name, and `crewlet jira
// provision` stays the Data Center path — it reports the seat identities and
// registers the inbound webhook, which is everything Data Center allows.
// See decisions/706-atlassian-service-accounts.md.
package atlassian

import "slices"

// Product is one Atlassian app a seat can hold a licence for.
//
// An agent's ACCOUNT, its address and its credential are shared across
// products; only the licence and the permissions differ. That is why this is
// a product rather than a whole integration: two products, one identity.
type Product string

const (
	// ProductJira is Jira Software, Work Management or Service Management —
	// one licence covers the site's Jira, whichever flavours it runs.
	ProductJira Product = "jira"
	// ProductConfluence is Confluence.
	ProductConfluence Product = "confluence"
)

// Products is every product this build provisions, in READING ORDER.
//
// The order is load-bearing rather than cosmetic: scopes, licences,
// verification and the report all iterate this slice, so two lists derived
// from different callers' product orders are still comparable element by
// element. A set that sorted differently per caller would make drift
// detection report a change on every run.
var Products = []Product{ProductJira, ProductConfluence}

// Valid reports a product this build serves, so an unknown value off a
// config is a value rather than a panic.
func (p Product) Valid() bool { return slices.Contains(Products, p) }

// Label is the product's name as a person writes it, for a report.
func (p Product) Label() string {
	switch p {
	case ProductJira:
		return "Jira"
	case ProductConfluence:
		return "Confluence"
	default:
		return string(p)
	}
}

// ProductNames is every product as a string, for an error that has to name
// the closed set.
func ProductNames() []string {
	out := make([]string, 0, len(Products))
	for _, p := range Products {
		out = append(out, string(p))
	}
	return out
}
