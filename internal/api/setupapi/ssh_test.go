package setupapi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
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
	if state.SSH == nil || state.SSH.CAPublicKey != ca.PublicLine() || state.SSH.SetupVersion != sshint.SetupVersion ||
		len(state.SSH.Hosts) != 1 || state.SSH.Hosts[0].ID != "api-1" || state.SSH.Hosts[0].Port != 22 ||
		len(state.SSH.Hosts[0].Fingerprints) != 1 || state.SSH.Hosts[0].Fingerprints[0] != fingerprint {
		t.Fatalf("the answer shows %+v", state.SSH)
	}
	// THE INVENTORY IS SWITCHED BY ITS OWN ROUTE, which changes that one
	// field: the server's keys, users and description stand, the answer
	// names the commands before and after, and the setup then carries it.
	if len(state.SSH.InventoryCommands) != 2 || state.SSH.InventoryCommands[0].Command != config.SSHInventoryPorts ||
		state.SSH.InventoryCommands[0].Containers || !state.SSH.InventoryCommands[1].Containers ||
		state.SSH.Hosts[0].Inventory {
		t.Fatalf("before it is switched on the answer shows %+v", state.SSH)
	}
	if res := s.do(t, http.MethodPut, "/setup/integrations/ssh/hosts/db-9/inventory", `{"inventory": true}`, nil); res.Code != http.StatusNotFound {
		t.Errorf("the inventory of a server nobody registered = %d", res.Code)
	}
	for _, on := range []bool{true, false, true} {
		body := fmt.Sprintf(`{"inventory": %t}`, on)
		if res := s.do(t, http.MethodPut, "/setup/integrations/ssh/hosts/api-1/inventory", body, nil); res.Code != http.StatusOK {
			t.Fatalf("switch the inventory to %t = %d: %s", on, res.Code, res.Body)
		}
		res := s.do(t, http.MethodGet, "/setup/integrations/ssh", "", nil)
		var after setupapi.ToolState
		if err := json.Unmarshal(res.Body.Bytes(), &after); err != nil {
			t.Fatal(err)
		}
		if h := after.SSH.Hosts[0]; h.Inventory != on || h.Description != "Production API server" ||
			h.Users.Operate != "crewlet-ops" || len(h.Fingerprints) != 1 || h.Fingerprints[0] != fingerprint {
			t.Fatalf("after switching the inventory to %t the server is %+v", on, h)
		}
	}
	setupRes := s.do(t, http.MethodGet, "/setup/integrations/ssh/hosts/api-1/setup", "", nil)
	if !strings.Contains(setupRes.Body.String(), "inventory-allowlist") ||
		!strings.Contains(setupRes.Body.String(), `crewlet.%s.inventory`) {
		t.Errorf("the setup of a server that opts in lacks the inventory: %.300s", setupRes.Body)
	}
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
	mu            sync.Mutex
	steps         []sshint.TestStep
	asked         []sshint.TestInput
	verdictsAsked []accessdomain.VerdictQuery
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

// Verdicts answers two verdicts, the second of which does not open, and
// records the query it was asked.
func (f *fakeSSHAccess) Verdicts(_ context.Context, q accessdomain.VerdictQuery,
	_ statelog.Freshness) (accessdomain.VerdictPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verdictsAsked = append(f.verdictsAsked, q)
	return accessdomain.VerdictPage{Rows: []accessdomain.VerdictRow{{Handle: "sre", ID: "v1",
		Request: "call-1", Kind: delegated.KindSSHOperate, Rule: 1, By: "reviewer",
		ByKind: accessdomain.VerdictByAgent, Through: accessdomain.VerdictThroughDelegation,
		Asked: delegated.Allow, Confidence: delegated.Low,
		Verdict: delegated.Escalate, Overruled: []string{"low_confidence"}, Final: true,
		Host: "api-1", WordsSealed: "sealed"}, {Handle: "sre", ID: "v2", Request: "call-2",
		Kind: delegated.KindSSHOperate, ByKind: accessdomain.VerdictByEngine,
		Verdict: delegated.Escalate, Overruled: []string{"deadline"}}},
		Next: "17", Complete: true}, nil
}

