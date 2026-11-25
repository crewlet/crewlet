package googleanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// Product is Google Analytics as the growth tools reach it.
type Product struct {
	company func() *config.Company
	client  *googlegrowth.Client
}

// New is the product on the engine's active document, resolver, tokens,
// access log and apply leases; hc is nil in production.
func New(company func() *config.Company, env func() *config.Resolver, tokens googlegrowth.Tokens,
	audit googlegrowth.Audit, leases googlegrowth.Leaser, hc *http.Client) *Product {
	p := &Product{company: company, client: &googlegrowth.Client{
		Kind: string(Kind), Name: "Google Analytics", Company: company, Env: env, Tokens: tokens,
		Audit: audit, Leases: leases, HTTP: hc, Secrets: []string{"secretValue", "channelToken"},
		Refused: vendorRefusal, ReadScopes: readScope, Signer: growth.SignerOwn,
	}}
	p.client.BeforeRead = p.beforeRead
	return p
}

// Catalog is the product's catalog.
func (p *Product) Catalog() growth.Catalog {
	c, _ := growth.CatalogOf(string(Kind))
	return c
}

// seat is handle's block, nil where the seat is not on.
func (p *Product) seat(handle string) *config.RoleGoogleAnalytics {
	c := p.company()
	if c == nil || !c.Integrations.GrowthEnabled(string(Kind)) {
		return nil
	}
	for role := range c.EachRole() {
		if strings.TrimSpace(role.IdentityKey()) == handle {
			b := role.Integrations.GoogleAnalytics
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

// refusal is a call refused, in words the agent acts on.
func refusal(kind growth.RefusalKind, said string) error { return growth.Refuse(kind, said) }

// vendorRefusal says Google's refusal of req in words the agent acts on: a
// 403 names the roles that may make the call; nil passes Google's error on.
func vendorRefusal(req googlegrowth.Request, api *googleauth.APIError) error {
	switch api.Status {
	case http.StatusForbidden:
		return refusal(growth.RefusedForbidden, fmt.Sprintf("Google Analytics refused %s (%s). This agent's "+
			"Google identity needs one of these roles on the property: %s. A person changes it in Google "+
			"Analytics, Admin, Property access management.", req.Op.ID, api.Answer(), roleNames(req.Op.Roles)))
	case http.StatusTooManyRequests:
		return refusal(growth.RefusedUnavailable, "Google Analytics refused more requests for now ("+api.Answer()+
			"). Hourly quotas free up within the hour; daily quotas reset at midnight Pacific time.")
	case http.StatusNotFound, http.StatusBadRequest, http.StatusConflict:
		return refusal(growth.RefusedInvalid, "Google Analytics answered "+api.Answer()+".")
	}
	return nil
}

// lowestRole is the least of ids, by the ladder, "any role" where it names none.
func lowestRole(ids []string) string {
	for _, r := range roles() {
		if slices.Contains(ids, r.ID) {
			return r.Name
		}
	}
	return "any role"
}

// roleNames are the names of role IDs, for a person.
func roleNames(ids []string) string {
	if len(ids) == 0 {
		return "any role"
	}
	var out []string
	for _, r := range roles() {
		if slices.Contains(ids, r.ID) {
			out = append(out, r.Name)
		}
	}
	return strings.Join(out, ", ")
}

// inScope refuses req where it acts in a property the seat may not reach, or
// an account any of whose properties it may not reach, or names nothing the
// seat's scope can hold: only a listing cut to the seat's properties and the
// opening of a new account act in none.
func (p *Product) inScope(ctx context.Context, handle string, req googlegrowth.Request) error {
	b := p.seat(handle)
	if b == nil {
		return refusal(growth.RefusedForbidden, "this agent is off at Google Analytics. No vendor was reached.")
	}
	if b.Role == config.GoogleAnalyticsMarketer && !req.Reads() && !slices.Contains(req.Op.Roles, roleMarketer) {
		// OPERATE AS MARKETER: its changes are Marketer's, whatever its identity holds.
		return refusal(growth.RefusedForbidden, req.Op.ID+" needs "+lowestRole(req.Op.Roles)+" or above in "+
			"Google Analytics, and this agent works as a Marketer: audiences, key events, attribution and event "+
			"create rules. A person raises it to Operate on the Agents tab. No vendor was reached.")
	}
	places, err := placesOf(req)
	if err != nil {
		return err
	}
	listing := req.Reads() && listsAccounts(req.Op.ID)
	if len(places) == 0 && !listing && req.Op.ID != actsInNone {
		return refusal(growth.RefusedForbidden, req.Op.ID+" names no property or account this agent's "+
			"properties can hold. No vendor was reached.")
	}
	for _, place := range places {
		if err := p.reaches(ctx, handle, b, place); err != nil {
			return err
		}
	}
	return nil
}

// reaches refuses place, a property or account, where the seat b may not
// reach it: a call on an account reaches every property in it.
func (p *Product) reaches(ctx context.Context, handle string, b *config.RoleGoogleAnalytics, place string) error {
	kind, id, _ := strings.Cut(place, "/")
	if kind == "properties" {
		if !slices.Contains(b.Properties, id) || !p.company().Integrations.GoogleAnalytics.Reaches(id) {
			return refusal(growth.RefusedForbidden, "property "+id+" is not one of this agent's properties ("+
				words.And(b.Properties)+"). No vendor was reached.")
		}
		return nil
	}
	props, err := p.accountProperties(ctx, handle, id)
	if err != nil {
		return err
	}
	for _, prop := range props {
		if !slices.Contains(b.Properties, prop) {
			return refusal(growth.RefusedForbidden, "account "+id+" holds property "+prop+", which is not one "+
				"of this agent's properties, and a call on the account reaches every property in it. No vendor "+
				"was reached.")
		}
	}
	if len(props) == 0 {
		return refusal(growth.RefusedForbidden, "account "+id+" holds none of this agent's properties. No vendor "+
			"was reached.")
	}
	return nil
}

// summary is one account and its properties as accountSummaries lists them.
type summary struct {
	Account     string `json:"account"`
	DisplayName string `json:"displayName"`
	Properties  []struct {
		Property    string `json:"property"`
		DisplayName string `json:"displayName"`
	} `json:"propertySummaries"`
}

// summaries is every account and property handle's identity reaches.
func (p *Product) summaries(ctx context.Context, handle string) ([]summary, error) {
	op, _ := p.Catalog().Operation(summariesOp)
	var out []summary
	token := ""
	for page := 0; page < maxPages; page++ {
		req, err := newRequest(op, nil, map[string]string{"pageSize": "200"}, nil)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Query.Set("pageToken", token)
		}
		raw, err := p.client.Send(ctx, handle, req)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Summaries []summary `json:"accountSummaries"`
			Next      string    `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("googleanalytics: decode the account summaries: %w", err)
		}
		out = append(out, resp.Summaries...)
		if resp.Next == "" {
			return out, nil
		}
		token = resp.Next
	}
	return nil, fmt.Errorf("googleanalytics: the account summaries did not end after %d pages", maxPages)
}

// maxPages bounds a listing the engine reads whole: 200 accounts a page.
const maxPages = 50

// summariesOp lists every account and property an identity reaches.
const summariesOp = "analyticsadmin.accountSummaries.list"

// accountProperties are the property IDs of account, as handle sees it.
func (p *Product) accountProperties(ctx context.Context, handle, account string) ([]string, error) {
	all, err := p.summaries(ctx, handle)
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if strings.TrimPrefix(s.Account, "accounts/") != account {
			continue
		}
		var out []string
		for _, prop := range s.Properties {
			out = append(out, strings.TrimPrefix(prop.Property, "properties/"))
		}
		return out, nil
	}
	return nil, nil
}
