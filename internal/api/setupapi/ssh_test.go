package setupapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/api/setupapi"
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
