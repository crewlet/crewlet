package adsense

import (
	"context"
	"fmt"
	"log/slog"
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
)

// Product is AdSense as the growth tools reach it.
type Product struct {
	company  func() *config.Company
	client   *googlegrowth.Client
	now      func() time.Time
	accounts *accountCache
}

// New is the product on the engine's active document, the company's consent
// grant, the access log and the engine's growth act lease; hc is nil in
// production.
func New(company func() *config.Company, env func() *config.Resolver, grant googlegrowth.Grant,
	audit googlegrowth.Audit, leases googlegrowth.Leaser, hc *http.Client) *Product {
	return &Product{
		company: company,
		client: &googlegrowth.Client{
			Kind: string(Kind), Name: "AdSense", Company: company, Env: env, Grant: grant, Audit: audit,
			Leases: leases, HTTP: hc, Texts: []string{"displayName", "description", "title"}, Refused: vendorRefusal,
		},
		now:      time.Now,
		accounts: newAccountCache(),
	}
}

// Catalog is the product's catalog.
func (p *Product) Catalog() growth.Catalog {
	c, _ := growth.CatalogOf(string(Kind))
	return c
}

// seat is handle's block, nil where the seat is not on.
func (p *Product) seat(handle string) *config.RoleAdSense {
	c := p.company()
	if c == nil || !c.Integrations.GrowthEnabled(string(Kind)) {
		return nil
	}
	for role := range c.EachRole() {
		if strings.TrimSpace(role.IdentityKey()) != handle {
			continue
		}
		if b := role.Integrations.AdSense; b != nil && config.SeatAccessOn(b.Enabled) {
			return b
		}
		return nil
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

// accountsOf are the publisher accounts handle may reach: its own, within the
// company's narrowing.
func (p *Product) accountsOf(handle string) []string {
	b := p.seat(handle)
	if b == nil {
		return nil
	}
	company := p.company().Integrations.AdSense
	var out []string
	for _, a := range b.Accounts {
		if company.Reaches(a) {
			out = append(out, a)
		}
	}
	return out
}

// refusal is a call refused, in words the agent acts on.
func refusal(kind growth.RefusalKind, said string) error { return growth.Refuse(kind, said) }

// inScope refuses req where the seat is off or the account it acts in is not
// one of the seat's. A call naming no account is the account listing, cut to
// the seat's accounts after.
func (p *Product) inScope(handle string, req googlegrowth.Request) error {
	if p.seat(handle) == nil {
		return refusal(growth.RefusedForbidden, "this agent is off at AdSense. No vendor was reached.")
	}
	if req.Account == "" {
		return nil
	}
	mine := p.accountsOf(handle)
	if !slices.Contains(mine, req.Account) {
		return refusal(growth.RefusedForbidden, "account "+req.Account+" is not one of this agent's AdSense "+
			"accounts ("+strings.Join(mine, ", ")+"). No vendor was reached.")
	}
	return namesItsOwn(req)
}

// namesItsOwn refuses a call whose path or body names a publisher other than
// the account it acts in and the sub-account its path names: an ad client,
// a resource name or a platform group of another account, which the
// company's grant may reach too.
func namesItsOwn(req googlegrowth.Request) error {
	own := []string{digits(req.Account)}
	for _, name := range []string{"accountsId", "childAccountsId"} {
		if sub := req.Params[name]; sub != "" && sub != req.Account && subAccount(req.Op, name) {
			own = append(own, digits(sub))
		}
	}
	var stray string
	check := func(s string) {
		for _, m := range publisherRef().FindAllStringSubmatch(s, -1) {
			if stray == "" && !slices.Contains(own, m[1]) {
				stray = m[0]
			}
		}
	}
	for _, v := range req.Params {
		check(v)
	}
	eachString(req.Body, check)
	if stray != "" {
		return refusal(growth.RefusedForbidden, fmt.Sprintf("the call names %q, which is not account %s's. "+
			"No vendor was reached.", stray, req.Account))
	}
	return nil
}

// subAccount reports whether op's path parameter name is a sub-account of the
// account the call acts in: the Platform API's account under a platform, and
// a platform's child account.
func subAccount(op growth.Operation, name string) bool {
	switch name {
	case "accountsId":
		return strings.HasPrefix(op.Path, platform+"/platforms/")
	case "childAccountsId":
		return true
	}
	return false
}

// owner is the path parameter naming the account op acts in: the platform's
// own publisher ID on the Platform API's v1 surface, the account elsewhere.
func owner(op growth.Operation) string {
	if strings.HasPrefix(op.Path, platform+"/platforms/") {
		return "platformsId"
	}
	return "accountsId"
}

// digits are a publisher ID's 16 digits.
func digits(publisher string) string { return strings.TrimPrefix(publisher, "pub-") }

// publisherRef is any reference that names a publisher: a resource name
// (accounts/pub-…), an ad client (ca-pub-…, partner-pub-…), or the ID.
var publisherRef = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`pub-([0-9]{16})`) })

