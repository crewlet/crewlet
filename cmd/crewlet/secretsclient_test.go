package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// bootWithAPI builds a Tier A carrying an HTTP surface and one token.
func bootWithAPI(t *testing.T, host string, port int, token string) *config.Bootstrap {
	t.Helper()
	boot := config.DefaultBootstrap()
	boot.API.Host = host
	boot.API.Port = port
	if token != "" {
		boot.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: token}}
	}
	return &boot
}

// A BIND ADDRESS IS NOT ALWAYS A DESTINATION.
//
// Tier A carries what the node BINDS, and 0.0.0.0 means "every interface",
// which as a target means nothing at all. A client that dialled it verbatim
// would fail on every node configured the way a container image configures
// one — which is to say, most of them.
func TestAWildcardBindResolvesToLoopback(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"", "0.0.0.0", "::", "[::]"} {
		client, err := newSecretsClient(bootWithAPI(t, host, 9090, "t"), "")
		if err != nil {
			t.Fatalf("host %q: %v", host, err)
		}
		if client.base != "http://127.0.0.1:9090" {
			t.Errorf("host %q resolved to %q, want loopback", host, client.base)
		}
	}
}

// AN IPv6 LITERAL KEEPS ITS BRACKETS. Without them the port reads as part of
// the address and every request goes nowhere, with a parse error that names
// neither.
func TestAnIPv6HostIsBracketed(t *testing.T) {
	t.Parallel()
	client, err := newSecretsClient(bootWithAPI(t, "fd00::1", 9090, "t"), "")
	if err != nil {
		t.Fatal(err)
	}
	if client.base != "http://[fd00::1]:9090" {
		t.Errorf("base = %q, want a bracketed literal", client.base)
	}
}

// A NODE WITH NO HTTP SURFACE IS REFUSED BY NAME.
//
// api.port 0 is a real posture — it serves no dashboard, no REST, no webhooks
// — and it is also the one shape where a running engine cannot be written
// through at all. "connection refused to :0" would send an operator looking
// for a network fault.
func TestANodeWithNoHTTPSurfaceIsRefusedByName(t *testing.T) {
	t.Parallel()
	_, err := newSecretsClient(bootWithAPI(t, "127.0.0.1", 0, "t"), "")
	if err == nil {
		t.Fatal("a node with api.port 0 was accepted as a write target")
	}
	if !strings.Contains(err.Error(), "api.port") {
		t.Errorf("the refusal does not name the setting to change: %v", err)
	}
}

// A NODE WITH NO TOKENS IS REFUSED BEFORE THE REQUEST, not after a 401.
//
// The guard would answer 401 either way; saying it here names the two ways
// out — add a token to Tier A, or export one — which a bare 401 cannot.
func TestANodeWithNoTokensSaysHowToAuthenticate(t *testing.T) {
	t.Parallel()
	_, err := newSecretsClient(bootWithAPI(t, "127.0.0.1", 8080, ""), "")
	if err == nil {
		t.Fatal("a node with no api.auth.tokens was accepted")
	}
	for _, want := range []string{"api.auth.tokens", apiTokenEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits %q: %v", want, err)
		}
	}
}

// THE ENVIRONMENT WINS OVER TIER A, because the token decides ATTRIBUTION.
// Every entry in the list authenticates, but the id is stamped as the author
// of the write, so an operator who wants their own name on a rotation has to
// be able to supply their own credential.
func TestTheEnvironmentTokenWinsOverTierA(t *testing.T) {
	t.Setenv(apiTokenEnv, "mine")
	client, err := newSecretsClient(bootWithAPI(t, "127.0.0.1", 8080, "shared"), "")
	if err != nil {
		t.Fatal(err)
	}
	if client.token != "mine" {
		t.Errorf("token = %q, want the one from the environment", client.token)
	}
}

// AUTH DISABLED NEEDS NO TOKEN. Refusing here would make the local-dev escape
// hatch the one posture this command cannot talk to.
func TestADisabledGuardNeedsNoToken(t *testing.T) {
	t.Parallel()
	boot := bootWithAPI(t, "127.0.0.1", 8080, "")
	boot.API.Auth.Disabled = true
	client, err := newSecretsClient(boot, "")
	if err != nil {
		t.Fatalf("a node with auth disabled was refused: %v", err)
	}
	if client.token != "" {
		t.Errorf("token = %q, want none", client.token)
	}
}

// fakeSecretsNode is a stand-in for a running node's /secrets surface.
type fakeSecretsNode struct {
	t      *testing.T
	server *httptest.Server

	last   secretsRequest
	status int
	body   string
}

type secretsRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   string
}

