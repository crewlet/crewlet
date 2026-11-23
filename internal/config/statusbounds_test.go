package config

import (
	"strings"
	"testing"
)

// AN ADDRESS AN INTEGRATION'S STATUS RECORDS EXACTLY IS BOUNDED, by the
// validator and in the artifact alike: `api.external_url` and
// `api.public.external_url` at MaxExternalURLLength characters, Datadog's
// `webhook_name` at MaxWebhookNameLength. The status keeps each whole to
// compare with the one in force, so these bounds are what give that record a
// widest form (internal/integration's TestTheWidestStatusFitsItsBound counts
// them). Characters, not bytes, as the schema counts them.
//
// Mutation: drop either check, or set a tag's maxlen to a figure the constant
// does not carry, and its arm fails.
func TestAnAddressAStatusKeepsExactIsBoundedWhereTheSchemaSaysSo(t *testing.T) {
	t.Parallel()
	at := func(n int) string { return "https://e.example/" + strings.Repeat("é", n-len("https://e.example/")) }
	if err := checkExternalURL(field("external_url"), at(MaxExternalURLLength), "", ""); err != nil {
		t.Errorf("an address of %d characters = %v, want it accepted", MaxExternalURLLength, err)
	}
	if err := checkExternalURL(field("external_url"), at(MaxExternalURLLength+1), "", ""); err == nil {
		t.Errorf("an address of %d characters was accepted", MaxExternalURLLength+1)
	}
	name := func(n int) *Datadog { return &Datadog{Enabled: true, WebhookName: strings.Repeat("é", n)} }
	if err := name(MaxWebhookNameLength).validate(field("datadog")); err != nil &&
		strings.Contains(err.Error(), "at most") {
		t.Errorf("a webhook name of %d characters = %v, want it within its bound", MaxWebhookNameLength, err)
	}
	if err := name(MaxWebhookNameLength + 1).validate(field("datadog")); err == nil ||
		!strings.Contains(err.Error(), "webhook_name") || !strings.Contains(err.Error(), "at most") {
		t.Errorf("a webhook name of %d characters = %v, want it refused naming the field",
			MaxWebhookNameLength+1, err)
	}

	for _, c := range []struct {
		tier       Tier
		def, field string
		bound      int
	}{
		{TierBootstrap, "API", "external_url", MaxExternalURLLength},
		{TierBootstrap, "APIPublic", "external_url", MaxExternalURLLength},
		{TierCompany, "Datadog", "webhook_name", MaxWebhookNameLength},
	} {
		defs, _ := schemaDoc(t, c.tier)["$defs"].(map[string]any)
		def, _ := defs[c.def].(map[string]any)
		props, _ := def["properties"].(map[string]any)
		prop, _ := props[c.field].(map[string]any)
		bound := prop["maxLength"]
		for _, branch := range anyOf(prop) {
			if own, _ := branch.(map[string]any); own["maxLength"] != nil {
				bound = own["maxLength"]
			}
		}
		if got, _ := bound.(float64); int(got) != c.bound {
			t.Errorf("%s.%s: the schema bounds it at %v, the validator at %d", c.def, c.field, bound, c.bound)
		}
	}
}
