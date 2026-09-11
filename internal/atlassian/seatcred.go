package atlassian

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// WHERE A SEAT'S ATLASSIAN CREDENTIAL LIVES, AND WHO MAY READ IT.
//
// One Atlassian account authenticates BOTH products. An agent gets one service
// account, its token is written into one `mcp_env` slot, and Jira and
// Confluence each authenticate as it. So "does this seat have an Atlassian
// credential, and where" is ONE question with one answer.
//
// It was answered in three places, by three lists that had drifted:
//
//	internal/jira        servers {atlassian, jira}        keys {JIRA_API_TOKEN, JIRA_PERSONAL_TOKEN, JIRA_TOKEN, ATLASSIAN_API_TOKEN, Authorization}
//	internal/confluence  servers {atlassian, confluence}  keys {CONFLUENCE_API_TOKEN, CONFLUENCE_PERSONAL_TOKEN, CONFLUENCE_TOKEN, ATLASSIAN_API_TOKEN}
//	internal/atlassian   servers {atlassian, jira, confluence}  keys {JIRA_API_TOKEN, CONFLUENCE_API_TOKEN, ATLASSIAN_API_TOKEN, JIRA_PERSONAL_TOKEN, CONFLUENCE_PERSONAL_TOKEN}
//
// Measured: a seat provisioned by this package held `mcp_env.atlassian` with
// `JIRA_API_TOKEN` and `JIRA_USERNAME` — the spelling this package's own plan
// note tells operators to use. Jira read it and reported the seat ready.
// Confluence's list has neither key, so the same account, on the same seat, in
// the same block, reported "no Confluence credential yet" for ever. Nothing was
// misconfigured and nothing could be done about it from the configuration.
//
// # Why this package
//
// It owns the ACCOUNT: it mints the service account, names it, and records the
// address Atlassian assigned back onto the seat. The products are consumers of
// an identity this package creates. `internal/jira` and `internal/confluence`
// therefore import it, which is the real direction of the dependency and
// introduces no cycle — this package imports neither.
//
// # The rules, and why each is the way it is
//
// SERVER ORDER IS THE PRODUCT'S OWN NAME, THEN THE SHARED `atlassian` BLOCK.
// `mcp_env.jira` and `mcp_env.confluence` exist only where an operator
// deliberately configured a product-specific server, so a specific declaration
// beats the shared one. This flips Jira's previous order, which tried the
// shared block first; nothing has shipped, and the alternative — shared-first
// over a merged key list — would let a split Data Center pair authenticate Jira
// with its Confluence token.
//
// A BLOCK COUNTS ONLY IF IT HOLDS A KEY THIS PRODUCT CAN READ. Selection is by
// content rather than by position, so an empty or wrong-product `atlassian`
// block cannot shadow a populated `jira` one.
//
// THE CROSS-PRODUCT FALLBACK IS THE FIX, AND IT IS DELIBERATELY NARROW. In the
// SHARED block only, each product will also read the sibling's `*_API_TOKEN`.
// On Cloud one API token belongs to the ACCOUNT and authenticates both
// products, so a shared block holding `JIRA_API_TOKEN` is a Confluence
// credential too. A `*_PERSONAL_TOKEN` is not: a Data Center PAT is issued by
// one product and refused by the other, so those spellings — and the bare
// `*_TOKEN` — never cross. The spelling already carries the distinction, so
// this needs no deployment flag.
//
// THE ADDRESS IS THE ACCOUNT'S, so every spelling counts for both products,
// the product's own first. Atlassian Cloud authenticates an API token as
// Basic base64(address:token) and refuses it as a bearer — the same credential,
// rejected purely on which scheme carried it — so a seat holding only a token
// authenticates as nobody.

// Product is which Atlassian product is asking.
//
// It changes the ORDER preferences are tried in and nothing else: the account
// is the same one either way.
type Product string

const (
	// ProductJira and ProductConfluence prefer their own spellings.
	ProductJira       Product = "jira"
	ProductConfluence Product = "confluence"

	// ProductAny is the provisioner's view: any spelling identifies the
	// seat, because what it is deciding is whether to create an account
	// rather than which product will authenticate with it.
	ProductAny Product = ""
)

// Credential is a seat's own Atlassian identity, as its tools hold it.
//
// COMPARABLE, because the engine keys its resolved-identity cache on one: a
// lookup is a function of the credential, so two seats sharing a token share an
// answer and a rotation is a cache miss.
//
// The EMPTY EMAIL IS MEANINGFUL rather than missing. Cloud authenticates
// base64(email:token) and Data Center takes a bearer PAT, so an absent address
// selects the bearer scheme rather than indicating an incomplete credential.
type Credential struct{ Token, Email string }

// Held reports a seat that carries a credential at all.
func (c Credential) Held() bool { return strings.TrimSpace(c.Token) != "" }

// servers is the mcp_env blocks this product reads, in order.
func (p Product) servers() []string {
	switch p {
	case ProductJira:
		return []string{"jira", sharedServer}
	case ProductConfluence:
		return []string{"confluence", sharedServer}
	default:
		return []string{sharedServer, "jira", "confluence"}
	}
}