func (*fakeSSHAccess) OpenVerdict(row accessdomain.VerdictRow) (string, string, []string, error) {
	if row.ID != "v1" {
		return "", "", nil, accessdomain.ErrUnreadable
	}
	return "systemctl restart api", "the item names this service", []string{"no sudo"}, nil
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

// THE AUDIT ANSWERS VERDICTS WITH THEIR FILTERS: kind=verdict hands the
// reader every filter the query names, the verdict's own words included, and
// answers each row's fields with what it decided and its giver's words opened,
// or says a row did not open rather than leaving it out; a verdict filter on
// another kind, and a flag that is not true or false, are refused naming the
// field; and the page's bound is the reader's own, so every limit the route
// admits is one the reader answers.
//
// Mutation: bound the limit at 200 again and a limit of MaxPageSize+1 reaches
// the reader; drop verdictFilters and a command page answered verdict=deny
// unnarrowed.
func TestTheSSHAuditAnswersVerdictsWithTheirFilters(t *testing.T) {
	t.Parallel()
	access := &fakeSSHAccess{}
	s := newSurfaceWithSSH(t, access, coordmemory.NewFleet())
	s.seed(t)
	limit := strconv.Itoa(accessdomain.MaxPageSize)
	res := s.do(t, http.MethodGet, "/setup/integrations/ssh/audit?kind=verdict&seat=sre&host=api-1"+
		"&by=reviewer&request=call-1&verdict=escalate&by_kind=agent&delegation_kind=ssh_operate"+
		"&final=true&sampled=false&since=2026-11-08T09:00:00Z&cursor=40&limit="+limit, "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("the verdict audit = %d: %s", res.Code, res.Body)
	}
	yes, no := true, false
	want := accessdomain.VerdictQuery{Handle: "sre", Host: "api-1", By: "reviewer", Request: "call-1",
		Verdict: delegated.Escalate, ByKind: accessdomain.VerdictByAgent,
		Kind: delegated.KindSSHOperate, Final: &yes, Sampled: &no,
		Since: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC), Cursor: "40", Limit: accessdomain.MaxPageSize}
	if len(access.verdictsAsked) != 1 || fmt.Sprintf("%+v", deref(access.verdictsAsked[0])) != fmt.Sprintf("%+v", deref(want)) {
		t.Fatalf("the reader was asked %+v, want %+v", access.verdictsAsked, deref(want))
	}
	var page struct {
		Kind     string           `json:"kind"`
		Rows     []map[string]any `json:"rows"`
		Next     string           `json:"next"`
		Complete bool             `json:"complete"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Kind != "verdict" || len(page.Rows) != 2 || page.Next != "17" || !page.Complete {
		t.Fatalf("the page is %s", res.Body)
	}
	opened, unopened := page.Rows[0], page.Rows[1]
	for field, value := range map[string]any{"seat": "sre", "request": "call-1", "kind": "ssh_operate",
		"rule": float64(1), "by": "reviewer", "by_kind": "agent", "through": "delegation", "asked": "allow",
		"confidence": "low", "verdict": "escalate", "final": true, "sampled": false, "host": "api-1",
		"subject": "systemctl restart api", "reason": "the item names this service"} {
		if opened[field] != value {
			t.Errorf("the verdict's %s is %v, want %v", field, opened[field], value)
		}
	}
	if fmt.Sprint(opened["overruled"]) != "[low_confidence]" || fmt.Sprint(opened["findings"]) != "[no sudo]" {
		t.Errorf("the verdict's lists are %v and %v", opened["overruled"], opened["findings"])
	}
	for _, field := range []string{"seat", "request", "kind", "rule", "turn", "by", "by_kind", "through",
		"asked", "confidence", "verdict", "final", "overruled", "sampled", "digest", "host", "user",
		"stdin_bytes", "stdin_sha256", "decision", "item", "at"} {
		if _, ok := unopened[field]; !ok {
			t.Errorf("a verdict that did not open answers no %s", field)
		}
	}
	if _, ok := unopened["subject"]; ok || unopened["subject_unopened"] == nil {
		t.Errorf("a verdict that did not open answers %v", unopened)
	}
	for _, refused := range []struct{ query, field string }{
		{"kind=command&verdict=deny", "verdict"},
		{"by_kind=engine", "by_kind"},
		{"kind=certificate&delegation_kind=ssh_operate", "delegation_kind"},
		{"kind=verdict&final=yes", "final"},
		{"kind=verdict&limit=" + strconv.Itoa(accessdomain.MaxPageSize+1), "limit"},
		{"kind=decision", "kind"},
	} {
		res := s.do(t, http.MethodGet, "/setup/integrations/ssh/audit?"+refused.query, "", nil)
		if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"field":"`+refused.field+`"`) {
			t.Errorf("%s = %d: %s; want 400 naming %s", refused.query, res.Code, res.Body, refused.field)
		}
	}
	if len(access.verdictsAsked) != 1 {
		t.Errorf("a refused audit reached the reader: %+v", access.verdictsAsked[1:])
	}
}

// deref is a query with its flags as values, for a comparison by value.
func deref(q accessdomain.VerdictQuery) any {
	flag := func(b *bool) string {
		if b == nil {
			return "unset"
		}
		return strconv.FormatBool(*b)
	}
	out := q
	out.Final, out.Sampled = nil, nil
	return struct {
		Query          accessdomain.VerdictQuery
		Final, Sampled string
	}{out, flag(q.Final), flag(q.Sampled)}
}

