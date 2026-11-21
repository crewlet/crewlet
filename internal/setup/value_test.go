package setup_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// provisioner is a pasted credential's field, held to Cloudflare's alphabet.
var provisioner = setup.Requirement{
	Field: "api_token", Label: "Provisioner token", Kind: setup.KindSecret,
	ConfigPath: "integrations.cloudflare.provisioning.api_token", SecretName: "CLOUDFLARE_API_TOKEN",
	Shape: setup.ShapeOpaqueToken,
}

// A PASTED CREDENTIAL WITH A STRAY CHARACTER INSIDE IS REFUSED AT ITS FIELD,
// before anything is sealed or written, saying how many of what and where and
// never quoting the value.
func TestASecretWithAStrayCharacterIsRefusedAtItsField(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	pasted := "cfat_abcdef ghijkl\u200bmnop"
	_, err := writer(rec).Write(t.Context(), []setup.Requirement{provisioner}, setup.Submission{
		Kind: integration.KindCloudflare, Values: map[string]string{"api_token": "  " + pasted + "\n"},
		Summary: "connect cloudflare", By: founder,
	})
	var value *setup.ValueError
	if !errors.As(err, &value) {
		t.Fatalf("Write = %v, want a ValueError", err)
	}
	if value.Field != "api_token" || value.ConfigPath != provisioner.ConfigPath {
		t.Errorf("refused at %s %s, want the provisioner token's field", value.Field, value.ConfigPath)
	}
	for _, want := range []string{"provisioner token", "1 space and 1 invisible character", "characters 12 and 19"} {
		if !strings.Contains(value.Problem, want) {
			t.Errorf("the refusal %q does not say %q", value.Problem, want)
		}
	}
	if strings.Contains(err.Error(), "abcdef") {
		t.Errorf("the refusal %q quotes the credential", err)
	}
	if len(rec.events) != 0 || len(rec.secrets) != 0 {
		t.Errorf("a refused submission wrote %v and sealed %v", rec.events, rec.secrets)
	}
}

// A FIELD'S RULE: a secret is one unbroken line, a shape holds it to the
// vendor's alphabet, a reference is a pointer and never judged, and a value
// that is fine passes.
func TestAFieldJudgesItsValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, value string
		says        string
	}{
		{"a token", "cfat_Ab-12_xyz", ""},
		{"a reference", "${CLOUDFLARE_API_TOKEN}", ""},
		{"a line break inside", "cfat_ab\ncd", "1 line break"},
		{"characters no token has", `cfat_ab!cd"ef`, "characters 8 and 11"},
		{"one character no token has", "cfat_ab.cd", "character 8)"},
	} {
		got := provisioner.ValueProblem(tc.value)
		if (got == "") != (tc.says == "") || !strings.Contains(got, tc.says) {
			t.Errorf("%s: %q, want a sentence saying %q", tc.name, got, tc.says)
		}
	}
	if got := (setup.Requirement{Field: "webhook_token", Kind: setup.KindSecret}).ValueProblem("ab:cd!ef"); got != "" {
		t.Errorf("a secret with no shape was held to an alphabet: %q", got)
	}
	for label, want := range map[string]string{
		"Provisioner token": "provisioner token", "API key": "API key", "": "api token",
	} {
		if got := (setup.Requirement{Field: "api_token", Label: label}).Noun(); got != want {
			t.Errorf("Noun of %q = %q, want %q", label, got, want)
		}
	}
}

// A VENDOR-ISSUED IDENTIFIER IS HELD TO ITS SHAPE AND NEVER MINTED: a
// Scaleway Organization ID or secret key is a UUID, an access key SCW and 17
// capital letters or digits, and a value Crewlet made up would name nothing.
func TestAnIdentifierShapeHoldsAPasteAndRefusesAMint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		shape setup.Shape
		value string
		ok    bool
	}{
		{setup.ShapeUUID, "11111111-2222-4333-8444-555555555555", true},
		{setup.ShapeUUID, "11111111-2222-4333-8444-55555555555", false},
		{setup.ShapeUUID, "11111111222243338444555555555555", false},
		{setup.ShapeScalewayAccessKey, "SCWABCDEFGHIJ0123456", true},
		{setup.ShapeScalewayAccessKey, "SCWABCDEFGHIJ012345", false},
		{setup.ShapeScalewayAccessKey, "scwabcdefghij0123456", false},
		{setup.ShapeAppleKeyID, "2X9R4HXF34", true},
		{setup.ShapeAppleKeyID, "2x9r4hxf34", false},
		{setup.ShapeAppleKeyID, "2X9R4HXF3", false},
		{setup.ShapeAppleKey, "-----BEGIN PRIVATE KEY-----\nnot a key\n-----END PRIVATE KEY-----", false},
		{setup.ShapeAppleAdsKey, sec1Key(t), true},
		{setup.ShapeAppleAdsKey, "-----BEGIN EC PRIVATE KEY-----\nnot a key\n-----END EC PRIVATE KEY-----", false},
		{setup.ShapeAppleAdsID, "SEARCHADS.aeb3ef5f-0c5a-4f2a-99c8-fca83f25a9e1", true},
		{setup.ShapeAppleAdsID, "aeb3ef5f-0c5a-4f2a-99c8-fca83f25a9e1", false},
	} {
		problem := tc.shape.Problem("access key", tc.value)
		if (problem == "") != tc.ok {
			t.Errorf("%s %q: problem %q", tc.shape, tc.value, problem)
		}
		if problem != "" && strings.Contains(problem, tc.value) {
			t.Errorf("%s: the refusal quotes the value", tc.shape)
		}
	}
	for _, shape := range []setup.Shape{setup.ShapeUUID, setup.ShapeScalewayAccessKey, setup.ShapeAppleKey,
		setup.ShapeAppleKeyID, setup.ShapeAppleAdsKey, setup.ShapeAppleAdsID} {
		if value, err := shape.Mint(func() string { return "minted" }); err == nil || value != "" {
			t.Errorf("%s minted %q", shape, value)
		}
	}
}

// A GOOGLE IDENTIFIER IS HELD TO ITS SHAPE AND NEVER MINTED: a service
// account's email and a workload identity provider's full name name something
// Google made, and a value Crewlet made up would name nothing.
func TestAGoogleIdentifierIsHeldToItsShapeAndNeverMinted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		shape setup.Shape
		value string
		ok    bool
	}{
		{setup.ShapeGoogleServiceAccount, "crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com", true},
		{setup.ShapeGoogleServiceAccount, "ops@acme.example", false},
		{setup.ShapeGoogleServiceAccount, "acme-crewlet-id", false},
		{setup.ShapeGoogleProvider,
			"projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine", true},
		{setup.ShapeGoogleProvider, "projects/123456789012/locations/global/workloadIdentityPools/crewlet", false},
		{setup.ShapeGoogleProvider, "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/q", false},
	} {
		problem := tc.shape.Problem("provider", tc.value)
		if (problem == "") != tc.ok {
			t.Errorf("%s %q: problem %q", tc.shape, tc.value, problem)
		}
		if problem != "" && strings.Contains(problem, tc.value) {
			t.Errorf("%s: the refusal quotes the value", tc.shape)
		}
	}
	for _, shape := range []setup.Shape{setup.ShapeGoogleServiceAccount, setup.ShapeGoogleProvider} {
		if value, err := shape.Mint(func() string { return "minted" }); err == nil || value != "" {
			t.Errorf("%s minted %q", shape, value)
		}
	}
}

// sec1Key is a fresh P-256 key as `openssl ecparam -genkey` writes it.
func sec1Key(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}