// tokenKeys is the spellings this product reads a token under, in order.
//
// shared says whether the block being read is the SHARED one, which is the only
// place the sibling product's API token counts.
func (p Product) tokenKeys(shared bool) []string {
	var own, sibling []string
	switch p {
	case ProductJira:
		own = []string{"JIRA_API_TOKEN", "JIRA_PERSONAL_TOKEN", "JIRA_TOKEN"}
		sibling = []string{"CONFLUENCE_API_TOKEN"}
	case ProductConfluence:
		own = []string{"CONFLUENCE_API_TOKEN", "CONFLUENCE_PERSONAL_TOKEN", "CONFLUENCE_TOKEN"}
		sibling = []string{"JIRA_API_TOKEN"}
	default:
		// The provisioner reads every spelling wherever it finds it: it is
		// deciding whether this seat has opted in at all.
		return []string{
			"JIRA_API_TOKEN", "CONFLUENCE_API_TOKEN", "ATLASSIAN_API_TOKEN",
			"JIRA_PERSONAL_TOKEN", "CONFLUENCE_PERSONAL_TOKEN",
			"JIRA_TOKEN", "CONFLUENCE_TOKEN", authorizationKey,
		}
	}
	keys := append(own, "ATLASSIAN_API_TOKEN", authorizationKey)
	if shared {
		keys = append(keys, sibling...)
	}
	return keys
}

// emailKeys is the spellings this product reads an address under, in order.
//
// ALL FIVE FOR BOTH, because the address belongs to the account rather than to
// a product: whichever key the operator reached for names the same mailbox.
func (p Product) emailKeys() []string {
	jira := []string{"JIRA_USERNAME", "JIRA_EMAIL"}
	conf := []string{"CONFLUENCE_USERNAME", "CONFLUENCE_EMAIL"}
	if p == ProductConfluence {
		jira, conf = conf, jira
	}
	return append(append(jira, "ATLASSIAN_EMAIL"), conf...)
}

const (
	// sharedServer is the mcp_env block the community MCP server uses, which
	// covers both products under one entry and is where a provisioned seat's
	// credential lands.
	sharedServer = "atlassian"

	// authorizationKey is a whole header rather than a token, and the scheme
	// is stripped where it is read. See [CredentialFor].
	authorizationKey = "Authorization"
)

// CredentialOf is one seat's Atlassian credential for this product, resolved.
//
// A HUMAN SEAT HOLDS NONE. A person has their own Atlassian account, and
// reading a credential for one would report an identity this engine neither
// created nor manages.
func CredentialOf(p Product, seat *org.Role, value func(string) string) Credential {
	if seat == nil || seat.IsHuman() || value == nil {
		return Credential{}
	}
	return CredentialFor(p, seat.MCPEnv, value)
}

// CredentialFor is the same answer from a raw mcp_env.
//
// THE MAP FORM EXISTS because the setup surface holds a [config.Role] and the
// engine holds an [org.Role], and the two must not each grow their own scan.
func CredentialFor(p Product, env map[string]map[string]string, value func(string) string) Credential {
	cred, _ := CredentialAt(p, env, value)
	return cred
}

// CredentialAt is the credential and the mcp_env address it was read from,
// as `<server>.<KEY>`, or empty where the seat holds none.
//
// The WHERE is what a setup screen reports and what a plan note names, so it is
// returned rather than re-derived by a second scan that could disagree.
func CredentialAt(
	p Product, env map[string]map[string]string, value func(string) string,
) (Credential, string) {
	if value == nil {
		return Credential{}, ""
	}
	for _, server := range p.servers() {
		block := env[server]
		if len(block) == 0 {
			continue
		}
		for _, key := range p.tokenKeys(server == sharedServer) {
			raw := strings.TrimSpace(value(block[key]))
			if raw == "" {
				continue
			}
			// A `Bearer <pat>` carries the credential behind a scheme, and
			// stripping it is what lets one config shape serve both an HTTP
			// MCP server and this lookup. A Basic header is left alone: its
			// payload is already email:token, and re-encoding it would
			// produce a credential that authenticates as nobody.
			cred := Credential{Token: strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))}
			// THE ADDRESS COMES FROM THE SAME BLOCK the token did. A seat
			// with a Data Center PAT under `jira` and a Cloud address under
			// `atlassian` holds two credentials, not one.
			for _, ek := range p.emailKeys() {
				if address := strings.TrimSpace(value(block[ek])); address != "" {
					cred.Email = address
					break
				}
			}
			return cred, fmt.Sprintf("%s.%s", server, key)
		}
	}
	return Credential{}, ""
}

// SeatEmail is the Atlassian account one seat authenticates as, or empty.
//
// THE ACCOUNT, NOT THE SLOT. Atlassian assigns the address when it creates a
// service account and the pass records it on the seat, so this is the agent's
// identity at the app rather than a note about where a value is kept.
//
// The value is returned AS WRITTEN, which may be a whole `${VAR}`: this
// package holds no resolver, and the caller that displays it does.
func SeatEmail(env map[string]map[string]string) string {
	_, value := seatEmail(env)
	return value
}

// seatEmail finds a seat's address slot, and says where.
func seatEmail(env map[string]map[string]string) (where, value string) {
	for _, server := range ProductAny.servers() {
		block := env[server]
		if len(block) == 0 {
			continue
		}
		for _, key := range ProductAny.emailKeys() {
			if v := strings.TrimSpace(block[key]); v != "" {
				return fmt.Sprintf("%s.%s", server, key), v
			}
		}
	}
	return "", ""
}

// seatCredential finds a seat's Atlassian credential slot, and says where.
//
// UNRESOLVED, because the provisioner needs the `${VAR}` reference itself:
// what it plans is where a minted token will be WRITTEN.
func seatCredential(env map[string]map[string]string) (where, value string) {
	for _, server := range ProductAny.servers() {
		block := env[server]
		if len(block) == 0 {
			continue
		}
		for _, key := range ProductAny.tokenKeys(server == sharedServer) {
			if v := strings.TrimSpace(block[key]); v != "" {
				return fmt.Sprintf("%s.%s", server, key), v
			}
		}
	}
	return "", ""
}
