package googlecloud_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
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
	for _, r := range googlecloud.Requirements(testCompany, block, nil, resolve) {
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
	before := googlecloud.Requirements(testCompany, nil, nil, nil)[1]
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
	text, err := googlecloud.CloudShellBlock(testCompany, provisioner, pemText, false, nil)
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
		`--issuer-uri="` + googleauth.Issuer(testCompany) + `"`,
		`--attribute-mapping="google.subject=assertion.sub"`,
		`--attribute-condition="assertion.sub == '` + googleauth.Subject(testCompany) + `'"`,
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
	if got := strings.ReplaceAll(member, "${NUMBER}", number); got != googleauth.Principal(testProvider, testCompany) {
		t.Errorf("the provisioner trusts %q, want the client's principal %q", got,
			googleauth.Principal(testProvider, testCompany))
	}
	if _, err := googlecloud.CloudShellBlock(testCompany, "ops@acme.example", pemText, false, nil); err == nil {
		t.Error("a block was drawn for a provisioner that is no service account")
	}
	if _, err := googlecloud.CloudShellBlock(testCompany, provisioner, "not a key", false, nil); err == nil {
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
	text, err := googlecloud.CloudShellBlock(testCompany, provisioner, pemText, false, nil)
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
		// A STOP WRITES TO STDERR, so naming gcloud in its words prints nothing.
		if strings.Contains(s, "gcloud ") && !strings.HasPrefix(s, "stop ") {
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

var update = flag.Bool("update", false, "rewrite testdata/cloudshell-*.golden.sh from the block's text")

// fixedKey is a federation key that is the same on every run, so a golden
// block's JWKS is too. Obviously a test key.
func fixedKey(t *testing.T) string {
	t.Helper()
	d := sha256.Sum256([]byte("crewlet cloud shell golden"))
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d[:])
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// THE BLOCK IS EXACTLY ITS GOLDEN TEXT, with and without the atlas and a growth
// product, so any change to what a person pastes into Cloud Shell is a reviewed
// diff of testdata. Run with -update to rewrite the files.
func TestTheBlockMatchesItsGolden(t *testing.T) {
	t.Parallel()
	key := fixedKey(t)
	for name, tc := range map[string]struct {
		atlas  bool
		growth []googleauth.Product
	}{
		"plain": {},
		"atlas-growth": {atlas: true, growth: []googleauth.Product{{Name: "Google Play",
			APIs: []string{"androidpublisher.googleapis.com", "playdeveloperreporting.googleapis.com"}}}},
	} {
		text, err := googlecloud.CloudShellBlock(testCompany, provisioner, key, tc.atlas, tc.growth)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "cloudshell-"+name+".golden.sh")
		if *update {
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to write it)", err)
		}
		if text != string(want) {
			t.Errorf("the %s block differs from %s; run with -update and review the diff", name, path)
		}
	}
}

// fakeGcloud is a gcloud on PATH that logs each call and answers from the
// environment: FAKE_ACCOUNT, FAKE_APIS and FAKE_ACCOUNTS (space-separated),
// and FAKE_SERVICES_FAIL. Every describe finds what it asks about.
const fakeGcloud = `#!/bin/bash
echo "$*" >> "$GCLOUD_LOG"
case "$*" in
  "config get-value account") [ -n "$FAKE_ACCOUNT" ] && echo "$FAKE_ACCOUNT"; exit 0 ;;
  "services list --enabled "*) [ -z "$FAKE_SERVICES_FAIL" ] || exit 1; printf '%s\n' $FAKE_APIS ;;
  "iam service-accounts list "*) printf '%s\n' $FAKE_ACCOUNTS ;;
  "projects describe "*) echo 123456789012 ;;
  *" describe "*|"iam workload-identity-pools providers describe "*)
    echo "projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine" ;;
esac
exit 0
`

// preflightRun is one run of the block against [fakeGcloud].
type preflightRun struct {
	stdout, stderr string
	failed         bool
	calls          []string
}

// runBlock runs block under bash with the fake gcloud and env.
func runBlock(t *testing.T, block string, env map[string]string) preflightRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("the block is bash, and this machine has none: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte(fakeGcloud), 0o755); err != nil { //nolint:gosec // an executable fake
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "calls")
	cmd := exec.CommandContext(t.Context(), bash, "-c", block)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "TMPDIR=" + dir, "GCLOUD_LOG=" + logPath}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	logged, _ := os.ReadFile(logPath)
	return preflightRun{stdout: stdout.String(), stderr: stderr.String(), failed: runErr != nil,
		calls: strings.Split(strings.TrimSpace(string(logged)), "\n")}
}

// The gcloud calls the preflight makes, all of them reads.
var preflightReads = []string{"config get-value account", "services list --enabled ", "iam service-accounts list "}

