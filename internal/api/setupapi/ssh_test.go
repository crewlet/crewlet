package setupapi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/setup"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/statelog"
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

// fakeSSHAccess answers Test connection with steps a case scripts, and records
// what it was asked.
type fakeSSHAccess struct {
	mu    sync.Mutex
	steps []sshint.TestStep
	asked []sshint.TestInput
}

func (f *fakeSSHAccess) TestConnection(_ context.Context, in sshint.TestInput) []sshint.TestStep {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, in)
	return f.steps
}

func (*fakeSSHAccess) Certificates(context.Context, accessdomain.AuditQuery,
	statelog.Freshness) (accessdomain.CertificatePage, error) {
	return accessdomain.CertificatePage{Rows: []accessdomain.CertificateRow{{Handle: "sre", Serial: "00000000000000a1",
		KeyID: "crewlet/sre/turn-1", Purpose: accessdomain.PurposeRead, HostIDs: []string{"api-1"}}}}, nil
}

func (*fakeSSHAccess) Commands(context.Context, accessdomain.AuditQuery,
	statelog.Freshness) (accessdomain.CommandPage, error) {
	return accessdomain.CommandPage{Rows: []accessdomain.CommandRow{{Handle: "sre", ID: "c1", Host: "api-1",
		Level: accessdomain.LevelOperate, CommandSealed: "sealed"}}}, nil
}

func (*fakeSSHAccess) OpenCommand(accessdomain.CommandRow) (string, error) {
	return "systemctl restart api", nil
}

