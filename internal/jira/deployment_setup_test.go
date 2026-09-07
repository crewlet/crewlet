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

// A FIELD THAT CANNOT APPLY IS NOT AN OPTIONAL FIELD.
//
// A Cloud site and a Data Center instance need genuinely different things,
// and the form offered both sets at once: a Cloud operator was asked for a
// webhook signing secret their site will never send, because Atlassian
// restricts the webhook API to Connect and OAuth apps and Cloud events reach
// this engine through the Forge relay instead.
func TestACloudSiteIsNotAskedForDataCenterFields(t *testing.T) {
	t.Parallel()
	got := fields(t, &config.Jira{URL: "https://acme.atlassian.net"})
	if got["webhook_secret"] {
		t.Error("a Cloud site was asked for a Data Center signing secret")
	}
	for _, want := range []string{"url", "email", "token"} {
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
		URL:           "https://acme.atlassian.net",
		WebhookSecret: "${JIRA_WEBHOOK_SECRET}",
	})
	if !got["webhook_secret"] {
		t.Error("a signing secret the document holds was hidden from the form")
	}
}
