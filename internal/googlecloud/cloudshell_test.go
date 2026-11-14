package googlecloud_test

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/setup"
)

// heldKeyRow is the federation key's row of a company whose provisioner is
// saved and whose key is held as pemText.
func heldKeyRow(t *testing.T, pemText string) setup.Requirement {
	t.Helper()
	block := &config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{
		Provisioner: "${GCP_PROVISIONER}", FederationKey: "${" + googlecloud.FederationKeySecret + "}",
	}}
	values := map[string]string{"GCP_PROVISIONER": provisioner, googlecloud.FederationKeySecret: pemText}
	resolve := func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
	for _, r := range googlecloud.Requirements(testCompany, block, resolve) {
		if r.Field == "federation_key" {
			return r
		}
	}
	t.Fatal("no federation_key row")
	return setup.Requirement{}
}

// statements is a shell block's commands, each continued line joined to its
// first.
func statements(text string) []string {
	var out []string
	current := ""
	for line := range strings.SplitSeq(text, "\n") {
		current += strings.TrimSpace(line)
		if cut, ok := strings.CutSuffix(current, `\`); ok {
			current = cut
			continue
		}
		if current != "" {
			out = append(out, current)
		}
		current = ""
	}
	return out
}

// THE KEY IS MINTED AS AN ES256 KEY, AND ONCE IT IS HELD ITS ROW CARRIES THE
// BLOCK: only the key's public half, as the JWKS Google matches the engine's
// tokens against by the key's thumbprint, and never the private half, which
// the wire refuses to carry.
func TestTheBlockCarriesThePublicHalfAlone(t *testing.T) {
	t.Parallel()
	key, pemText := fakeKey(t)
	before := googlecloud.Requirements(testCompany, nil, nil)[1]
	if before.Shape != setup.ShapeES256Key || before.Public != nil {
		t.Fatalf("before the key, its row is shape %q with view %v, want an ES256 mint and no block",
			before.Shape, before.Public)
	}
	row := heldKeyRow(t, pemText)
	if row.Public == nil || row.Public.Language != "sh" {
		t.Fatalf("the held key's row shows %+v, want the Cloud Shell block", row.Public)
	}
	text := row.Public.Text
	body := strings.TrimSpace(strings.TrimPrefix(pemText, "-----BEGIN PRIVATE KEY-----"))
	if strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, body[:40]) {
		t.Fatal("the block holds the private key")
	}
	if _, err := json.Marshal(row); err != nil {
		t.Fatalf("the row does not reach the wire: %v", err)
	}
	_, after, _ := strings.Cut(text, "<<'CREWLET_JWKS'\n")
	jwks, _, found := strings.Cut(after, "\nCREWLET_JWKS\n")
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if !found || json.Unmarshal([]byte(jwks), &set) != nil || len(set.Keys) != 1 {
		t.Fatalf("the block's heredoc holds %q, want one key's JWKS", jwks)
	}
	jwk := set.Keys[0]
	x, _ := base64.RawURLEncoding.DecodeString(jwk["x"])
	if want, _ := coordinates(&key.PublicKey); want == nil {
		t.Fatal("the federation key has no public point")
	} else if _, private := jwk["d"]; private || jwk["alg"] != "ES256" || !slices.Equal(x, want) {
		t.Fatalf("the JWKS key %v is not the federation key's public half", jwk)
	}
	if jwk["kid"] != thumbprint(&key.PublicKey) {
		t.Errorf("the JWKS names the key %q, want its thumbprint, which the engine's tokens name", jwk["kid"])
	}
}

// THE BLOCK TRUSTS EXACTLY WHAT THE CLIENT SIGNS: the issuer and subject the
// engine's token carries, the provider name it prints is the one the client
// takes, and the principal the provisioner lets act as it is the client's.
func TestTheBlockTrustsWhatTheClientSigns(t *testing.T) {
	t.Parallel()
	_, pemText := fakeKey(t)
	text, err := googlecloud.CloudShellBlock(testCompany, provisioner, pemText, false)
	if err != nil {
		t.Fatal(err)
	}
	number := strings.Split(testProvider, "/")[1]
	if got := googlecloud.ProviderName(number); got != testProvider {
		t.Fatalf("the block's provider in project %s is %q, want %q", number, got, testProvider)
	}
	for _, want := range []string{
		"IDENTITY=" + identity,
		"PROV=" + provisioner,
		`--issuer-uri="` + googlecloud.Issuer(testCompany) + `"`,
		`--attribute-mapping="google.subject=assertion.sub"`,
		`--attribute-condition="assertion.sub == '` + googlecloud.Subject(testCompany) + `'"`,
	} {
		if strings.Count(text, want) == 0 {
			t.Errorf("the block does not say %s", want)
		}
	}
	var member string
	for _, s := range statements(text) {
		if strings.Contains(s, "roles/iam.workloadIdentityUser") {
			_, rest, _ := strings.Cut(s, `--member="`)
			member, _, _ = strings.Cut(rest, `"`)
		}
	}
	if got := strings.ReplaceAll(member, "${NUMBER}", number); got != googlecloud.Principal(testProvider, testCompany) {
		t.Errorf("the provisioner trusts %q, want the client's principal %q", got,
			googlecloud.Principal(testProvider, testCompany))
	}
	if _, err := googlecloud.CloudShellBlock(testCompany, "ops@acme.example", pemText, false); err == nil {
		t.Error("a block was drawn for a provisioner that is no service account")
	}
	if _, err := googlecloud.CloudShellBlock(testCompany, provisioner, "not a key", false); err == nil {
		t.Error("a block was drawn for a key the engine never minted")
	}
}