// newSurfaceWithSSH is the fixture surface over access and tests.
func newSurfaceWithSSH(t *testing.T, access setupapi.SSHAccess, tests setupapi.SSHTests) *surface {
	t.Helper()
	fixture := newConfigSurface(t)
	v := &vault{}
	var ticks atomic.Int64
	s := &surface{
		mux: http.NewServeMux(), config: fixture.svc, vault: v, configs: fixture.db.Configs(),
		cipher: fixture.cipher, plane: fixture.plane, gate: fixture.gate,
		status: &statusStore{}, webhookBase: fixtureWebhookBase, webhookBaseKey: oneListener,
		access: &fakeAccess{},
	}
	s.setup = newService(t, setupapi.Options{
		Config: fixture.svc, Secrets: v, Access: s.access, Resolve: v.get, Status: s.status,
		WebhookBase: fixtureWebhookBase, WebhookBaseKey: oneListener, SSH: access, SSHTests: tests,
		// A SECOND A READING, so each press is later than the one before.
		Now: func() time.Time { return pinned.Add(time.Duration(ticks.Add(1)) * time.Second) },
	})
	mount(t, s.mux, s.setup, fixture.svc)
	return s
}

// seatedAdmin is a person holding seat, an administrator, freshly proved.
func seatedAdmin(seat string) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: seat + ".person", Kind: iam.KindPerson, Seat: seat,
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour)}
}

