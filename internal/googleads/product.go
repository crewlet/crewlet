package googleads

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// Product is Google Ads as the growth tools reach it.
type Product struct {
	company func() *config.Company
	client  *googlegrowth.Client
	now     func() time.Time
}

// New is the product on the engine's active document, resolver, tokens,
// access log and apply leases; hc is nil in production.
func New(company func() *config.Company, env func() *config.Resolver, tokens googlegrowth.Tokens,
	audit googlegrowth.Audit, leases googlegrowth.Leaser, hc *http.Client) *Product {
	p := &Product{company: company, now: time.Now, client: &googlegrowth.Client{
		Kind: string(Kind), Name: "Google Ads", Company: company, Env: env, Tokens: tokens, Audit: audit,
		Leases: leases, HTTP: hc, Refused: vendorRefusal, Signer: growth.SignerOwn,
	}}
	p.client.BeforeRead = p.targets
	return p
}

// Catalog is the product's catalog.
func (p *Product) Catalog() growth.Catalog {
	c, _ := growth.CatalogOf(string(Kind))
	return c
}

// block is the company's Google Ads block, nil where it is off.
func (p *Product) block() *config.GoogleAds {
	c := p.company()
	if c == nil || !c.Integrations.GrowthEnabled(string(Kind)) {
		return nil
	}
	return c.Integrations.GoogleAds
}

// seat is handle's block, nil where the seat is not on.
func (p *Product) seat(handle string) *config.RoleGoogleAds {
	if p.block() == nil {
		return nil
	}
	for role := range p.company().EachRole() {
		if strings.TrimSpace(role.IdentityKey()) == handle {
			b := role.Integrations.GoogleAds
			if b != nil && config.SeatAccessOn(b.Enabled) {
				return b
			}
			return nil
		}
	}
	return nil
}

// Level is the seat's level, "" where it is off.
func (p *Product) Level(handle string) growth.Level {
	b := p.seat(handle)
	if b == nil {
		return ""
	}
	return b.LevelOrDefault()
}

// newRequest is op's request, acting in the customer its path names or,
// where it names none, its body's customer_id, which Google reads in either
// spelling (a reach forecast's user lists), sent through that customer's
// manager where the company names one. A body naming a field twice with two
// values is refused ([canonical]).
func (p *Product) newRequest(op growth.Operation, path, query map[string]string, body map[string]any) (
	googlegrowth.Request, error) {
	req, err := googlegrowth.NewRequest(op, path, query, body)
	if err != nil {
		return req, err
	}
	if _, err = canonical(body, "body."); err != nil {
		return req, err
	}
	// THE PATH'S CUSTOMER IS THE TEMPLATE'S: NewRequest refuses a parameter
	// the path does not name.
	customer := path["customersId"]
	if named := googlegrowth.Lookup(body, "customerId"); named != nil {
		id, _ := named.(string)
		if !customerID().MatchString(id) || (customer != "" && id != customer) {
			return req, refusal(growth.RefusedInvalid, fmt.Sprintf("body.customer_id names %s, which is not the "+
				"account the call acts in (%s): name one customer ID, once. No vendor was reached.",
				googlegrowth.Words(named), cmp.Or(customer, "none")))
		}
		customer = id
	}
	if customer != "" {
		req.Account = "customers/" + customer
		if manager := p.block().ManagerOf(customer); manager != "" {
			req.Header = http.Header{"Login-Customer-Id": {manager}}
		}
	}
	return req, nil
}

