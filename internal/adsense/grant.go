package adsense

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/words"
)

// GrantScopes are the scopes the company's sign-in asks for now: the read
// scopes while every seat reads, and the write scope too once any seat's level
// may change settings, so a company that only reads never grants changes.
func GrantScopes(c *config.Company) []string {
	if writes(c) {
		return OperateScopes()
	}
	return ReadScopes()
}

// writes reports whether any seat of c is on AdSense at a level that changes.
func writes(c *config.Company) bool {
	if c == nil {
		return false
	}
	for role := range c.EachRole() {
		if b := role.Integrations.AdSense; b != nil && config.SeatAccessOn(b.Enabled) &&
			b.LevelOrDefault().Allows(growth.ClassOperate) {
			return true
		}
	}
	return false
}

// named is every publisher account the company names: its narrowing, and
// every seat's, each once and in order.
func named(c *config.Company) []string {
	var out []string
	add := func(ids []string) {
		for _, id := range ids {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	add(c.Integrations.AdSense.Accounts)
	for role := range c.EachRole() {
		if b := role.Integrations.AdSense; b != nil && config.SeatAccessOn(b.Enabled) {
			add(b.Accounts)
		}
	}
	slices.Sort(out)
	return out
}

// Check is what the pass reports of a held grant that granted scopes: a
// sign-in made for reading while a seat may change settings, and each named
// account the grant does not reach. It answers the accounts it reached, which
// the grant's record keeps.
func (p *Product) Check(ctx context.Context, granted []string) ([]integration.Finding, []string, error) {
	c := p.company()
	if c == nil || !c.Integrations.GrowthEnabled(string(Kind)) {
		return nil, nil, integration.ErrNotConfigured
	}
	var out []integration.Finding
	if writes(c) && !slices.Contains(granted, scopeManage) {
		out = append(out, integration.Finding{Kind: integration.FindingGrantShort, Subject: "adsense",
			Title:  "Sign in again to allow changes",
			Detail: "An agent may change AdSense settings, and the sign-in allows reading only.",
			Remedy: "Sign in with Google again on the AdSense page and allow managing your AdSense data"})
	}
	op, _ := p.Catalog().Operation(accountGetOp)
	var reached, missed []string
	reasons := map[string]string{}
	for _, id := range named(c) {
		req, err := newRequest(op, map[string]string{"accountsId": id}, nil, nil)
		if err != nil {
			return nil, nil, err
		}
		raw, err := p.client.Send(ctx, "", req)
		var a account
		switch {
		case err == nil && json.Unmarshal(raw, &a) == nil && a.Name == "accounts/"+id:
			reached = append(reached, id)
			continue
		case err == nil:
			reasons[id] = "Google answered for another account"
		case transient(err):
			// A FAULT THAT CLEARS is the pass's wait, never a finding.
			return nil, nil, err
		default:
			reasons[id] = err.Error()
		}
		missed = append(missed, id)
	}
	if len(missed) > 0 {
		var detail []string
		for _, id := range missed {
			detail = append(detail, id+": "+reasons[id])
		}
		out = append(out, integration.Finding{Kind: integration.FindingGrantShort, Subject: "accounts",
			Subjects: missed,
			Title:    "The sign-in cannot reach " + words.And(saids(missed)),
			Detail:   strings.Join(detail, "; "),
			Remedy: "Sign in again as a Google account that can see them in AdSense, or take them off the " +
				"agents' accounts"})
	}
	return out, reached, nil
}

// transient reports a failure that clears by itself: Google's quota or
// outage, a timeout, or the engine's own fault.
func transient(err error) bool {
	ce, ok := errors.AsType[*googlegrowth.CallError](err)
	if !ok {
		return true
	}
	return slices.Contains([]growth.Failure{growth.FailureRateLimited, growth.FailureVendorUnavailable,
		growth.FailureTimedOut, growth.FailureEngineError}, ce.Failure)
}

// saids are ids as words a sentence names.
func saids(ids []string) []words.Said {
	out := make([]words.Said, len(ids))
	for i, id := range ids {
		out[i] = words.Said(id)
	}
	return out
}
