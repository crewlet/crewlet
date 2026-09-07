package jira_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jira"
)

// fields is what the form asks, for a company that has DECLARED which
// Atlassian it runs. The deployment is no longer derived from the address:
// a form has to decide what to ask before an address exists.
func fields(t *testing.T, cloud bool, in *config.Jira) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, r := range jira.Requirements(in, cloud, func(string) (string, bool) { return "", false }) {
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
	got := fields(t, true, &config.Jira{URL: "https://acme.atlassian.net"})
	// NOT the site address: a Cloud company's sites are read from its
	// organization, and an address typed here is used INSTEAD of the
	// gateway, which is the only place a service account's token
	// authenticates.
	for _, want := range []string{"email", "token", "webhook_secret"} {
		if !got[want] {
			t.Errorf("a Cloud site was not asked for %s", want)
		}
	}
}

// AND A DATA CENTER INSTANCE IS NOT ASKED FOR THE GATEWAY'S. A cloud id and
// a link address mean nothing off Atlassian's own gateway.
func TestADataCenterInstanceIsNotAskedForCloudFields(t *testing.T) {
	t.Parallel()
	got := fields(t, false, &config.Jira{URL: "https://jira.acme.example"})
	for _, gone := range []string{"cloud_id", "site_url"} {
		if got[gone] {
			t.Errorf("a Data Center instance was asked for %s", gone)
		}
	}
	if !got["webhook_secret"] {
		t.Error("a Data Center instance was not asked for its signing secret")
	}
	// AND IT IS ASKED FOR ITS ADDRESS, which is the only way in: nothing
	// discovers a self-hosted instance.
	if !got["url"] {
		t.Error("a Data Center instance was not asked for its address")
	}
}

// A VALUE THE DOCUMENT HOLDS SURVIVES THE FILTER.
//
// The document is a fact: dropping a setting a company has written down would
// make the form disagree with the configuration, and Save would then clear
// it.
func TestAWrittenFieldIsNeverHidden(t *testing.T) {
	t.Parallel()
	got := fields(t, false, &config.Jira{
		URL:     "https://jira.acme.example",
		CloudID: "abc-123",
	})
	if !got["cloud_id"] {
		t.Error("a cloud id the document holds was hidden from the form")
	}
}

// A CLOUD BLOCK KEEPS SOMEWHERE TO RECORD WHAT DISCOVERY FINDS.
//
// The organization pass reads the site and its cloud id from the
// organization key and writes them into these fields. Dropping them from a
// company that has connected nothing yet left nothing for the discovered
// values to be written into, so a connect that named no site was refused by
// the config for naming no instance, which is the exact state discovery
// exists to fill.
func TestACloudBlockKeepsSomewhereToRecordTheSite(t *testing.T) {
	t.Parallel()
	got := fields(t, true, &config.Jira{Email: "${E}", Token: "${T}"})
	for _, want := range []string{"cloud_id", "site_url"} {
		if !got[want] {
			t.Errorf("a block with no address offers no %s to record one in", want)
		}
	}
}

// THE ACCOUNT EMAIL IS REQUIRED ON CLOUD AND UNUSED ON DATA CENTER.
//
// Cloud authenticates an API token as Basic base64(email:token) and refuses
// it as a bearer, measured against a live site: 403 without the address, 200
// with it. So without the email the webhook this integration exists to
// register is never created. Data Center takes the token as a bearer and
// wants no address at all.
//
// It follows the DECLARED deployment now. Derived from the address it was
// wrong in exactly the case that mattered: with nothing typed yet, an empty
// string reads as Data Center, so the one field a fresh Cloud connect cannot
// do without was marked optional on the form where it is first asked.
func TestTheAccountEmailFollowsTheDeclaredDeployment(t *testing.T) {
	t.Parallel()
	required := func(cloud bool, in *config.Jira) bool {
		for _, r := range jira.Requirements(in, cloud, func(string) (string, bool) { return "", false }) {
			if r.Field == "email" {
				return r.Required
			}
		}
		t.Fatal("no email requirement")
		return false
	}
	if !required(true, &config.Jira{Token: "${T}"}) {
		t.Error("a Cloud company that has connected nothing yet is not asked for the email")
	}
	if !required(true, &config.Jira{URL: "https://acme.atlassian.net", Token: "${T}"}) {
		t.Error("a Cloud site is not asked for the email its auth needs")
	}
	if required(false, &config.Jira{URL: "https://jira.acme.example", Token: "${T}"}) {
		t.Error("a Data Center instance is asked for an email it does not use")
	}
}