// THE BLOCK IS SAFE TO PASTE AGAIN AND PRINTS ONLY THE PROVIDER: everything
// it makes is described first and made only where absent, an existing
// provider is updated with the current key, it runs in a subshell so a failed
// step leaves Cloud Shell open, and every command but the last is silenced, so
// the provider's name is the last line it prints.
func TestTheBlockIsSafeToPasteAgainAndPrintsTheProvider(t *testing.T) {
	t.Parallel()
	_, pemText := fakeKey(t)
	text, err := googlecloud.CloudShellBlock(testCompany, provisioner, pemText, false)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if lines[0] != "(" || lines[len(lines)-1] != ")" || !slices.Contains(lines, "set -uo pipefail") ||
		!slices.Contains(lines, "export CLOUDSDK_CORE_DISABLE_PROMPTS=1") {
		t.Fatalf("the block is not one subshell that stops on an unset name with prompts off:\n%s", text)
	}
	for _, made := range []struct{ describe, create string }{
		{"gcloud iam service-accounts describe", "gcloud iam service-accounts create"},
		{"gcloud iam workload-identity-pools describe", "gcloud iam workload-identity-pools create"},
		{"gcloud iam workload-identity-pools providers describe", "gcloud iam workload-identity-pools providers create-oidc"},
	} {
		d, c := strings.Index(text, made.describe), strings.Index(text, made.create)
		if d < 0 || c < 0 || d > c {
			t.Errorf("%q is not guarded by %q", made.create, made.describe)
		}
	}
	if !strings.Contains(text, "providers update-oidc "+googlecloud.ProviderID) {
		t.Error("an existing provider is not updated with the current key")
	}
	all := statements(text)
	var commands []string
	for _, s := range all {
		if strings.Contains(s, "gcloud ") {
			commands = append(commands, s)
		}
	}
	last := commands[len(commands)-1]
	if all[len(all)-2] != last || !strings.HasPrefix(last, "gcloud iam workload-identity-pools providers describe "+
		googlecloud.ProviderID) || !strings.HasSuffix(last, "--format='value(name)'") {
		t.Fatalf("the block's last command is %q, want the provider's name printed", last)
	}
	for _, s := range commands[:len(commands)-1] {
		if !strings.Contains(s, "--format=none") && !strings.Contains(s, ">/dev/null") && !strings.Contains(s, "$(gcloud") {
			t.Errorf("%q prints to the block's output, so the provider is not its last line", s)
		}
	}
	for _, s := range commands {
		if strings.Contains(s, "add-iam-policy-binding \"$IDENTITY\"") && !strings.Contains(s, "--condition=None") {
			t.Errorf("%q grants with a condition prompt left open", s)
		}
	}
}
