package jira_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jira"
)

func fields(t *testing.T, in *config.Jira) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, r := range jira.Requirements(in, func(string) (string, bool) { return "", false }) {
		out[r.Field] = true
	}
	return out
}

// A CLOUD SITE SIGNS ITS DELIVERIES TOO.
//
// This asserted the opposite for one commit, on the reasoning that Atlassian
// serves the webhook API to Connect and OAuth apps only. It does — on
// /rest/api/3/webhook, the DYNAMIC API. Webhook administration lives at
// /rest/webhooks/1.0/webhook on both deployments and takes an API token,
// which is the path this engine has always used and hooks.go has always
// documented. The signing secret is a real requirement on Cloud.
func TestACloudSiteIsAskedForItsSigningSecret(t *testing.T) {
	t.Parallel()
	got := fields(t, &config.Jira{URL: "https://acme.atlassian.net"})
	for _, want := range []string{"url", "email", "token", "webhook_secret"} {
		if !got[want] {
			t.Errorf("a Cloud site was not asked for %s", want)
		}
	}
}

// AND A DATA CENTER INSTANCE IS NOT ASKED FOR THE GATEWAY'S. A cloud id and
// a link address mean nothing off Atlassian's own gateway.
func TestADataCenterInstanceIsNotAskedForCloudFields(t *testing.T) {
	t.Parallel()
	got := fields(t, &config.Jira{URL: "https://jira.acme.example"})
	for _, gone := range []string{"cloud_id", "site_url"} {
		if got[gone] {
			t.Errorf("a Data Center instance was asked for %s", gone)
		}
	}
	if !got["webhook_secret"] {
		t.Error("a Data Center instance was not asked for its signing secret")
	}
}

// A VALUE THE DOCUMENT HOLDS SURVIVES THE DERIVATION.
//
// The deployment is a guess from an address and the document is a fact:
// hiding a setting a company has written down would make the form disagree
// with the configuration, and Save would then clear it.
func TestAWrittenFieldIsNeverHidden(t *testing.T) {
	t.Parallel()
	got := fields(t, &config.Jira{
		URL:     "https://jira.acme.example",
		CloudID: "abc-123",
	})
	if !got["cloud_id"] {
		t.Error("a cloud id the document holds was hidden from the form")
	}
}

// AN EMPTY ADDRESS IS NOT A DATA CENTER INSTANCE.
//
// DeploymentOf answers DataCenter for a blank string, which is the right
// default for a real address it cannot place and the wrong answer for no
// address at all: that is a company mid-connect. Dropping the gateway fields
// there left nothing for the discovered cloud id to be written into, so a
// connect that left the site blank was refused by the config for naming no
// instance — the exact state the discovery exists to fill.
func TestAnUnaddressedBlockKeepsSomewhereToRecordTheSite(t *testing.T) {
	t.Parallel()
	got := fields(t, &config.Jira{Email: "${E}", Token: "${T}"})
	for _, want := range []string{"cloud_id", "site_url"} {
		if !got[want] {
			t.Errorf("a block with no address offers no %s to record one in", want)
		}
	}
}

// THE ACCOUNT EMAIL IS REQUIRED UNTIL DATA CENTER IS ESTABLISHED.
//
// Cloud authenticates an API token as Basic base64(email:token) and refuses
// it as a bearer — measured against a live site: 403 without the address,
// 200 with it — so without the email the webhook this integration exists to
// register is never created. Data Center takes the token as a bearer and
// wants no address at all.
//
// The unknown case is the one that was wrong: with no site typed yet,
// DeploymentOf answers DataCenter for the empty string, so the one field a
// fresh Cloud connect cannot do without was marked optional.
func TestTheAccountEmailIsRequiredUntilDataCenterIsKnown(t *testing.T) {
	t.Parallel()
	required := func(in *config.Jira) bool {
		for _, r := range jira.Requirements(in, func(string) (string, bool) { return "", false }) {
			if r.Field == "email" {
				return r.Required
			}
		}
		t.Fatal("no email requirement")
		return false
	}
	if !required(&config.Jira{Token: "${T}"}) {
		t.Error("a company that has named no site yet is not asked for the email")
	}
	if !required(&config.Jira{URL: "https://acme.atlassian.net", Token: "${T}"}) {
		t.Error("a Cloud site is not asked for the email its auth needs")
	}
	if required(&config.Jira{URL: "https://jira.acme.example", Token: "${T}"}) {
		t.Error("a Data Center instance is asked for an email it does not use")
	}
}