func newFakeSecretsNode(t *testing.T) *fakeSecretsNode {
	t.Helper()
	n := &fakeSecretsNode{t: t}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		n.last = secretsRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), body: string(raw),
		}
		status, body := n.status, n.body
		if status == 0 {
			status = http.StatusOK
		}
		if body == "" {
			body = "{}"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(n.server.Close)
	return n
}

func (n *fakeSecretsNode) client(t *testing.T) *secretsClient {
	t.Helper()
	c, err := newSecretsClient(bootWithAPI(t, "127.0.0.1", 1, "ops-token"), n.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// THE VALUE IS THE BODY, verbatim.
//
// A credential is arbitrary bytes — a PEM key has newlines, a token can hold
// anything — and wrapping it in JSON puts an encoding step between the
// operator and the byte sequence the vendor will compare. A value that came
// back re-encoded would fail at the vendor with a 401 that names neither.
func TestSettingASecretSendsTheRawValue(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	value := "-----BEGIN KEY-----\nline two\n\ttabbed \n"

	if err := node.client(t).Set(t.Context(), "GL_TOKEN", value, "ignored", "cli", time.Now()); err != nil {
		t.Fatalf("set: %v", err)
	}
	if node.last.body != value {
		t.Errorf("the node received %q, want the value byte for byte", node.last.body)
	}
	if node.last.method != http.MethodPut || node.last.path != "/secrets/GL_TOKEN" {
		t.Errorf("%s %s, want PUT /secrets/GL_TOKEN", node.last.method, node.last.path)
	}
	if node.last.query != "source=cli" {
		t.Errorf("query = %q, want the provenance to travel", node.last.query)
	}
	if node.last.auth != "Bearer ops-token" {
		t.Errorf("auth = %q, want the bearer token", node.last.auth)
	}
}

// A NAME WITH A SLASH IN IT STILL ADDRESSES ONE ROW.
//
// ${VAR} names are conventionally uppercase words, but nothing refuses an odd
// one, and an unescaped slash would silently address a different path — which
// on this surface is a write that goes somewhere else entirely.
func TestAnAwkwardNameIsEscapedIntoThePath(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	if err := node.client(t).Set(t.Context(), "a/b c", "v", "", "", time.Now()); err != nil {
		t.Fatalf("set: %v", err)
	}
	if node.last.path != "/secrets/a/b c" {
		t.Errorf("path = %q, want the name escaped rather than split", node.last.path)
	}
}

// A MISSING SECRET COMES BACK AS THE SENTINEL, not as a generic refusal.
//
// The provisioning sink treats absence as "mint one" and any other error as
// "stop"; collapsing the two would have a node it cannot reach look exactly
// like a credential nobody set, and the run would rotate the lot.
func TestAMissingSecretIsTheNotFoundSentinel(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.status, node.body = http.StatusNotFound, `{"error":"not_found"}`

	_, err := node.client(t).Get(t.Context(), "ABSENT")
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("err = %v, want secrets.ErrNotFound", err)
	}
}

// A 404 WITH NO not_found BODY IS AN OLD NODE, not a missing secret.
//
// A binary from before secrets moved onto the fleet serves no /secrets at
// all, and reporting that as "no such secret" would have an operator set a
// value over and over against a node that will never hold it.
func TestA404WithoutTheBodyIsReportedAsAMissingSurface(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.status, node.body = http.StatusNotFound, `{}`

	_, err := node.client(t).Get(t.Context(), "TOKEN")
	if errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("a node with no /secrets surface was reported as a missing "+
			"secret: %v", err)
	}
	if !strings.Contains(err.Error(), "/secrets") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// A REJECTED TOKEN NAMES THE VARIABLE THAT FIXES IT.
func TestARejectedTokenSaysWhatToSet(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.status = http.StatusUnauthorized

	err := node.client(t).Set(t.Context(), "T", "v", "", "", time.Now())
	if err == nil || !strings.Contains(err.Error(), apiTokenEnv) {
		t.Fatalf("err = %v, want it to name %s", err, apiTokenEnv)
	}
}

// THE NODE'S HINT REACHES THE OPERATOR. A refusal this client swallowed would
// leave a 503 with no reason anywhere the person running the command can see.
func TestTheNodesHintIsCarriedIntoTheError(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.status = http.StatusServiceUnavailable
	node.body = `{"error":"no_keyring","hint":"run crewlet secrets keygen"}`

	err := node.client(t).Set(t.Context(), "T", "v", "", "", time.Now())
	if err == nil {
		t.Fatal("a 503 was accepted as success")
	}
	for _, want := range []string{"no_keyring", "crewlet secrets keygen"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error omits %q: %v", want, err)
		}
	}
}

