package config

import (
	"strings"
	"testing"
)

// AN ADDRESS AN INTEGRATION'S STATUS RECORDS EXACTLY IS BOUNDED, by each
// field's validator and in the artifact alike: every external address at
// MaxExternalURLLength characters as written, Datadog's `webhook_name` at
// MaxWebhookNameLength whether or not its block is on. The status keeps the
// vendors' address and the name whole, so these bounds give that record a
// widest form (internal/integration's TestTheWidestStatusFitsItsBound).
// Characters, not bytes, as the schema counts them.
//
// Mutation: drop a field's check, skip the name's on a block switched off, or
// set a tag's maxlen to a figure the constant does not carry, and it fails.
func TestAnAddressAStatusKeepsExactIsBoundedWhereTheSchemaSaysSo(t *testing.T) {
	t.Parallel()
	at := func(prefix string, n int) string { return prefix + strings.Repeat("é", n-len(prefix)) }
	bound := "at most 2048"
	for name, validate := range map[string]func(raw string) error{
		"api.external_url": func(raw string) error {
			return (&API{ExternalURL: raw}).validateExternalURL(field("api"), true)
		},
		"api.public.external_url": func(raw string) error {
			return (&APIPublic{Port: 8443, ExternalURL: raw}).validate(field("public"), true)
		},
		"api.artifacts.external_url": func(raw string) error {
			return (&APIArtifacts{Port: 8001, ExternalURL: raw}).validate(field("artifacts"),
				&API{ExternalURL: "https://crewlet.example.com"}, true)
		},
	} {
		if err := validate(at("https://e.example/", MaxExternalURLLength)); err != nil &&
			strings.Contains(err.Error(), bound) {
			t.Errorf("%s of %d characters = %v, want it within its bound", name, MaxExternalURLLength, err)
		}
		if err := validate(at("https://e.example/", MaxExternalURLLength+1)); err == nil ||
			!strings.Contains(err.Error(), bound) {
			t.Errorf("%s of %d characters = %v, want it refused at its bound", name, MaxExternalURLLength+1, err)
		}
	}
	for _, on := range []bool{true, false} {
		name := func(n int) *Datadog { return &Datadog{Enabled: on, WebhookName: strings.Repeat("é", n)} }
		if err := name(MaxWebhookNameLength).validate(field("datadog")); err != nil &&
			strings.Contains(err.Error(), "at most") {
			t.Errorf("a webhook name of %d characters (on %t) = %v, want it within its bound",
				MaxWebhookNameLength, on, err)
		}
		if err := name(MaxWebhookNameLength + 1).validate(field("datadog")); err == nil ||
			!strings.Contains(err.Error(), "webhook_name") || !strings.Contains(err.Error(), "at most") {
			t.Errorf("a webhook name of %d characters (on %t) = %v, want it refused naming the field",
				MaxWebhookNameLength+1, on, err)
		}
	}

	for _, c := range []struct {
		tier       Tier
		def, field string
		bound      int
	}{
		{TierBootstrap, "API", "external_url", MaxExternalURLLength},
		{TierBootstrap, "APIPublic", "external_url", MaxExternalURLLength},
		{TierBootstrap, "APIArtifacts", "external_url", MaxExternalURLLength},
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