// THE BLOCK STOPS BEFORE IT CHANGES ANYTHING IN A PROJECT THAT IS NOT CREWLET'S
// ALONE, with one line saying why, and runs to the provider in Crewlet's own
// project, a first time or again. Mutation: drop the preflight, or let it read
// the accounts with IAM off, or forget the provisioner, and a case fails.
func TestTheBlockStopsInAProjectThatIsNotCrewletsAlone(t *testing.T) {
	t.Parallel()
	const ownAPIs = "iam.googleapis.com iamcredentials.googleapis.com sts.googleapis.com cloudasset.googleapis.com"
	agent := googlecloud.AccountID(testCompany, "sre") + "@" + identity + ".iam.gserviceaccount.com"
	foreign := "firebase-adminsdk-x1@" + identity + ".iam.gserviceaccount.com"
	compute := "123456789012-compute@developer.gserviceaccount.com"
	for _, tc := range []struct {
		name        string
		provisioner string
		env         map[string]string
		stop        string
	}{
		{"a new project", provisioner, map[string]string{"FAKE_ACCOUNT": "ops@example.com",
			"FAKE_APIS": "bigquery.googleapis.com storage.googleapis.com"}, ""},
		{"Crewlet's own project, again", provisioner, map[string]string{"FAKE_ACCOUNT": "ops@example.com",
			"FAKE_APIS": ownAPIs, "FAKE_ACCOUNTS": provisioner + " " + agent + " " +
				googlecloud.AtlasAccountIn(identity) + " " + googleauth.AccountEmail(googleauth.ReaderAccountID(testCompany), identity)}, ""},
		{"a provisioner named otherwise", "ops-bot@" + identity + ".iam.gserviceaccount.com", map[string]string{
			"FAKE_ACCOUNT": "ops@example.com", "FAKE_APIS": ownAPIs,
			"FAKE_ACCOUNTS": "ops-bot@" + identity + ".iam.gserviceaccount.com"}, ""},
		{"no active account", provisioner, map[string]string{}, "Cloud Shell has no active account. Run gcloud auth login"},
		{"APIs it cannot read", provisioner, map[string]string{"FAKE_ACCOUNT": "ops@example.com", "FAKE_SERVICES_FAIL": "1"},
			"could not read the APIs enabled on " + identity},
		{"an app", provisioner, map[string]string{"FAKE_ACCOUNT": "ops@example.com",
			"FAKE_APIS": "firebase.googleapis.com compute.googleapis.com iam.googleapis.com"},
			identity + " runs workloads (Firebase, Compute Engine)."},
		{"an account Crewlet did not make", provisioner, map[string]string{"FAKE_ACCOUNT": "ops@example.com",
			"FAKE_APIS": ownAPIs, "FAKE_ACCOUNTS": provisioner + " " + foreign},
			identity + " holds a service account Crewlet did not make (" + foreign + ")."},
		{"several accounts Crewlet did not make", provisioner, map[string]string{"FAKE_ACCOUNT": "ops@example.com",
			"FAKE_APIS": ownAPIs, "FAKE_ACCOUNTS": compute + " " + agent + " " + foreign},
			identity + " holds 2 service accounts Crewlet did not make, such as " + compute + "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			block, err := googlecloud.CloudShellBlock(testCompany, tc.provisioner, fixedKey(t), true, nil)
			if err != nil {
				t.Fatal(err)
			}
			run := runBlock(t, block, tc.env)
			if tc.stop == "" {
				if run.failed || strings.TrimSpace(run.stdout) != googlecloud.ProviderName("123456789012") {
					t.Fatalf("the block did not run to the provider: stdout %q, stderr %q", run.stdout, run.stderr)
				}
				return
			}
			lines := strings.Split(strings.TrimSpace(run.stderr), "\n")
			last := lines[len(lines)-1]
			if !run.failed || run.stdout != "" || !strings.HasPrefix(last, googlecloud.StopPrefix) ||
				!strings.Contains(last, tc.stop) {
				t.Fatalf("stderr ends %q (failed %v, stdout %q), want the stop line saying %q",
					last, run.failed, run.stdout, tc.stop)
			}
			if strings.Contains(tc.stop, identity+" ") && !strings.Contains(last, "a new, empty project used only for "+
				"Crewlet: the provisioner becomes Service Account Admin of the whole project") {
				t.Errorf("the stop line %q does not say what the project must be, and why", last)
			}
			for _, call := range run.calls {
				if !slices.ContainsFunc(preflightReads, func(read string) bool { return strings.HasPrefix(call, read) }) {
					t.Errorf("the block ran %q before it stopped", call)
				}
			}
		})
	}
}

// THE PREFLIGHT AND THE ENGINE CLAIM THE SAME ACCOUNTS, so the block never
// stops over an account the probe and the pass call Crewlet's, nor runs beside
// one they warn about. Mutation: change the prefix in either, and this fails.
func TestThePreflightClaimsTheAccountsTheEngineDoes(t *testing.T) {
	t.Parallel()
	block, err := googlecloud.CloudShellBlock(testCompany, provisioner, fixedKey(t), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{
		provisioner, "crewlet-atlas@" + identity + ".iam.gserviceaccount.com",
		googlecloud.AccountID(testCompany, "sre") + "@" + identity + ".iam.gserviceaccount.com",
		"deploy@" + identity + ".iam.gserviceaccount.com", "123-compute@developer.gserviceaccount.com",
		"crewletx-a@" + identity + ".iam.gserviceaccount.com",
	} {
		run := runBlock(t, block, map[string]string{"FAKE_ACCOUNT": "ops@example.com",
			"FAKE_APIS": "iam.googleapis.com", "FAKE_ACCOUNTS": email})
		if stopped := run.failed; stopped == googlecloud.Ours(email, provisioner) {
			t.Errorf("%s: the block stopped %v, the engine calls it Crewlet's %v", email, stopped,
				googlecloud.Ours(email, provisioner))
		}
	}
}
