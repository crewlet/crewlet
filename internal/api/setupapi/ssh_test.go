package setupapi_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE CA IS MINTED ON SAVE AND NEVER SHOWN: a Save asking the engine for the
// authority seals an unencrypted Ed25519 OpenSSH key as
// SSH_CERTIFICATE_AUTHORITY, the document holds only its reference, and every
// answer after it carries the authority's public line and nothing of its key.
func TestTheCAIsMintedOnSaveAndNeverShown(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	save := s.do(t, http.MethodPost, "/setup/integrations/ssh/inputs",
		`{"values": {"enabled": "true"}, "generate": ["certificate_authority"]}`, nil)
	if save.Code != http.StatusCreated {
		t.Fatalf("save = %d: %s", save.Code, save.Body)
	}
	held, ok := s.vault.get("SSH_CERTIFICATE_AUTHORITY")
	if !ok {
		t.Fatal("nothing was sealed as SSH_CERTIFICATE_AUTHORITY")
	}
	ca, err := setup.ParseSSHCertificateAuthority(held)
	if err != nil {
		t.Fatalf("the sealed value is not an authority the engine signs with: %v", err)
	}
	if ca.Signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Errorf("the authority is %s, want Ed25519", ca.Signer.PublicKey().Type())
	}
	doc := s.do(t, http.MethodGet, "/config", "", nil)
	if !strings.Contains(doc.Body.String(), "${SSH_CERTIFICATE_AUTHORITY}") {
		t.Fatalf("the document does not point at the sealed authority: %s", doc.Body)
	}
	res := s.do(t, http.MethodGet, "/setup/integrations/ssh", "", nil)
	var state setupapi.ToolState
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	var public string
	for _, r := range state.Requirements {
		if r.Field == "certificate_authority" && r.Public != nil {
			public = r.Public.Text
		}
	}
	if strings.TrimSpace(public) != ca.PublicLine() || !strings.Contains(public, " crewlet-ssh-ca-") {
		t.Errorf("the form shows %q, want the authority's public line %q", public, ca.PublicLine())
	}
	lines := strings.Split(strings.TrimSpace(held), "\n")
	for name, body := range map[string]string{"save": save.Body.String(), "config": doc.Body.String(),
		"listing": res.Body.String()} {
		if strings.Contains(body, "PRIVATE KEY") {
			t.Errorf("the %s answer carries a private key's armour", name)
		}
		for _, line := range lines[1 : len(lines)-1] {
			if strings.Contains(body, line) {
				t.Errorf("the %s answer carries a line of the authority's private key", name)
			}
		}
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(public)); err != nil {
		t.Errorf("the shown line is not one a server's TrustedUserCAKeys reads: %v", err)
	}
}

// sshHostKeyLine is a made-up server's host key line.
func sshHostKeyLine(t *testing.T) (string, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))), ssh.FingerprintSHA256(pk)
}

