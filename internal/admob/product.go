package admob

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

// Product is AdMob as the growth tools reach it.
type Product struct {
	company  func() *config.Company
	client   *googlegrowth.Client
	now      func() time.Time
	accounts *accountCache
}

// New is the product on the engine's active document, the company's consent
// grant and the access log; hc is nil in production.
func New(company func() *config.Company, env func() *config.Resolver, grant googlegrowth.Grant,
	audit googlegrowth.Audit, hc *http.Client) *Product {
	return &Product{
		company: company,
		client: &googlegrowth.Client{
			Kind: string(Kind), Name: "AdMob", Company: company, Env: env, Grant: grant, Audit: audit, HTTP: hc,
			Secrets: []string{secretField}, Refused: vendorRefusal,
		},
		now:      time.Now,
		accounts: newAccountCache(),
	}
}

// secretField holds a third-party network's identifiers for an ad unit
// mapping, some of them keys: withheld from every answer (package doc).
const secretField = "adUnitConfigurations"

// Catalog is the product's catalog.
func (p *Product) Catalog() growth.Catalog {
	c, _ := growth.CatalogOf(string(Kind))
	return c
}

// seat is handle's block, nil where the seat is not on.
func (p *Product) seat(handle string) *config.RoleAdMob {
	c := p.company()
	if c == nil || !c.Integrations.GrowthEnabled(string(Kind)) {
		return nil
	}
	for role := range c.EachRole() {
		if strings.TrimSpace(role.IdentityKey()) != handle {
			continue
		}
		if b := role.Integrations.AdMob; b != nil && config.SeatAccessOn(b.Enabled) {
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
	company := p.company().Integrations.AdMob
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

// inScope refuses req where the seat is off or the account it names is not
// one of the seat's. A call naming no account is a listing, cut to the
// seat's accounts after.
func (p *Product) inScope(handle string, req googlegrowth.Request) error {
	if p.seat(handle) == nil {
		return refusal(growth.RefusedForbidden, "this agent is off at AdMob. No vendor was reached.")
	}
	if req.Account == "" {
		return nil
	}
	mine := p.accountsOf(handle)
	if !slices.Contains(mine, req.Account) {
		return refusal(growth.RefusedForbidden, "account "+req.Account+" is not one of this agent's AdMob "+
			"accounts ("+strings.Join(mine, ", ")+"). No vendor was reached.")
	}
	return bodyInScope(req)
}

// bodyInScope refuses a body naming another account than the one its path
// names: a batch item's parent, and any account or app, ad unit or mapping ID
// it holds (each carries its publisher's digits), since the company's grant
// may reach other accounts too.
func bodyInScope(req googlegrowth.Request) error {
	items, _ := req.Body["requests"].([]any)
	for i, item := range items {
		m, _ := item.(map[string]any)
		parent, _ := m["parent"].(string)
		if !strings.HasPrefix(parent, "accounts/"+req.Account+"/adUnits/") {
			return refusal(growth.RefusedForbidden, fmt.Sprintf("requests[%d].parent %q is not an ad unit of "+
				"account %s, which the call names. No vendor was reached.", i, parent, req.Account))
		}
	}
	digits := strings.TrimPrefix(req.Account, "pub-")
	var stray string
	eachString(req.Body, func(s string) {
		for _, m := range publisherRef().FindAllStringSubmatch(s, -1) {
			if stray == "" && m[1] != digits {
				stray = m[0]
			}
		}
	})
	if stray != "" {
		return refusal(growth.RefusedForbidden, fmt.Sprintf("the body names %q, which is not account %s's. "+
			"No vendor was reached.", stray, req.Account))
	}
	return nil
}

// publisherRef is a reference that names its publisher: a resource name
// (accounts/pub-…) or an app, ad unit or mapping ID (ca-app-pub-…).
var publisherRef = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?:accounts/pub-|ca-app-pub-)([0-9]+)`)
})

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

// newRequest is op's request, acting in the publisher account its path names.
func newRequest(op growth.Operation, path, query map[string]string, body map[string]any) (googlegrowth.Request,
	error) {
	if a, named := path["accountsId"]; named && !publisherID(a) {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%q is not a publisher ID: "+
			"pub- and 16 digits. No vendor was reached.", a))
	}
	for name := range path {
		// A PARAMETER THE PATH DOES NOT NAME would address nothing, yet be
		// read as the call's account or target.
		if !strings.Contains(op.Path, "{"+name+"}") {
			return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%s takes no path "+
				"parameter %q. No vendor was reached.", op.ID, name))
		}
	}
	if key := snakeKey(body); key != "" {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("the body's field %q is "+
			"spelled as the API's proto name; give it as its JSON name, %q, which the engine classes the "+
			"change by. No vendor was reached.", key, camel(key)))
	}
	req, err := googlegrowth.NewRequest(op, path, query, body)
	if err != nil {
		return req, err
	}
	if op.Method == "PATCH" {
		for _, p := range maskPaths(query["updateMask"]) {
			if _, ok := parseMask(p); !ok {
				return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("updateMask path %q is "+
					"not one AdMob takes: snake_case fields, a map entry as name[\"key\"], joined by dots. "+
					"No vendor was reached.", p))
			}
		}
	}
	req.Account = path["accountsId"]
	req.Diff = diffOf(op, body, req.Query.Get("updateMask"))
	return req, nil
}

// vendorRefusal says Google's refusal of req in words the agent acts on; nil
// passes Google's error on (package doc, "Who the engine is at AdMob").
func vendorRefusal(req googlegrowth.Request, api *googleauth.APIError) error {
	switch api.Status {
	case http.StatusUnauthorized:
		return refusal(growth.RefusedUnavailable, "Google no longer accepts the company's AdMob sign-in ("+
			api.Answer()+"). A person signs in with Google again on the AdMob integration's page.")
	case http.StatusForbidden:
		return refusal(growth.RefusedForbidden, forbidden(req, api))
	case http.StatusTooManyRequests:
		return refusal(growth.RefusedUnavailable, "AdMob refused more requests for now ("+api.Answer()+
			"). Per-minute quotas free up within the minute, and the inventory daily quota at midnight Pacific time.")
	case http.StatusNotFound, http.StatusBadRequest, http.StatusConflict:
		return refusal(growth.RefusedInvalid, "AdMob answered "+api.Answer()+".")
	}
	return nil
}

// forbidden says which of the four refusals a 403 is: the API off in the
// client's project, the sign-in's scopes, a limited-access method not yet
// allowed, or the signing person's role.
func forbidden(req googlegrowth.Request, api *googleauth.APIError) string {
	reasons := strings.Join(api.Reasons, " ")
	switch {
	case strings.Contains(reasons, "SERVICE_DISABLED") || strings.Contains(api.Message, "has not been used in project"):
		return "the AdMob API is off in the Google Cloud project of the company's OAuth client (" + api.Answer() +
			"). A person turns on the AdMob API there."
	case strings.Contains(reasons, "ACCESS_TOKEN_SCOPE_INSUFFICIENT"):
		return "the company's AdMob sign-in allows reading only, and " + req.Op.ID + " changes settings (" +
			api.Answer() + "). A person signs in with Google again, once an agent is at operate, to allow changes."
	case limitedAccess(req.Op) && !strings.Contains(api.Message, "effective user"):
		return "Google has not allowed this account " + req.Op.ID + ", a limited-access method (" + api.Answer() +
			"). The account's AdMob account manager asks Google to allow it."
	}
	return fmt.Sprintf("AdMob refused %s (%s). The Google account that signed in needs one of these on the "+
		"account: %s; a custom role limited to some apps reaches only those. A person changes it in AdMob, "+
		"Settings, Users.", req.Op.ID, api.Answer(), roleNames(req.Op.Roles))
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

// Denied logs that a person denied the held call, or that it lapsed; the
// verdict itself is the held call's record.
func (p *Product) Denied(ctx context.Context, call growth.Call) error {
	slog.InfoContext(ctx, "growth_call_denied", "kind", string(Kind), "tool", call.Tool, "seat", call.Handle,
		"held_call", call.HeldCall)
	return nil
}
