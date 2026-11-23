package metaads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
)

// Prove proves each token and the admin system user's, renews one near its
// end, checks Require App Secret, and reads the system user's reach and the
// app's tier; a pass turned off forgets every renewal.
func (p *Product) Prove(ctx context.Context) ([]integration.Finding, error) {
	company := p.deps.Company()
	block := company.Integrations.MetaAds
	if !block.GrowthEnabled() {
		// ONCE PER TURNING OFF: every pass runs for every company, and one
		// with no Meta block has nothing to forget after the first.
		if !p.forgotten.Load() {
			if err := p.Forget(ctx); err != nil {
				return nil, err
			}
			p.forgotten.Store(true)
		}
		return nil, integration.ErrNotConfigured
	}
	p.forgotten.Store(false)
	subject := func(field string) string { return "integrations." + Kind + "." + field }
	c, err := p.connNow()
	if err != nil {
		//nolint:nilerr // A credential that does not resolve is the finding this pass raises.
		return []integration.Finding{{Kind: integration.FindingCredentialMissing, Subject: subject("read_token"),
			Detail: err.Error()}}, nil
	}
	var findings []integration.Finding
	if p.deps.Renewals != nil {
		for _, s := range slots {
			if c.tokens[s.name] == nil {
				// A CLEARED TOKEN takes its renewal with it.
				if derr := p.deps.Renewals.Delete(ctx, s.name); derr != nil {
					return nil, fmt.Errorf("metaads: drop the cleared %s token's renewal: %w", s.name, derr)
				}
			}
		}
	}
	users := map[string]string{}
	for _, s := range slots {
		t := c.tokens[s.name]
		if t == nil {
			continue
		}
		t.mu.Lock()
		t.checked = time.Time{}
		t.mu.Unlock()
		cerr := p.check(ctx, c, t)
		switch {
		case errors.Is(cerr, ErrTokenRefused):
			findings = append(findings, integration.Finding{Kind: integration.FindingCredentialRejected,
				Subject: subject(s.field), Detail: cerr.Error()})
			continue
		case cerr != nil:
			return findings, fmt.Errorf("metaads: prove the %s token: %w", s.name, cerr)
		}
		users[s.name] = t.user
		r, rerr := p.renew(ctx, c, t)
		if rerr != nil {
			return findings, rerr
		}
		if r.failed != "" && !r.expires.IsZero() {
			findings = append(findings, integration.Finding{Kind: integration.FindingCredentialExpiring,
				Subject: subject(s.field), ExpiresAt: r.expires,
				Detail: fmt.Sprintf("the %s token expires on %s, and Crewlet could not renew it: %s. Generate a new token "+
					"for the system user and paste it", s.name, r.expires.Format("2 January 2006"), r.failed)})
		}
	}
	if u := users[slotRead]; u != "" {
		for name, other := range users {
			if other != u {
				findings = append(findings, integration.Finding{Kind: integration.FindingCredentialRejected,
					Subject: subject(name + "_token"), Detail: fmt.Sprintf("the %s token is another user's than the read "+
						"token's: every token is one system user's", name)})
			}
		}
	}
	if len(findings) > 0 && slices.ContainsFunc(findings, func(f integration.Finding) bool {
		return f.Kind == integration.FindingCredentialRejected && f.Subject == subject("read_token")
	}) {
		return findings, nil
	}
	if admin := strings.TrimSpace(p.deps.Resolve(block.AdminSystemUserToken)); admin != "" {
		g := &graph{client: c.client, appID: c.appID, business: strings.TrimSpace(p.deps.Resolve(block.BusinessID)),
			admin: admin}
		f, aerr := g.proveAdmin(ctx, p.deps.Now())
		if aerr != nil {
			return findings, fmt.Errorf("metaads: prove the admin system user token: %w", aerr)
		}
		if f != nil {
			findings = append(findings, *f)
		}
	}
	f, perr := p.proofRequired(ctx, c)
	if perr != nil {
		return findings, perr
	}
	if f != nil {
		findings = append(findings, *f)
	}
	c.mu.Lock()
	c.accounts = nil
	c.mu.Unlock()
	reach, err := p.accountsOf(ctx, c)
	if err != nil {
		return findings, fmt.Errorf("metaads: read the system user's ad accounts: %w", err)
	}
	findings = append(findings, p.reachFindings(reach, c)...)
	return findings, nil
}

