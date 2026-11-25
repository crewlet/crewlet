package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
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

// AN OBJECT STORE'S IDENTITY FITS THE FLEET'S RECORD OF IT: the endpoint, the
// bucket and the prefix are each refused past their bound, naming the field,
// where the schema bounds them too, and at their bounds, every character four
// bytes and the endpoint behind the shorter scheme, http://, the identity the
// fleet records, 13,292 bytes, is a small value.
//
// Mutation: drop the prefix from checkObjectIdentity, or raise
// MaxS3PrefixLength to 4096, and it fails.
func TestAnObjectStoresIdentityFitsTheFleetsRecordOfIt(t *testing.T) {
	t.Parallel()
	wide := func(prefix string, n int) string { return prefix + strings.Repeat("😀", n-len(prefix)) }
	widest := ObjectsS3{Endpoint: wide("http://", MaxExternalURLLength), Region: "us-east-1",
		Bucket: wide("", MaxS3BucketLength), Prefix: wide("", MaxS3PrefixLength)}
	if err := errors.Join(checkObjectIdentity(field("s3"), widest),
		checkS3Endpoint(field("endpoint"), widest.Endpoint)); err != nil {
		t.Errorf("an S3 store at every bound = %v, want it within them", err)
	}
	id := StoreObjects{Backend: ObjectBackendS3, S3: widest}.Identity()
	if len(id) != 13_292 {
		t.Errorf("the widest S3 store's identity is %d bytes, want the 13,292 its bounds give", len(id))
	}
	if len(id) > coord.SmallValueBytes {
		t.Errorf("the widest S3 store's identity is %d bytes, past the %d the fleet's record holds",
			len(id), coord.SmallValueBytes)
	}
	for name, past := range map[string]ObjectsS3{
		"endpoint": {Endpoint: widest.Endpoint + "a"},
		"bucket":   {Bucket: widest.Bucket + "a"},
		"prefix":   {Prefix: widest.Prefix + "a"},
	} {
		if err := checkObjectIdentity(field("s3"), past); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("an S3 %s past its bound = %v, want it refused naming the field", name, err)
		}
	}
	for f, bound := range map[string]int{"endpoint": MaxExternalURLLength, "bucket": MaxS3BucketLength,
		"prefix": MaxS3PrefixLength} {
		defs, _ := schemaDoc(t, TierBootstrap)["$defs"].(map[string]any)
		def, _ := defs["ObjectsS3"].(map[string]any)
		props, _ := def["properties"].(map[string]any)
		prop, _ := props[f].(map[string]any)
		schema := prop["maxLength"]
		for _, branch := range anyOf(prop) {
			if own, _ := branch.(map[string]any); own["maxLength"] != nil {
				schema = own["maxLength"]
			}
		}
		if got, _ := schema.(float64); int(got) != bound {
			t.Errorf("ObjectsS3.%s: the schema bounds it at %v, the validator at %d", f, schema, bound)
		}
	}
}