// A ROTATION STAGES, PROMOTES ONLY ONCE EVERY SERVER PASSED AS IT IS NOW, AND
// LEAVES NO STRAY KEY. Stage seals a second authority the setup trusts beside
// the current one, answers the key the store holds once its revision landed
// (whichever stage sealed it), and is refused while one is staged. Promote is
// refused until every server's latest test with the staged authority passed
// on the server as it is now (a test from before the server changed counts
// for nothing), then seals the staged key under the current name, removes the
// staged copy, keeps the retired line for the answer to list, and clears the
// reference; a promote run again over a key already moved names no retired
// line. Cancel removes the staged key too.
func TestARotationPromotesOnlyOnceEveryServerPassedWithTheStagedAuthority(t *testing.T) {
	t.Parallel()
	access := &fakeSSHAccess{steps: []sshint.TestStep{{ID: sshint.StepReach, OK: false, Detail: "Not reached."}}}
	s := newSurfaceWithSSH(t, access, coordmemory.NewFleet())
	s.seed(t)
	s.connect(t, "ssh", `{"values": {"enabled": "true"}, "generate": ["certificate_authority"]}`)
	line, _ := sshHostKeyLine(t)
	putHost := func(address string) {
		t.Helper()
		host := `{"address": "` + address + `", "host_keys": ["` + line + `"], "users": {"read": "crewlet-read"}}`
		if res := s.do(t, http.MethodPut, "/setup/integrations/ssh/hosts/api-1", host, nil); res.Code != http.StatusOK {
			t.Fatalf("put the server = %d: %s", res.Code, res.Body)
		}
	}
	putHost("api-1.example.com")
	rotate := func(action string) *httptest.ResponseRecorder {
		return s.do(t, http.MethodPost, "/setup/integrations/ssh/rotation", `{"action": "`+action+`"}`, nil)
	}
	press := func(passes bool) {
		t.Helper()
		access.mu.Lock()
		access.steps[0].OK = passes
		access.mu.Unlock()
		if res := s.doAs(t, seatedAdmin("ops-lead"), http.MethodPost, "/setup/integrations/ssh/hosts/api-1/test",
			`{"ca": "next"}`, nil); res.Code != http.StatusOK {
			t.Fatalf("test with the staged authority = %d: %s", res.Code, res.Body)
		}
	}
	parse := func(name string) (string, *setup.SSHCertificateAuthority) {
		t.Helper()
		value, ok := s.vault.get(name)
		ca, err := setup.ParseSSHCertificateAuthority(value)
		if !ok || err != nil {
			t.Fatalf("%s holds no authority: %v", name, err)
		}
		return value, ca
	}
	_, old := parse("SSH_CERTIFICATE_AUTHORITY")
	for _, action := range []string{"promote", "cancel"} {
		if res := rotate(action); res.Code != http.StatusConflict {
			t.Errorf("%s with nothing staged = %d: %s", action, res.Code, res.Body)
		}
	}
	// ANOTHER STAGE'S KEY WINS THE STORE: the answer names the sealed one.
	winner, err := setup.GenerateSSHCertificateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	s.vault.override("SSH_CERTIFICATE_AUTHORITY_NEXT", winner)
	res := rotate("stage")
	nextValue, next := parse("SSH_CERTIFICATE_AUTHORITY_NEXT")
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), strings.Fields(next.PublicLine())[1]) ||
		strings.Contains(res.Body.String(), "PRIVATE KEY") {
		t.Fatalf("stage = %d: %s, want the sealed key's line %s", res.Code, res.Body, next.PublicLine())
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
	press(false)
	if res := rotate("promote"); res.Code != http.StatusConflict {
		t.Fatalf("promote after a failed test = %d: %s", res.Code, res.Body)
	}
	press(true)
	putHost("api-1b.example.com")
	if res := rotate("promote"); res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "api-1") {
		t.Fatalf("promote on a test from before the server changed = %d: %s", res.Code, res.Body)
	}
	press(true)
	res = rotate("promote")
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), strings.Fields(old.PublicLine())[1]) {
		t.Fatalf("promote = %d: %s", res.Code, res.Body)
	}
	if current, _ := s.vault.get("SSH_CERTIFICATE_AUTHORITY"); current != nextValue {
		t.Fatal("promote did not seal the staged authority as the current one")
	}
	if _, left := s.vault.get("SSH_CERTIFICATE_AUTHORITY_NEXT"); left {
		t.Fatal("promote left the staged key sealed beside the current one")
	}
	if doc := s.do(t, http.MethodGet, "/config", "", nil).Body.String(); strings.Contains(doc, "SSH_CERTIFICATE_AUTHORITY_NEXT") {
		t.Fatal("the document still points at a staged authority after promote")
	}
	var state setupapi.ToolState
	if err := json.Unmarshal(s.do(t, http.MethodGet, "/setup/integrations/ssh", "", nil).Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.SSH == nil || len(state.SSH.RetiredCAPublicKeys) != 1 ||
		strings.Fields(state.SSH.RetiredCAPublicKeys[0])[1] != strings.Fields(old.PublicLine())[1] {
		t.Fatalf("the answer lists the retired lines %+v, want the old authority's", state.SSH)
	}

	// A PROMOTE THAT MOVED THE KEY AND STOPPED, run again, retires nothing.
	if res := rotate("stage"); res.Code != http.StatusOK {
		t.Fatalf("stage after promote = %d: %s", res.Code, res.Body)
	}
	press(true)
	staged, _ := s.vault.get("SSH_CERTIFICATE_AUTHORITY_NEXT")
	if err := s.vault.Set(t.Context(), "SSH_CERTIFICATE_AUTHORITY", staged, secrets.Author{Name: "jane-doe"},
		secrets.SourceSetup, time.Now()); err != nil {
		t.Fatal(err)
	}
	if res := rotate("promote"); res.Code != http.StatusOK || strings.Contains(res.Body.String(), "retired_ca_public_key") {
		t.Fatalf("a promote run again = %d: %s, want no retired line", res.Code, res.Body)
	}

	if res := rotate("stage"); res.Code != http.StatusOK {
		t.Fatalf("stage = %d: %s", res.Code, res.Body)
	}
	if res := rotate("cancel"); res.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", res.Code, res.Body)
	}
	if _, left := s.vault.get("SSH_CERTIFICATE_AUTHORITY_NEXT"); left {
		t.Fatal("cancel left the cancelled authority's key sealed")
	}
	if res := rotate("rotate"); res.Code != http.StatusBadRequest {
		t.Errorf("an unknown action = %d: %s", res.Code, res.Body)
	}
}

// THE AUDIT'S PAGE IS THE READER'S OWN: the route admits every limit up to the
// access log's page and refuses the first past it, naming the field, rather
// than admitting a page the reader then refuses as an error of its own.
//
// Mutation: bound the route at 200 again, and a limit one past the reader's
// page is admitted.
func TestTheSSHAuditsPageIsTheReadersOwn(t *testing.T) {
	t.Parallel()
	s := newSurfaceWithSSH(t, &fakeSSHAccess{}, coordmemory.NewFleet())
	s.seed(t)
	at := func(limit int) *httptest.ResponseRecorder {
		return s.do(t, http.MethodGet, "/setup/integrations/ssh/audit?kind=command&limit="+strconv.Itoa(limit), "", nil)
	}
	if res := at(accessdomain.MaxPageSize); res.Code != http.StatusOK {
		t.Errorf("a page of the reader's own size = %d: %s", res.Code, res.Body)
	}
	if res := at(accessdomain.MaxPageSize + 1); res.Code != http.StatusBadRequest ||
		!strings.Contains(res.Body.String(), `"field":"limit"`) {
		t.Errorf("a page past the reader's = %d: %s", res.Code, res.Body)
	}
}
