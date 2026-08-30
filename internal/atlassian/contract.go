package atlassian

import "strings"

// What a Crewlet agent is meant to be able to do, per product.
//
// # It names PERMISSIONS, not a role
//
// The obvious shape is "put the agent in the Developer role and be done". It
// does not survive contact with a real instance: a role's meaning is set by
// the tenant's own permission scheme, so "Developer" is administrative on one
// site and read-mostly on the next. The only reliable statement is a list of
// the individual permissions the agent needs, checked against what the
// product itself says the agent holds.
//
// # Forbidden is not the complement of Allowed
//
// Crewlet never grants anything on this list — it cannot; container
// permissions are the tenant's. It is here because a tenant's scheme can
// attach these to whatever role the agent lands in, and an agent that can
// delete a space is a fact its operator has to know. So every placement is
// checked against both halves, and a hit on the second is REPORTED rather
// than accepted in silence. The two lists are asked for in one round trip
// because a second call for the forbidden half is the call that gets dropped,
// and its absence then reads as "nothing forbidden was granted".

// contract states what an agent may and may not do in one product.
type contract struct {
	Allowed   []string
	Forbidden []string
}

var contracts = map[Product]contract{
	ProductJira: {
		Allowed: []string{
			"BROWSE_PROJECTS",
			"CREATE_ISSUES",
			"EDIT_ISSUES",
			"ADD_COMMENTS",
			"TRANSITION_ISSUES",
			"ASSIGNABLE_USER",
			"ASSIGN_ISSUES",
			"LINK_ISSUES",
			"CREATE_ATTACHMENTS",
		},
		Forbidden: []string{
			"ADMINISTER_PROJECTS",
			"DELETE_ISSUES",
			"DELETE_ALL_COMMENTS",
			"DELETE_ALL_ATTACHMENTS",
			"DELETE_ALL_WORKLOGS",
			"MANAGE_SPRINTS_PERMISSION",
			"MODIFY_REPORTER",
			"EDIT_ALL_COMMENTS",
			"MOVE_ISSUES",
			"ARCHIVE_ISSUES",
		},
	},
	// Confluence names a permission as an OPERATION on a TARGET, and the
	// pair is the unit: "delete:comment" and "delete:space" are very
	// different rights. Both halves travel as one string and are split only
	// where a call needs them apart.
	ProductConfluence: {
		Allowed: []string{
			"read:space",
			"create:page",
			"update:page",
			"create:comment",
			"create:attachment",
			"create:blogpost",
			"update:blogpost",
		},
		Forbidden: []string{
			"administer:space",
			"delete:space",
			"delete_space:space",
			"archive_space:space",
			"delete:page",
			"delete:blogpost",
			"delete:comment",
			"delete:attachment",
			"export:space",
			"export_content:space",
			"restrict_content:space",
			"manage_users:space",
			"manage_guest_users:space",
			"manage_templates:space",
			"manage_public_links:space",
		},
	},
}

// Allowed is the contract permissions for a product, for a report that has to
// name what an agent is short of.
func Allowed(p Product) []string { return append([]string(nil), contracts[p].Allowed...) }

// Forbidden is the permissions Crewlet never grants and always checks for.
func Forbidden(p Product) []string { return append([]string(nil), contracts[p].Forbidden...) }

// PermissionQuery asks about both halves in one round trip.
func PermissionQuery(p Product) []string {
	c := contracts[p]
	out := make([]string, 0, len(c.Allowed)+len(c.Forbidden))
	out = append(out, c.Allowed...)
	out = append(out, c.Forbidden...)
	return out
}

// Classify turns a product's permission map into what is missing and what is
// excess.
//
// The two are separate because they call for OPPOSITE responses: missing is
// the operator's to grant, and excess is only the tenant's to revoke. A
// single "wrong" count would send an operator to the same screen for both and
// be right about half the time.
func Classify(p Product, held map[string]bool) (missing, excess []string) {
	c := contracts[p]
	for _, name := range c.Allowed {
		if !held[name] {
			missing = append(missing, name)
		}
	}
	for _, name := range c.Forbidden {
		if held[name] {
			excess = append(excess, name)
		}
	}
	return missing, excess
}

// agentScopes are the token scopes an agent needs per product.
//
// A token carries ONLY the products its agent is enabled for, so a
// documentation agent holds no credential that can move a sprint. Scopes are
// fixed at mint time and cannot be widened afterwards, which is why the
// scope set is derived from the agent's products rather than from the
// company's: a seat that gains a product needs a new credential, not a
// broader one.
var agentScopes = map[Product][]string{
	ProductJira: {
		"read:jira-work",
		"write:jira-work",
		// The engine's Jira user lookups resolve a colleague's account id
		// before a seat can assign work or @mention anyone.
		"read:jira-user",
	},
	ProductConfluence: {
		"read:confluence-content.all",
		"write:confluence-content",
		"read:confluence-space.summary",
		// Reading a space's permissions means resolving the groups they
		// were granted to, and an agent's own membership is the only way
		// to know which of those are its own.
		"read:confluence-user",
	},
}

// Scopes are the scopes a credential covering these products needs, in a
// stable order.
//
// ORDERED BY [Products], never by the caller's slice: two scope lists are
// compared element by element in more than one place, and an order that
// followed whoever asked would make the same credential look different to two
// readers.
func Scopes(products []Product) []string {
	out := []string{}
	for _, p := range Products {
		for _, want := range products {
			if want != p {
				continue
			}
			out = append(out, agentScopes[p]...)
			break
		}
	}
	return out
}

// productARIs name each product's resource on a site.
//
// NOTE jira-software rather than jira. The plain `jira` ARI is ACCEPTED by
// the grant endpoint and silently grants nothing — a run that reports success
// and agents that can do nothing, with no error anywhere to explain it.
var productARIs = map[Product]string{
	ProductJira:       "jira-software",
	ProductConfluence: "confluence",
}

// MemberRoleARI makes an account a licensed user of a product: able to work
// in it, with no administration of the product itself.
func MemberRoleARI(p Product) string {
	return "ari:cloud:" + productARIs[p] + "::role/product/member"
}

// SiteResourceARI names the product on the site a licence applies to.
func SiteResourceARI(p Product, cloudID string) string {
	return "ari:cloud:" + productARIs[p] + "::site/" + cloudID
}

// restPrefix is the REST root each product is served from, under the /ex
// gateway.
//
// Confluence is served from /ex/confluence AND under /wiki; addressed under
// /ex/jira it answers 401, as though the token lacked the scope rather than
// as though the address were wrong. That misreading costs an afternoon, so
// the prefix is a table rather than a string built at each call site.
var restPrefix = map[Product]string{
	ProductJira:       "/rest/api/3",
	ProductConfluence: "/wiki/rest/api",
}

// gatewayHost is the /ex segment each product is served from.
var gatewayHost = map[Product]string{
	ProductJira:       "jira",
	ProductConfluence: "confluence",
}

// ProductBase is the REST base an agent's own credential reads a product on.
func ProductBase(gateway string, p Product, cloudID string) string {
	return strings.TrimRight(gateway, "/") + "/ex/" + gatewayHost[p] + "/" + cloudID + restPrefix[p]
}