// TEST CONNECTION IS KEPT AS THE SERVER'S LATEST TEST WITH ITS AUTHORITY: the
// press signs with the authority it names, as the person's seat, and the
// result lands in the fleet's record the answer and a promote read; a staged
// authority that does not exist, and a caller acting as no seat, are refused
// before anything is signed.
func TestTestConnectionIsKeptAsTheServersLatestTest(t *testing.T) {
	t.Parallel()
	access := &fakeSSHAccess{steps: []sshint.TestStep{{ID: sshint.StepReach, OK: true, Detail: "Reached."},
		{ID: sshint.StepHostKey, OK: false, Detail: "Not pinned."}}}
	tests := coordmemory.NewFleet()
	s := newSurfaceWithSSH(t, access, tests)
	s.seed(t)
	s.connect(t, "ssh", `{"values": {"enabled": "true"}, "generate": ["certificate_authority"]}`)
	line, _ := sshHostKeyLine(t)
	host := `{"address": "api-1.example.com", "host_keys": ["` + line + `"], "users": {"read": "crewlet-read"}}`
	if res := s.do(t, http.MethodPut, "/setup/integrations/ssh/hosts/api-1", host, nil); res.Code != http.StatusOK {
		t.Fatalf("add the server = %d: %s", res.Code, res.Body)
	}
	if res := s.do(t, http.MethodPost, "/setup/integrations/ssh/hosts/api-1/test", `{"ca": "next"}`, nil); res.Code != http.StatusConflict {
		t.Errorf("a test with no staged authority = %d: %s", res.Code, res.Body)
	}
	if len(access.asked) != 0 {
		t.Fatalf("a refused test was run: %+v", access.asked)
	}
	res := s.doAs(t, seatedAdmin("ops-lead"), http.MethodPost, "/setup/integrations/ssh/hosts/api-1/test", `{}`, nil)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"ok":false`) {
		t.Fatalf("the test = %d: %s", res.Code, res.Body)
	}
	if len(access.asked) != 1 || access.asked[0].Person != "ops-lead" || access.asked[0].Host.ID != "api-1" {
		t.Fatalf("the engine was asked %+v", access.asked)
	}
	kept, err := tests.ListSSHTests(t.Context())
	if err != nil || len(kept) != 1 || kept[0].Host != "api-1" || kept[0].OK || len(kept[0].Steps) != 2 ||
		kept[0].Authority != access.asked[0].CA.Fingerprint() {
		t.Fatalf("kept %+v, %v", kept, err)
	}
	audit := s.do(t, http.MethodGet, "/setup/integrations/ssh/audit?kind=command", "", nil)
	if audit.Code != http.StatusOK || !strings.Contains(audit.Body.String(), `"command":"systemctl restart api"`) {
		t.Errorf("the command audit = %d: %s", audit.Code, audit.Body)
	}
}

// newSurfaceWithSSH is the fixture surface over access and tests.
func newSurfaceWithSSH(t *testing.T, access setupapi.SSHAccess, tests setupapi.SSHTests) *surface {
	t.Helper()
	fixture := newConfigSurface(t)
	v := &vault{}
	s := &surface{
		mux: http.NewServeMux(), config: fixture.svc, vault: v, configs: fixture.db.Configs(),
		cipher: fixture.cipher, plane: fixture.plane, gate: fixture.gate,
		status: &statusStore{}, webhookBase: fixtureWebhookBase, webhookBaseKey: oneListener,
		access: &fakeAccess{},
	}
	s.setup = newService(t, setupapi.Options{
		Config: fixture.svc, Secrets: v, Access: s.access, Resolve: v.get, Status: s.status,
		WebhookBase: fixtureWebhookBase, WebhookBaseKey: oneListener, SSH: access, SSHTests: tests,
		Now: func() time.Time { return pinned },
	})
	mount(t, s.mux, s.setup, fixture.svc)
	return s
}

// seatedAdmin is a person holding seat, an administrator, freshly proved.
func seatedAdmin(seat string) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: seat + ".person", Kind: iam.KindPerson, Seat: seat,
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour)}
}

// A ROTATION STAGES, PROMOTES ONLY ONCE EVERY SERVER PASSED, AND CANCELS: stage
// seals a second authority the setup trusts beside the current one and is
// refused while one is staged; promote is refused until every server's latest
// test with the staged authority passed, then seals it under the current name
// and clears the staged reference, answering the retired line; cancel drops a
// staged one, and is refused with none staged.
func TestARotationPromotesOnlyOnceEveryServerPassedWithTheStagedAuthority(t *testing.T) {
	t.Parallel()
	tests := coordmemory.NewFleet()
	s := newSurfaceWithSSH(t, &fakeSSHAccess{}, tests)
	s.seed(t)
	s.connect(t, "ssh", `{"values": {"enabled": "true"}, "generate": ["certificate_authority"]}`)
	line, _ := sshHostKeyLine(t)
	host := `{"address": "api-1.example.com", "host_keys": ["` + line + `"], "users": {"read": "crewlet-read"}}`
	if res := s.do(t, http.MethodPut, "/setup/integrations/ssh/hosts/api-1", host, nil); res.Code != http.StatusOK {
		t.Fatalf("add the server = %d: %s", res.Code, res.Body)
	}
	rotate := func(action string) *httptest.ResponseRecorder {
		return s.do(t, http.MethodPost, "/setup/integrations/ssh/rotation", `{"action": "`+action+`"}`, nil)
	}
	oldValue, _ := s.vault.get("SSH_CERTIFICATE_AUTHORITY")
	old, err := setup.ParseSSHCertificateAuthority(oldValue)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"promote", "cancel"} {
		if res := rotate(action); res.Code != http.StatusConflict {
			t.Errorf("%s with nothing staged = %d: %s", action, res.Code, res.Body)
		}
	}
	if res := rotate("stage"); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "next_ca_public_key") ||
		strings.Contains(res.Body.String(), "PRIVATE KEY") {
		t.Fatalf("stage = %d: %s", res.Code, res.Body)
	}
	nextValue, ok := s.vault.get("SSH_CERTIFICATE_AUTHORITY_NEXT")
	next, err := setup.ParseSSHCertificateAuthority(nextValue)
	if !ok || err != nil {
		t.Fatalf("nothing usable was staged: %v", err)
	}
	if res := rotate("stage"); res.Code != http.StatusConflict {
		t.Errorf("a second stage = %d: %s", res.Code, res.Body)
	}
	script := s.do(t, http.MethodGet, "/setup/integrations/ssh/hosts/api-1/setup", "", nil).Body.String()
	for _, ca := range []*setup.SSHCertificateAuthority{old, next} {
		if !strings.Contains(script, strings.Fields(ca.PublicLine())[1]) {
			t.Errorf("the setup while staged does not trust %s", ca.Fingerprint())
		}
	}
	if res := rotate("promote"); res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "api-1") {
		t.Fatalf("promote before any test = %d: %s", res.Code, res.Body)
	}
	for _, passed := range []bool{false, true} {
		if _, err := tests.PutSSHTest(t.Context(), coord.SSHTest{Host: "api-1", Authority: next.Fingerprint(),
			At: time.Now(), By: "ops-lead", ByKind: iam.ActorHuman, OK: passed,
			Steps: []coord.SSHTestStep{{ID: sshint.StepReach, OK: passed, Detail: "Reached."}}}); err != nil {
			t.Fatal(err)
		}
		res := rotate("promote")
		if !passed {
			if res.Code != http.StatusConflict {
				t.Fatalf("promote after a failed test = %d: %s", res.Code, res.Body)
			}
			continue
		}
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), old.PublicLine()) {
			t.Fatalf("promote = %d: %s", res.Code, res.Body)
		}
	}
	if current, _ := s.vault.get("SSH_CERTIFICATE_AUTHORITY"); current != nextValue {
		t.Fatal("promote did not seal the staged authority as the current one")
	}
	if doc := s.do(t, http.MethodGet, "/config", "", nil).Body.String(); strings.Contains(doc, "SSH_CERTIFICATE_AUTHORITY_NEXT") {
		t.Fatal("the document still points at a staged authority after promote")
	}
	if res := rotate("stage"); res.Code != http.StatusOK {
		t.Fatalf("stage after promote = %d: %s", res.Code, res.Body)
	}
	if res := rotate("cancel"); res.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", res.Code, res.Body)
	}
	if res := rotate("rotate"); res.Code != http.StatusBadRequest {
		t.Errorf("an unknown action = %d: %s", res.Code, res.Body)
	}
}
