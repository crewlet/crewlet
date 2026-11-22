package appleads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
)

// Prove signs in as each connection's API user and reads who it is and the ad
// accounts it reaches: a key Apple refuses, or an organization other than the
// one the document names, is a finding naming the connection, as is an ad
// account the company or a seat names that no connection reaches. A key that
// does not parse is one too.
func (p *Product) Prove(ctx context.Context) ([]integration.Finding, error) {
	company := p.deps.Company()
	block := company.Integrations.AppleAds
	if !block.GrowthEnabled() {
		return nil, integration.ErrNotConfigured
	}
	conns, err := p.connsNow()
	if err != nil {
		//nolint:nilerr // A key that does not read is the finding this pass raises, not a failed check.
		return []integration.Finding{{Kind: integration.FindingCredentialMissing,
			Subject: "integrations." + Kind + ".connections", Detail: err.Error()}}, nil
	}
	var findings []integration.Finding
	reached := map[string]bool{}
	for _, c := range conns {
		subject := "integrations." + Kind + ".connections." + c.id
		resp, err := c.client.Do(ctx, Request{Method: http.MethodGet, Path: "/v1/me", Read: true, Reserved: true})
		var apiErr *APIError
		switch {
		case errors.Is(err, ErrCredentials) || (errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized ||
			apiErr.Status == http.StatusForbidden)):
			findings = append(findings, integration.Finding{Kind: integration.FindingCredentialRejected, Subject: subject,
				Detail: fmt.Sprintf("Apple Ads refused the %s connection's key: its public key was removed or replaced, "+
					"or the API user lost its role. Make a new key, upload its public half and paste it", c.id)})
			continue
		case err != nil:
			return findings, fmt.Errorf("appleads: prove the %s connection: %w", c.id, err)
		}
		var me struct {
			Result struct {
				OrgID any `json:"orgId"`
			} `json:"result"`
		}
		if derr := decode(resp.Body, &me); derr != nil {
			return findings, fmt.Errorf("appleads: read the %s connection's user: %w", c.id, derr)
		}
		if org := idText(me.Result.OrgID); c.org != "" && org != c.org {
			findings = append(findings, integration.Finding{Kind: integration.FindingCredentialRejected, Subject: subject,
				Detail: fmt.Sprintf("the %s connection's API user answers organization %s, not %s: connect a user of %s, "+
					"or correct org_id", c.id, org, c.org, c.org)})
			continue
		}
		c.mu.Lock()
		c.acls = nil
		c.mu.Unlock()
		accounts, err := p.aclsOf(ctx, c)
		if err != nil {
			return findings, fmt.Errorf("appleads: read the %s connection's ad accounts: %w", c.id, err)
		}
		for _, a := range accounts {
			reached[a.ID] = true
		}
	}
	if missing := unreached(scopedAccounts(p), reached); len(missing) > 0 && len(findings) == 0 {
		findings = append(findings, integration.Finding{Kind: integration.FindingGrantShort,
			Subject: "integrations." + Kind + ".ad_accounts",
			Detail: fmt.Sprintf("no connected API user reaches %s %s: give one access in Apple Ads, or remove %s from "+
				"the company and its agents", noun(len(missing)), words.And(missing), pronoun(len(missing)))})
	}
	return findings, nil
}

func noun(n int) string {
	if n == 1 {
		return "ad account"
	}
	return "ad accounts"
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// scopedAccounts are the ad accounts the company and its seats name.
func scopedAccounts(p *Product) []string {
	company := p.deps.Company()
	out := slices.Clone(company.Integrations.AppleAds.AdAccounts)
	for role := range company.EachRole() {
		if b := role.Integrations.AppleAds; b != nil && b.Enabled != nil && *b.Enabled {
			for _, a := range b.AdAccounts {
				if !slices.Contains(out, a) {
					out = append(out, a)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

func unreached(scoped []string, reached map[string]bool) []string {
	var out []string
	for _, a := range scoped {
		if !reached[a] {
			out = append(out, a)
		}
	}
	return out
}

// Probe is the connect form's check: an API user's key is proven by the pass
// once saved, since Apple proves a key only by signing in with all its IDs.
func Probe() setup.Probe {
	return func(context.Context, setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.ErrNotCheckable
	}
}
