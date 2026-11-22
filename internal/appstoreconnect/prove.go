package appstoreconnect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// Prove reads, with each connected key, a request every role answers, and
// with the Admin slot's key one only an Admin's key answers: a key Apple
// refuses, or an Admin slot holding another role's key, is a finding naming
// its slot. A key that does not parse is one too.
func (p *Product) Prove(ctx context.Context) ([]integration.Finding, error) {
	block := p.deps.Company().Integrations.AppStoreConnect
	if !block.GrowthEnabled() {
		return nil, integration.ErrNotConfigured
	}
	teams, err := p.teamsNow()
	if err != nil {
		//nolint:nilerr // A key that does not read is the finding this pass raises, not a failed check.
		return []integration.Finding{{Kind: integration.FindingCredentialMissing,
			Subject: "integrations." + Kind + ".connections", Detail: err.Error()}}, nil
	}
	var findings []integration.Finding
	for _, t := range teams {
		for _, slot := range t.slots {
			if slot == adminSlot {
				// READ AS A KEY, NEVER CALLED WITH: the Admin key makes approved
				// account changes alone, so a pass proves it parses (teamsNow).
				continue
			}
			subject := "integrations." + Kind + ".connections." + t.id + ".keys." + slot
			path := "/v1/apps"
			_, err := t.client.Do(ctx, slot, Request{Method: "GET", Path: path, Query: url.Values{"limit": {"1"}},
				Read: true, Reserved: true})
			var apiErr *APIError
			switch {
			case err == nil:
			case errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden):
				detail := fmt.Sprintf("Apple refused the %s key: it was revoked, its private key does not match, "+
					"or its role is not the slot's; create a new key with the slot's role and paste it", slot)
				findings = append(findings, integration.Finding{Kind: integration.FindingCredentialRejected,
					Subject: subject, Detail: detail})
			default:
				return findings, fmt.Errorf("appstoreconnect: prove the %s connection's %s key: %w", t.id, slot, err)
			}
		}
	}
	return findings, nil
}

// Probe is the connect form's check: App Store Connect's keys are proven by
// the pass once saved, since a key is proven only with its team's issuer.
func Probe() setup.Probe {
	return func(context.Context, setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.ErrNotCheckable
	}
}