// customerID is a Google Ads customer ID: digits alone.
var customerID = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,20}$`) })

// customerOf is the customer req acts in, "" where none (a planning method
// that names no account).
func customerOf(req googlegrowth.Request) string {
	id, _ := strings.CutPrefix(req.Account, "customers/")
	return id
}

// inScope refuses req where it acts in a customer the seat may not reach, or
// where its body names a resource of another customer than the one it acts
// in, or another customer itself, at any depth: what an operation acts on and
// every resource its fields point at. A field whose purpose is to name
// another account (otherAccount) may.
func (p *Product) inScope(handle string, req googlegrowth.Request) error {
	b := p.seat(handle)
	if b == nil {
		return refusal(growth.RefusedForbidden, "this agent is off at Google Ads. No vendor was reached.")
	}
	customer := customerOf(req)
	if customer != "" && (!slices.Contains(b.Customers, customer) || !p.block().Reaches(customer)) {
		return refusal(growth.RefusedForbidden, "account "+customer+" is not one of this agent's accounts ("+
			words.Or(b.Customers)+"). No vendor was reached.")
	}
	for _, n := range namesIn(req.Body, "") {
		if owner, _ := ownerOf(n.name); owner != customer && n.path != otherAccount(req.Op.ID) {
			return refusal(growth.RefusedForbidden, fmt.Sprintf("the body names %s, which is not in the account "+
				"the call acts in (%s). No vendor was reached.", n.name, cmp.Or(customer, "none")))
		}
	}
	return nil
}

// otherAccount is the body field, of the methods with one, whose purpose is
// to name another Google Ads account than the one the call acts in, which may
// be outside the seat's accounts too: a manager's new client, the manager a
// client moves to, the account a product link joins. Each method is admin.
func otherAccount(id string) string {
	switch id {
	case "googleads.customers.customerClientLinks.mutate":
		return "operation.create.client_customer"
	case "googleads.customers.customerManagerLinks.moveManagerLink":
		return "new_manager"
	case "googleads.customers.productLinks.create":
		return "product_link.google_ads.customer"
	}
	return ""
}

// vendorRefusal says Google's refusal of req in words the agent acts on.
func vendorRefusal(req googlegrowth.Request, api *googleauth.APIError) error {
	switch api.Status {
	case http.StatusForbidden:
		return refusal(growth.RefusedForbidden, fmt.Sprintf("Google Ads refused %s (%s). This agent's Google identity "+
			"needs one of these access levels on the account: %s; a person changes it in Google Ads, Admin, Access "+
			"and security. A manager's access needs the account's manager named in integrations.google_ads.customers.",
			req.Op.ID, api.Answer(), roleNames(req.Op.Roles)))
	case http.StatusTooManyRequests:
		return refusal(growth.RefusedUnavailable, "Google Ads refused more requests for now ("+api.Answer()+
			"). The daily operations limit frees over a sliding 24 hours.")
	case http.StatusNotFound, http.StatusBadRequest, http.StatusConflict:
		return refusal(growth.RefusedInvalid, "Google Ads answered "+api.Answer()+".")
	}
	return nil
}

// roleNames are the names of role IDs, for a person.
func roleNames(ids []string) string {
	if len(ids) == 0 {
		return "any level"
	}
	var out []string
	for _, r := range roles() {
		if slices.Contains(ids, r.ID) {
			out = append(out, r.Name)
		}
	}
	return words.Or(out)
}

// Denied records that an administrator denied the held call, or that it
// lapsed: its one answer.
func (p *Product) Denied(ctx context.Context, call growth.Call) error {
	req, _ := p.requestOf(call)
	p.client.Denied(ctx, call, req, p.toolClass(call.Tool))
	return nil
}

// toolClass is the class of the tool named, which every record of its call
// names: the class the call was made, or asked and answered, at.
func (p *Product) toolClass(name string) growth.Class {
	tool, _ := p.Catalog().Tool(name)
	return tool.Class
}

// named is a resource name a body holds, at its dotted snake_case path; a
// list's items share the list's path.
type named struct{ path, name string }

// namesIn is every resource name, and every customer named alone, a body
// holds below path, at any depth.
func namesIn(v any, path string) []named {
	switch t := v.(type) {
	case string:
		if _, ok := ownerOf(t); ok {
			return []named{{path: path, name: t}}
		}
	case map[string]any:
		var out []named
		for _, k := range slices.Sorted(maps.Keys(t)) {
			out = append(out, namesIn(t[k], strings.TrimPrefix(path+"."+snake(k), "."))...)
		}
		return out
	case []any:
		var out []named
		for _, inner := range t {
			out = append(out, namesIn(inner, path)...)
		}
		return out
	}
	return nil
}

// customerName is a customer named alone: customers/{id}.
var customerName = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^customers/([0-9]{1,20})$`) })

// ownerOf is the customer a resource name belongs to, or the one a customer
// name names; ok is false for anything else.
func ownerOf(name string) (customer string, ok bool) {
	if owner, _, ok := kindOf(name); ok {
		return owner, true
	}
	if m := customerName().FindStringSubmatch(name); m != nil {
		return m[1], true
	}
	return "", false
}
