package oidc_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam/oidc"
)

// A DISCOVERY DOCUMENT THAT SAYS THE SIGN-IN CANNOT WORK THERE IS REPORTED,
// NAMING THE FIELD — AND AN ABSENT LIST SAYS NOTHING.
//
// Each of these would otherwise be met a round trip later as a failure with no
// name: every sign-in refused as an unverifiable token (no algorithm this
// engine verifies), a request the provider refuses (no `code` response type),
// a PKCE challenge the provider may ignore (no S256), a probe with no refresh
// token to ask with (no `offline_access`). Every list is optional in the
// discovery specification and unevenly kept, so an absent one is not a
// concern, and `offline_access` is one only while this deployment asks for it.
//
// Mutation: drop a check and its field goes missing from the first case;
// treat an absent list as a mismatch and the second case reports.
func TestADiscoveryDocumentsConcernsNameTheirField(t *testing.T) {
	t.Parallel()
	incapable := oidc.Metadata{
		ResponseTypes:      []string{"id_token"},
		SigningAlgorithms:  []string{"EdDSA", "HS256"},
		CodeChallengeTypes: []string{"plain"},
		ScopesSupported:    []string{"openid", "email"},
	}
	var fields []string
	for _, c := range incapable.Concerns(testConfig()) {
		fields = append(fields, c.Field)
		if c.Detail == "" {
			t.Errorf("the %s concern says nothing an operator could act on", c.Field)
		}
	}
	for _, want := range []string{"response_types_supported",
		"id_token_signing_alg_values_supported", "code_challenge_methods_supported",
		"scopes_supported"} {
		if !slices.Contains(fields, want) {
			t.Errorf("a document advertising nothing this engine uses raised no "+
				"concern about %s (raised %v)", want, fields)
		}
	}

	if got := (oidc.Metadata{}).Concerns(testConfig()); len(got) != 0 {
		t.Errorf("a document advertising no lists at all raised %+v", got)
	}
	capable := oidc.Metadata{
		ResponseTypes:      []string{"code", "id_token"},
		SigningAlgorithms:  []string{"RS256", "EdDSA"},
		CodeChallengeTypes: []string{"plain", "S256"},
		ScopesSupported:    []string{"openid", "offline_access"},
	}
	if got := capable.Concerns(testConfig()); len(got) != 0 {
		t.Errorf("a document advertising what this engine uses raised %+v", got)
	}
	narrowed := testConfig()
	narrowed.Scopes = []string{"openid", "email"}
	withoutOffline := capable
	withoutOffline.ScopesSupported = []string{"openid"}
	if got := withoutOffline.Concerns(narrowed); len(got) != 0 {
		t.Errorf("a deployment that asks no offline_access was told the provider "+
			"lacks it: %+v", got)
	}
}

// AND THE PROVIDER SAYS SO WHEN IT FETCHES THE DOCUMENT.
//
// Mutation: drop the report at the fetch and nothing is logged.
func TestAProviderReportsItsDocumentsConcernsWhenItFetchesIt(t *testing.T) {
	t.Parallel()
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                server.URL,
			"authorization_endpoint":                server.URL + "/authorize",
			"token_endpoint":                        server.URL + "/token",
			"jwks_uri":                              server.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"EdDSA"},
		})
	}))
	t.Cleanup(server.Close)
	config := testConfig()
	config.Issuer = server.URL
	var logged bytes.Buffer
	provider := oidc.NewProvider(config, server.Client(), func() time.Time { return at }).
		WithLogger(slog.New(slog.NewTextHandler(&logged, nil)))
	if _, err := provider.Metadata(t.Context()); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if line := logged.String(); !strings.Contains(line, "oidc_provider_metadata_concern") ||
		!strings.Contains(line, "id_token_signing_alg_values_supported") {
		t.Errorf("fetching a document this engine cannot verify tokens from "+
			"logged %q", line)
	}
}
