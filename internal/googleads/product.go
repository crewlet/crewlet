package googleads

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// Staged answers the highest class a batch job's staged operations came to,
// from the access log's records of them (the engine's GrowthAudit); false
// where it holds none.
type Staged interface {
	StagedClass(ctx context.Context, kind, account, batchJob string) (growth.Class, bool, error)
}

// Product is Google Ads as the growth tools reach it.
type Product struct {
	company func() *config.Company
	client  *googlegrowth.Client
	staged  Staged
	now     func() time.Time
}

// New is the product on the engine's active document, resolver, tokens,
// access log, apply leases and staged batch jobs; hc is nil in production.
func New(company func() *config.Company, env func() *config.Resolver, tokens googlegrowth.Tokens,
	audit googlegrowth.Audit, leases googlegrowth.Leaser, staged Staged, hc *http.Client) *Product {
	p := &Product{company: company, staged: staged, now: time.Now, client: &googlegrowth.Client{
		Kind: string(Kind), Name: "Google Ads", Company: company, Env: env, Tokens: tokens, Audit: audit,
		Leases: leases, HTTP: hc, Refused: vendorRefusal,
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

// newRequest is op's request, acting in the customer its path names, sent
// through that customer's manager where the company names one.
func (p *Product) newRequest(op growth.Operation, path, query map[string]string, body map[string]any) (
	googlegrowth.Request, error) {
	req, err := googlegrowth.NewRequest(op, path, query, body)
	if err != nil {
		return req, err
	}
	if customer := path["customersId"]; customer != "" {
		req.Account = "customers/" + customer
		if manager := p.block().ManagerOf(customer); manager != "" {
			req.Header = http.Header{"Login-Customer-Id": {manager}}
		}
	}
	return req, nil
}

// customerOf is the customer req acts in, "" where none (a planning method
// that names no account).
func customerOf(req googlegrowth.Request) string {
	id, _ := strings.CutPrefix(req.Account, "customers/")
	return id
}

// inScope refuses req where it acts in a customer the seat may not reach, or
// where an operation in it names another customer's resource.
func (p *Product) inScope(handle string, req googlegrowth.Request) error {
	b := p.seat(handle)
	if b == nil {
		return refusal(growth.RefusedForbidden, "this agent is off at Google Ads. No vendor was reached.")
	}
	customer := customerOf(req)
	if customer == "" {
		return nil
	}
	if !slices.Contains(b.Customers, customer) || !p.block().Reaches(customer) {
		return refusal(growth.RefusedForbidden, "account "+customer+" is not one of this agent's accounts ("+
			words.Or(b.Customers)+"). No vendor was reached.")
	}
	ops, _ := parse(req.Op, req.Body)
	for _, name := range namesIn(ops) {
		if owner, _, _ := kindOf(name); owner != customer {
			return refusal(growth.RefusedForbidden, fmt.Sprintf("an operation names %s, which is not in account %s "+
				"the call acts in. No vendor was reached.", name, customer))
		}
	}
	return nil
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

// namesIn is every resource name the operations hold, at any depth: what each
// acts on and every resource its fields point at.
func namesIn(ops []operation) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if _, _, ok := kindOf(t); ok {
				out = append(out, t)
			}
		case map[string]any:
			for _, inner := range t {
				walk(inner)
			}
		case []any:
			for _, inner := range t {
				walk(inner)
			}
		}
	}
	for _, o := range ops {
		walk(o.name)
		walk(o.object)
	}
	return out
}