// proofRequired reads once without appsecret_proof, which Meta refuses where
// Require App Secret is on: a read it answers is an advisory, since a leaked
// token then works without the app secret.
func (p *Product) proofRequired(ctx context.Context, c *conn) (*integration.Finding, error) {
	tok := c.tokens[slotRead]
	_, err := c.client.Do(ctx, tok.valueNow(), Request{Method: http.MethodGet, Path: "/me",
		Params: url.Values{"fields": {"id"}}, Read: true, Reserved: true, NoProof: true})
	var apiErr *APIError
	switch {
	case err == nil:
		return &integration.Finding{Kind: integration.FindingGrantExcess, Subject: "integrations." + Kind + ".app_secret",
			Title:  "Require App Secret is off",
			Detail: "Meta answers this app's tokens without the app secret, so a leaked token works on its own",
			Remedy: "Turn on Require App Secret in your Meta app's Advanced settings."}, nil
	case errors.As(err, &apiErr):
		return nil, nil
	}
	return nil, fmt.Errorf("metaads: check Require App Secret: %w", err)
}

// reachFindings are what the system user's reach says: an ad account the
// company or a seat names that it does not reach, an account that cannot run
// ads, Limited access, and an operate seat with no operate token.
func (p *Product) reachFindings(reach []adAccount, c *conn) []integration.Finding {
	company := p.deps.Company()
	var findings []integration.Finding
	var missing, stopped []string
	for _, id := range scopedAccounts(p) {
		i := slices.IndexFunc(reach, func(a adAccount) bool { return a.ID == id })
		switch {
		case i < 0:
			missing = append(missing, id)
		case !reach[i].serving():
			stopped = append(stopped, id+" ("+reach[i].statusText()+")")
		}
	}
	if len(missing) > 0 {
		findings = append(findings, integration.Finding{Kind: integration.FindingGrantShort,
			Subject: "integrations." + Kind + ".ad_accounts",
			Detail: fmt.Sprintf("the system user reaches %d of the %d ad accounts named here: assign it %s in Business "+
				"Settings, or remove them", len(scopedAccounts(p))-len(missing), len(scopedAccounts(p)), words.And(missing))})
	}
	if len(stopped) > 0 {
		findings = append(findings, integration.Finding{Kind: integration.FindingLevelUnavailable,
			Subject: "integrations." + Kind + ".ad_accounts", Title: "An ad account cannot run ads",
			Detail: fmt.Sprintf("%s cannot run ads; agents can still read its history", words.And(stopped)),
			Remedy: "Fix it in Meta's Business Support Home."})
	}
	if Limited(c.client.Tier()) {
		findings = append(findings, integration.Finding{Kind: integration.FindingLevelUnavailable,
			Subject: "integrations." + Kind + ".app_id", Title: "Meta has this app on Limited access",
			Detail: "Limited access allows about 300 calls an hour per ad account for changes (plus 40 per active ad) " +
				"and 600 for insights (plus 400 per active ad)",
			Remedy: "Meta moves the app to Full access after 500 successful calls in 15 days with an error rate under " +
				"15%; request it on your app's Marketing API page."})
	}
	operate := false
	for role := range company.EachRole() {
		if b := role.Integrations.MetaAds; b.GrowthSeatLevel() != "" && growth.LevelOf(b.Level) != growth.LevelRead {
			operate = true
		}
	}
	if t := c.tokens[slotOperate]; operate && (t == nil || t.refusal() != nil) {
		findings = append(findings, integration.Finding{Kind: integration.FindingGrantShort,
			Subject: "integrations." + Kind + ".operate_token",
			Detail:  "an agent is set to operate, but no operate token is connected, so it only reads",
			Remedy:  "Paste the system user's token with ads_management as the operate token."})
	}
	return findings
}

// scopedAccounts are the ad accounts the company and its seats name.
func scopedAccounts(p *Product) []string {
	company := p.deps.Company()
	out := company.Integrations.MetaAds.AdAccountIDs()
	for role := range company.EachRole() {
		if b := role.Integrations.MetaAds; b.GrowthSeatLevel() != "" {
			for _, a := range b.AdAccountIDs() {
				if !slices.Contains(out, a) {
					out = append(out, a)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// Probe is the connect form's check: the tokens are proven by the pass once
// saved, since a token is proven only with the app's secret beside it.
func Probe() setup.Probe {
	return func(context.Context, setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.ErrNotCheckable
	}
}
