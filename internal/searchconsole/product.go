package searchconsole

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/words"
)

// Product is Search Console as the growth tools reach it.
type Product struct {
	company func() *config.Company
	client  *googlegrowth.Client
	// pages reads a page for the Indexing API's terms: the public-address
	// guard's client, never the one Google is reached through.
	pages *http.Client
}

// New is the product on the engine's active document, resolver, tokens,
// access log and apply leases. hc reaches Google and pages the company's own
// pages; both are nil in production.
func New(company func() *config.Company, env func() *config.Resolver, tokens googlegrowth.Tokens,
	audit googlegrowth.Audit, leases googlegrowth.Leaser, hc, pages *http.Client) *Product {
	if pages == nil {
		pages = httpx.Guarded(pageTimeout)
	}
	p := &Product{company: company, pages: pages, client: &googlegrowth.Client{
		Kind: string(Kind), Name: "Search Console", Company: company, Env: env, Tokens: tokens,
		Audit: audit, Leases: leases, HTTP: hc, Refused: vendorRefusal,
	}}
	p.client.BeforeRead = p.before
	return p
}

// Catalog is the product's catalog.
func (p *Product) Catalog() growth.Catalog {
	c, _ := growth.CatalogOf(string(Kind))
	return c
}

// seat is handle's block, nil where the seat is not on.
func (p *Product) seat(handle string) *config.RoleSearchConsole {
	c := p.company()
	if c == nil || !c.Integrations.GrowthEnabled(string(Kind)) {
		return nil
	}
	for role := range c.EachRole() {
		if strings.TrimSpace(role.IdentityKey()) == handle {
			b := role.Integrations.SearchConsole
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

// sites are the sites handle may reach: its own, within the company's.
func (p *Product) sites(handle string) []string {
	b := p.seat(handle)
	if b == nil {
		return nil
	}
	company := p.company().Integrations.SearchConsole
	return slices.DeleteFunc(slices.Clone(b.Sites), func(s string) bool { return !company.Reaches(s) })
}

// identityEmail is handle's growth identity's email, "" where the Google
// Cloud connection names no identity project.
func (p *Product) identityEmail(handle string) string {
	c := p.company()
	if c == nil || c.Integrations.GoogleCloud == nil || c.Integrations.GoogleCloud.Provisioning == nil {
		return ""
	}
	project, ok := config.GoogleCloudServiceAccount(c.Integrations.GoogleCloud.Provisioning.Provisioner)
	if !ok {
		return ""
	}
	return googleauth.AccountEmail(googleauth.GrowthAccountID(c.Name, handle), project)
}

// refusal is a call refused, in words the agent acts on.
func refusal(kind growth.RefusalKind, said string) error { return growth.Refuse(kind, said) }

// vendorRefusal says Google's refusal of req in words the agent acts on: a
// 403 names the permissions that may make the call; nil passes Google's
// error on. Either way Google's own error stays readable by the engine.
func vendorRefusal(req googlegrowth.Request, api *googleauth.APIError) error {
	if said := vendorWords(req, api); said != nil {
		return &googleRefused{said: said, api: api}
	}
	return nil
}

// vendorWords is Google's refusal of req in the agent's words, nil for one
// passed on as Google answered it.
func vendorWords(req googlegrowth.Request, api *googleauth.APIError) error {
	switch api.Status {
	case http.StatusForbidden:
		return refusal(growth.RefusedForbidden, fmt.Sprintf("Search Console refused %s (%s). This agent's "+
			"Google identity needs one of these permissions on the property: %s. A person changes it in "+
			"Search Console, Settings, Users and permissions.", req.Op.ID, api.Answer(), roleNames(req.Op.Roles)))
	case http.StatusTooManyRequests:
		return refusal(growth.RefusedUnavailable, "Google refused more requests for now ("+api.Answer()+"). "+
			"Search Analytics' load quota frees up within 15 minutes; URL Inspection's and the Indexing API's "+
			"daily quotas reset at midnight Pacific time.")
	case http.StatusNotFound, http.StatusBadRequest, http.StatusConflict:
		return refusal(growth.RefusedInvalid, "Google answered "+api.Answer()+".")
	}
	return nil
}

// googleRefused is Google's refusal said for the agent, keeping Google's own
// error for the engine, which reads a before-state's 404 as an absent target.
type googleRefused struct {
	said error
	api  *googleauth.APIError
}

func (e *googleRefused) Error() string   { return e.said.Error() }
func (e *googleRefused) Unwrap() []error { return []error{e.said, e.api} }

// roleNames are the names of role IDs, for a person.
func roleNames(ids []string) string {
	if len(ids) == 0 {
		return "any"
	}
	var out []string
	for _, r := range roles() {
		if slices.Contains(ids, r.ID) {
			out = append(out, r.Name)
		}
	}
	return words.Or(out)
}

// inScope refuses req where the site it acts in is not one of the seat's. A
// call naming no site is a listing, cut to the seat's sites.
func (p *Product) inScope(handle string, req googlegrowth.Request) error {
	if p.seat(handle) == nil {
		return refusal(growth.RefusedForbidden, "this agent is off at Search Console. No vendor was reached.")
	}
	sites := p.sites(handle)
	if req.Account != "" && !slices.Contains(sites, req.Account) {
		return refusal(growth.RefusedForbidden, "site "+req.Account+" is not one of this agent's sites ("+
			words.And(sites)+"). No vendor was reached.")
	}
	return nil
}

// Denied records that an administrator denied the held call, or that it
// lapsed: its one answer.
func (p *Product) Denied(ctx context.Context, call growth.Call) error {
	req, _, _ := p.requestOf(call)
	p.client.Denied(ctx, call, req, p.toolClass(call.Tool))
	return nil
}

// toolClass is the class of the tool named, which every record of its call
// names: the class the call was made, or asked and answered, at.
func (p *Product) toolClass(name string) growth.Class {
	tool, _ := p.Catalog().Tool(name)
	return tool.Class
}