// A SERVER IS WRITTEN AS A REVISION AND REMOVED ONLY ONCE NO AGENT NAMES IT: a
// server's fields land through the document's own rules, the answer shows it
// by its key's fingerprint beside the authority's public line, its setup is
// rendered trusting that line, and removing it is refused while an agent is
// granted it, naming the agent.
func TestAnSSHServerIsWrittenAsARevisionAndRemovedOnlyOnceNoAgentNamesIt(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	if res := s.do(t, http.MethodPut, "/setup/integrations/ssh/hosts/api-1", `{}`, nil); res.Code != http.StatusConflict ||
		!strings.Contains(res.Body.String(), "not_connected") {
		t.Fatalf("a server before SSH is connected = %d: %s", res.Code, res.Body)
	}
	s.connect(t, "ssh", `{"values": {"enabled": "true"}, "generate": ["certificate_authority"]}`)
	line, fingerprint := sshHostKeyLine(t)
	host := `{"address": "api-1.example.com", "host_keys": ["` + line + `"],
		"users": {"read": "crewlet-read", "operate": "crewlet-ops"}, "description": "Production API server"}`
	if res := s.do(t, http.MethodPut, "/setup/integrations/ssh/hosts/api-1", host, nil); res.Code != http.StatusOK {
		t.Fatalf("add the server = %d: %s", res.Code, res.Body)
	}
	bad := strings.Replace(host, "api-1.example.com", "127.0.0.1", 1)
	if res := s.do(t, http.MethodPut, "/setup/integrations/ssh/hosts/api-2", bad, nil); res.Code < 400 ||
		!strings.Contains(res.Body.String(), "integrations.ssh.hosts[1].address") {
		t.Errorf("a server on the engine host = %d: %s", res.Code, res.Body)
	}
	res := s.do(t, http.MethodGet, "/setup/integrations/ssh", "", nil)
	var state setupapi.ToolState
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	ca, err := setup.ParseSSHCertificateAuthority(func() string { v, _ := s.vault.get("SSH_CERTIFICATE_AUTHORITY"); return v }())
	if err != nil {
		t.Fatal(err)
	}
	if state.SSH == nil || state.SSH.CAPublicKey != ca.PublicLine() || state.SSH.SetupVersion != 1 ||
		len(state.SSH.Hosts) != 1 || state.SSH.Hosts[0].ID != "api-1" || state.SSH.Hosts[0].Port != 22 ||
		len(state.SSH.Hosts[0].Fingerprints) != 1 || state.SSH.Hosts[0].Fingerprints[0] != fingerprint {
		t.Fatalf("the answer shows %+v", state.SSH)
	}
	setupRes := s.do(t, http.MethodGet, "/setup/integrations/ssh/hosts/api-1/setup", "", nil)
	if setupRes.Code != http.StatusOK || !strings.Contains(setupRes.Body.String(), "HOST_ID=api-1") ||
		!strings.Contains(setupRes.Body.String(), strings.Fields(ca.PublicLine())[1]) {
		t.Errorf("the server setup = %d: %.300s", setupRes.Code, setupRes.Body)
	}
	if _, err := s.config.Apply(t.Context(), configapi.ApplyRequest{
		Patch: []byte(`{"roles": [{"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu",
			"integrations": {"ssh": {"enabled": true, "hosts": ["api-1"]}}}, {"name": "CTO", "handle": "cto", "llm": "zulu"}]}`),
		Summary: "grant api-1", By: iam.Actor{Name: "jane-doe", Kind: iam.ActorHuman},
	}); err != nil {
		t.Fatal(err)
	}
	if res := s.do(t, http.MethodDelete, "/setup/integrations/ssh/hosts/api-1", "", nil); res.Code != http.StatusConflict ||
		!strings.Contains(res.Body.String(), "granted to") || !strings.Contains(res.Body.String(), "sre-lead") {
		t.Errorf("removing a granted server = %d: %s", res.Code, res.Body)
	}
	if res := s.do(t, http.MethodDelete, "/setup/integrations/ssh/hosts/db-9", "", nil); res.Code != http.StatusNotFound {
		t.Errorf("removing a server nobody registered = %d", res.Code)
	}
}

// A SCAN IS HELD TO THE SERVER ADDRESS RULE before it dials: the engine host
// and a link-local address are refused at the field, and nothing is dialled.
func TestAnSSHScanOfTheEngineHostIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	s.connect(t, "ssh", `{"values": {"enabled": "true"}, "generate": ["certificate_authority"]}`)
	for _, address := range []string{"127.0.0.1", "169.254.169.254", "localhost", "", "ssh://api-1.example.com"} {
		res := s.do(t, http.MethodPost, "/setup/integrations/ssh/hosts/scan", `{"address": "`+address+`", "port": 22}`, nil)
		if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"field":"address"`) {
			t.Errorf("a scan of %q = %d: %s", address, res.Code, res.Body)
		}
	}
}
