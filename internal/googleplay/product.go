package googleplay

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

// Leases hold an app's write lease across the fleet (the engine's growth act
// lease): one Play commit at a time per app, since a commit invalidates every
// other open edit of it. Hold's error wraps [growth.ErrActHeld] where another
// holder has the lease; any other is the lease store's failure or ctx's end,
// and nothing was taken.
type Leases interface {
	Hold(ctx context.Context, resource string) (func(), error)
}

// Reader mints a token of the company's reader, the one identity that reads
// the monthly reports bucket (the engine's GoogleReaderToken).
type Reader interface {
	GoogleReaderToken(ctx context.Context, scopes []string, hc *http.Client) (string, error)
}

// Media reads a file an agent names from an artifact, for an upload.
type Media interface {
	// Open reads ref (artifact:<id>/<path>) as handle's turn may read it,
	// at most limit bytes.
	Open(ctx context.Context, handle, ref string, limit int64) (File, error)
}

// File is one file read for an upload: its bytes, and the digest an approval
// binds to.
type File struct {
	Ref, Name, ContentType string
	Data                   []byte
	SHA256                 string
}

// Audit is the access log's record of each step of an act, its answer too.
type Audit interface {
	googlegrowth.Audit
}

// Deps are what the product acts through; Reader and Media may be nil, which
// refuses the reports bucket and uploads.
type Deps struct {
	Company func() *config.Company
	Env     func() *config.Resolver
	Tokens  googlegrowth.Tokens
	Audit   Audit
	Leases  Leases
	Reader  Reader
	Media   Media
	// HTTP is the transport Google is reached through; nil in production.
	HTTP *http.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Product is Google Play as the growth tools reach it.
type Product struct {
	deps   Deps
	client *googlegrowth.Client
}

// New is the product on the engine's dependencies.
func New(d Deps) *Product {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Product{deps: d, client: &googlegrowth.Client{
		Kind: string(Kind), Name: "Google Play", Company: d.Company, Env: d.Env, Tokens: d.Tokens,
		Audit: d.Audit, HTTP: d.HTTP, Refused: vendorRefusal, Signer: growth.SignerOwn,
	}}
}

// Catalog is the product's catalog.
func (p *Product) Catalog() growth.Catalog {
	c, _ := growth.CatalogOf(string(Kind))
	return c
}

// company is the running document's block, nil where it holds none.
func (p *Product) company() *config.GooglePlay {
	if c := p.deps.Company(); c != nil {
		return c.Integrations.GooglePlay
	}
	return nil
}

// seat is handle's block, nil where the seat is not on.
func (p *Product) seat(handle string) *config.RoleGooglePlay {
	c := p.deps.Company()
	if c == nil || !c.Integrations.GrowthEnabled(string(Kind)) {
		return nil
	}
	for role := range c.EachRole() {
		if strings.TrimSpace(role.IdentityKey()) == handle {
			b := role.Integrations.GooglePlay
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
// 403 names the Play Console permissions that may make the call; nil passes
// Google's error on.
func vendorRefusal(req googlegrowth.Request, api *googleauth.APIError) error {
	switch api.Status {
	case http.StatusForbidden:
		return refusal(growth.RefusedForbidden, fmt.Sprintf("Google Play refused %s (%s). This agent's Google "+
			"identity needs one of these permissions on the app: %s. A person changes it in Play Console, Users "+
			"and permissions.", req.Op.ID, api.Answer(), roleNames(req.Op.Roles)))
	case http.StatusTooManyRequests:
		return refusal(growth.RefusedUnavailable, "Google Play refused more requests for now ("+api.Answer()+
			"). Its limits: 3,000 a minute per bucket, and per app 200 review reads an hour and 2,000 replies a day.")
	case http.StatusNotFound:
		return refusal(growth.RefusedNotFound, "Google Play answered "+api.Answer()+". An app Google Play's API "+
			"cannot find has no upload yet, or this agent's identity is not invited to it.")
	case http.StatusBadRequest, http.StatusConflict, http.StatusPreconditionFailed:
		return refusal(growth.RefusedInvalid, "Google Play answered "+api.Answer()+".")
	}
	return nil
}

// roleNames are the names of permission IDs, for a person.
func roleNames(ids []string) string {
	if len(ids) == 0 {
		return "View app information, which every permission includes"
	}
	var out []string
	for _, r := range roles() {
		if slices.Contains(ids, r.ID) {
			out = append(out, r.Name)
		}
	}
	return strings.Join(out, ", ")
}

// inScope refuses req where it names an app, developer account or bucket the
// seat may not reach. An account-wide call (users and grants, a financial
// report) needs a seat that reaches every app the company names.
func (p *Product) inScope(handle string, req googlegrowth.Request) error {
	b := p.seat(handle)
	if b == nil {
		return refusal(growth.RefusedForbidden, "this agent is off at Google Play. No vendor was reached.")
	}
	company := p.company()
	switch account := req.Account; {
	case strings.HasPrefix(account, "developers/"):
		id := strings.TrimPrefix(account, "developers/")
		if !slices.Contains(company.DeveloperAccounts, id) {
			return refusal(growth.RefusedForbidden, "developer account "+id+" is not in "+
				"integrations.google_play.developer_accounts. No vendor was reached.")
		}
		return p.reachesAll(b, "a developer account's users and permissions reach every app in it")
	case strings.HasPrefix(account, "b/"):
		bucket := strings.TrimPrefix(account, "b/")
		if !slices.Contains(company.ReportsBuckets, bucket) {
			return refusal(growth.RefusedForbidden, "bucket "+bucket+" is not in "+
				"integrations.google_play.reports_buckets. No vendor was reached.")
		}
		if object := req.Params["object"]; object != "" {
			return p.objectInScope(b, object)
		}
	case account != "":
		if !slices.Contains(b.Apps, account) || !company.Reaches(account) {
			return refusal(growth.RefusedForbidden, "app "+account+" is not one of this agent's apps ("+
				words.Or(b.Apps)+"). No vendor was reached.")
		}
	}
	return nil
}

// reachesAll refuses a seat that does not reach every app the company names,
// or a company that names none, for an account-wide call: why says what it
// reaches.
func (p *Product) reachesAll(b *config.RoleGooglePlay, why string) error {
	company := p.company()
	if len(company.Apps) == 0 {
		return refusal(growth.RefusedForbidden, why+", so the company names its apps first "+
			"(integrations.google_play.apps). No vendor was reached.")
	}
	for _, app := range company.Apps {
		if !slices.Contains(b.Apps, app) {
			return refusal(growth.RefusedForbidden, why+", and app "+app+" is not one of this agent's apps. "+
				"No vendor was reached.")
		}
	}
	return nil
}