// eachString calls f with every string v holds, map keys included.
func eachString(v any, f func(string)) {
	switch t := v.(type) {
	case string:
		f(t)
	case map[string]any:
		for k, inner := range t {
			f(k)
			eachString(inner, f)
		}
	case []any:
		for _, inner := range t {
			eachString(inner, f)
		}
	}
}

// vendorRefusal says Google's refusal of req in words the agent acts on; nil
// passes Google's error on (package doc, "Who the engine is at AdSense").
func vendorRefusal(req googlegrowth.Request, api *googleauth.APIError) error {
	switch api.Status {
	case http.StatusUnauthorized:
		return refusal(growth.RefusedUnavailable, "Google no longer accepts the company's AdSense sign-in ("+
			api.Answer()+"). A person signs in with Google again on the AdSense integration's page.")
	case http.StatusForbidden:
		return refusal(growth.RefusedForbidden, forbidden(req, api))
	case http.StatusTooManyRequests:
		return refusal(growth.RefusedUnavailable, "AdSense refused more requests for now ("+api.Answer()+
			"). It allows 100 requests a minute per user and 10,000 a day per project, which resets at midnight "+
			"Pacific time; report rows have a quota of their own, which fewer days or dimensions spare.")
	case http.StatusNotFound, http.StatusBadRequest, http.StatusConflict:
		return refusal(growth.RefusedInvalid, "AdSense answered "+api.Answer()+".")
	}
	return nil
}

// forbidden says which refusal a 403 is: the API off in the client's project,
// the sign-in's scopes, a Google account with no AdSense account, a method
// Google has not enabled for the project, or an account the sign-in does not
// reach.
func forbidden(req googlegrowth.Request, api *googleauth.APIError) string {
	reasons := strings.Join(api.Reasons, " ")
	apiName := "AdSense Management API"
	if platformOnly(req.Op) {
		apiName = "AdSense Platform API"
	}
	switch {
	case strings.Contains(reasons, "SERVICE_DISABLED") || strings.Contains(api.Message, "has not been used in project"):
		return "the " + apiName + " is off in the Google Cloud project of the company's OAuth client (" +
			api.Answer() + "). A person turns it on there; Google turns on the Platform API only for AdSense " +
			"for Platforms projects."
	case strings.Contains(reasons, "ACCESS_TOKEN_SCOPE_INSUFFICIENT"):
		return "the company's AdSense sign-in allows reading only, and " + req.Op.ID + " changes settings (" +
			api.Answer() + "). A person signs in with Google again, once an agent is at operate, to allow changes."
	case strings.Contains(api.Message, "does not have an AdSense account"):
		return "the Google account that signed in has no AdSense account (" + api.Answer() + "). A person " +
			"who manages the AdSense account signs in with Google again."
	case restricted(req.Op):
		return "Google has not enabled this project for " + req.Op.ID + " (" + api.Answer() + "), which it " +
			"enables for AdSense for Platforms projects; the account's AdSense account manager asks for it."
	}
	return fmt.Sprintf("AdSense refused %s (%s). The Google account that signed in needs Administrator or "+
		"Standard user on account %s; a person adds it in AdSense, Account, Access and authorization, User "+
		"management.", req.Op.ID, api.Answer(), req.AccountOrTarget())
}

// Denied logs that a person denied the held call, or that it lapsed; the
// verdict itself is the held call's record.
func (p *Product) Denied(ctx context.Context, call growth.Call) error {
	slog.InfoContext(ctx, "growth_call_denied", "kind", string(Kind), "tool", call.Tool, "seat", call.Handle,
		"held_call", call.HeldCall)
	return nil
}