// THE REKEY KEY ID TRAVELS, so the node can refuse a mismatch. A CLI whose
// Tier A names a different active key than the node's is rekeying onto a key
// the fleet will not seal with, and silent success there reports a rotation
// that did not happen.
func TestRekeySendsTheKeyIDItExpects(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.body = `{"moved":["A","B"]}`

	moved, err := node.client(t).Rekey(t.Context(), "key-2", "op", time.Now())
	if err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if node.last.query != "key_id=key-2" {
		t.Errorf("query = %q, want the expected key to travel", node.last.query)
	}
	if strings.Join(moved, ",") != "A,B" {
		t.Errorf("moved = %v, want what the node reported", moved)
	}
}

// A LISTING KEEPS ITS TIMESTAMPS, which is half of what a listing is for:
// "when did this last change" is the question an operator brings to it.
func TestAListingParsesWhatTheNodeReported(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.body = `{"secrets":[{"name":"A","key_id":"k1",` +
		`"updated_at":"2026-03-04T05:06:07Z","updated_by":"sam","source":"cli"}]}`

	rows, err := node.client(t).List(t.Context())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "A" || rows[0].UpdatedBy != "sam" {
		t.Fatalf("rows = %+v", rows)
	}
	if !rows[0].UpdatedAt.Equal(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)) {
		t.Errorf("updated_at = %v, want the reported instant", rows[0].UpdatedAt)
	}
	if rows[0].Value != "" {
		t.Error("a listing carried a value, which is the one thing it must not")
	}
}

// UNSET REPORTS WHETHER A ROW WENT. "It was not set" and "it is gone now" are
// different outcomes and an operator acts differently on each.
func TestUnsetReportsWhetherARowWent(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.body = `{"removed":false}`

	removed, err := node.client(t).Unset(t.Context(), "A")
	if err != nil {
		t.Fatalf("unset: %v", err)
	}
	if removed {
		t.Error("removed = true for a row the node said was not there")
	}
}

// THE WHOLE COMMAND GOES THROUGH THE NODE when -api names one, and never
// touches this machine's database.
//
// That is what makes `crewlet secrets` usable from a laptop against a fleet,
// and it is also the guard against the opposite mistake: opening the local
// store to "check" first would create an empty database beside the Tier A
// file that nothing ever reads, on a machine that runs no engine.
func TestTheCommandWritesThroughTheNamedNode(t *testing.T) {
	node := newFakeSecretsNode(t)
	cfg := bootstrapWithKeyring(t, "k1")
	t.Setenv(apiTokenEnv, "ops-token")

	var out, errs bytes.Buffer
	err := run([]string{"secrets", "set", "GL_TOKEN", "-value", "glpat-x",
		"-config", cfg, "-api", node.server.URL}, &out, &errs)
	if err != nil {
		t.Fatalf("set: %v\n%s", err, errs.String())
	}
	if node.last.method != http.MethodPut || node.last.path != "/secrets/GL_TOKEN" {
		t.Fatalf("the node saw %s %s", node.last.method, node.last.path)
	}
	if node.last.body != "glpat-x" {
		t.Errorf("the node received %q", node.last.body)
	}
	if !strings.Contains(out.String(), node.server.URL) {
		t.Errorf("the output does not say which store it wrote: %q", out.String())
	}
	if strings.Contains(out.String(), "next start") {
		t.Errorf("a fleet write was reported as a node-local one: %q", out.String())
	}
	// The local database beside the Tier A file must not have been created.
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(cfg), "index.db")); statErr == nil {
		t.Error("a database was created on a machine writing through a remote node")
	}
}

// A NODE-LOCAL WRITE SAYS SO, and says what will carry it to the fleet.
//
// An operator who wrote a value while the engine was stopped and saw nothing
// propagate would reasonably conclude the write failed. "This node will put
// it on the fleet at its next start" is not guessable.
func TestANodeLocalWriteSaysWhatHappensNext(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	var out, errs bytes.Buffer
	err := run([]string{"secrets", "set", "GL_TOKEN", "-value", "v", "-config", cfg},
		&out, &errs)
	if err != nil {
		t.Fatalf("set: %v\n%s", err, errs.String())
	}
	for _, want := range []string{
		"index.db",        // which store
		"no peer can see", // and what that costs
		// The note, whose phrasing is distinct from the store's own
		// description on purpose: dropping it while keeping the path
		// would still leave an operator without the way forward.
		"To reach a RUNNING fleet now",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output omits %q: %q", want, out.String())
		}
	}
}
