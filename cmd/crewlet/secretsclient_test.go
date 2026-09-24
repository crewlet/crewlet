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
	"github.com/crewlet/crewlet/internal/httpx"
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
	t.Setenv(apiTokenEnv, cliFixtureToken)
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
	t.Setenv(apiTokenEnv, cliFixtureToken)
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
	t.Setenv(apiTokenEnv, cliFixtureToken)
	_, err := newSecretsClient(bootWithAPI(t, "127.0.0.1", 0, "t"), "")
	if err == nil {
		t.Fatal("a node with api.port 0 was accepted as a write target")
	}
	if !strings.Contains(err.Error(), "api.port") {
		t.Errorf("the refusal does not name the setting to change: %v", err)
	}
}

// THE ENVIRONMENT WINS OVER TIER A, because the token decides ATTRIBUTION.
// Every entry in the list authenticates, but the id is stamped as the author
// of the write, so an operator who wants their own name on a rotation has to
// be able to supply their own credential.
func TestTheEnvironmentTokenWinsOverTierA(t *testing.T) {
	t.Setenv(apiTokenEnv, cliFixtureToken)
	t.Setenv(apiTokenEnv, "mine")
	client, err := newSecretsClient(bootWithAPI(t, "127.0.0.1", 8080, "shared"), "")
	if err != nil {
		t.Fatal(err)
	}
	if client.token != "mine" {
		t.Errorf("token = %q, want the one from the environment", client.token)
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
	t.Setenv(apiTokenEnv, "ops-token")
	node := newFakeSecretsNode(t)
	value := "-----BEGIN KEY-----\nline two\n\ttabbed \n"

	if _, err := node.client(t).Set(t.Context(), "GL_TOKEN", value,
		secrets.Author{Name: "ignored"}, "cli", time.Now()); err != nil {
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

// THE AUTHOR PRINTED IS THE ONE THE NODE RECORDED, and the one this command
// offers never travels.
//
// The node stamps the party its guard authenticated, so the shell's user is
// not who a fleet row names — and the command used to print it anyway, telling
// an operator writing through a Tier A token that the row carried their name.
func TestASecretWriteReportsTheAuthorTheNodeRecorded(t *testing.T) {
	t.Setenv(apiTokenEnv, "ops-token")
	node := newFakeSecretsNode(t)
	node.body = `{"name":"GL_TOKEN","bytes":5,"key_id":"k1",` +
		`"updated_by":"jane.doe","updated_by_kind":"operator",` +
		`"operator_id":"pat:0192f00d-0000-7000-8000-00000000000a"}`

	recorded, err := node.client(t).Set(t.Context(), "GL_TOKEN", "value",
		secrets.Author{Name: "shell-user", Kind: "operator"}, "cli", time.Now())
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	want := secrets.Author{Name: "jane.doe", Kind: "operator",
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}
	if recorded != want {
		t.Errorf("recorded = %+v, want the node's own answer %+v", recorded, want)
	}
	if strings.Contains(node.last.path+node.last.query+node.last.body, "shell-user") {
		t.Errorf("the offered author travelled (%s?%s), so a client could "+
			"choose who a fleet row names", node.last.path, node.last.query)
	}
	if got := describeAuthor(recorded.Name, recorded.OperatorID); got !=
		"jane.doe (through pat:0192f00d-0000-7000-8000-00000000000a)" {
		t.Errorf("printed author = %q, want the name and the credential beside it", got)
	}
}

// AUTHENTICATION IS THE ENVIRONMENT'S, AND A MISSING ONE IS REFUSED HERE.
//
// The guard would answer 401 either way; saying it before the request names
// the way out, which a bare 401 from the far end cannot.
//
// THE CONFIG'S OWN TOKENS ARE NOT A FALLBACK, which this asserts in both
// directions. `api.auth.tokens` is what a node ACCEPTS, and a command that
// helped itself to the first entry authored an operator's write under a name
// they had not chosen — and read a resolved ${VAR} out of a file in the clear
// to do it.
func TestTheCredentialIsTheEnvironmentsAndNotTheConfigs(t *testing.T) {
	t.Setenv(apiTokenEnv, "")
	// A node listing a perfectly good token is STILL refused, because the
	// command has none of its own.
	if _, err := newSecretsClient(bootWithAPI(t, "127.0.0.1", 8080, "ops-token"),
		""); err == nil {

		t.Fatal("a command with no credential was accepted, so it will send " +
			"nothing and read a 401 from the far end")
	} else if !strings.Contains(err.Error(), apiTokenEnv) {
		t.Errorf("the refusal does not name %s: %v", apiTokenEnv, err)
	}
	// AND THE ENVIRONMENT IS ENOUGH ON ITS OWN, which is the control: a
	// node whose Tier A lists nothing is still reachable by somebody
	// holding a machine token.
	t.Setenv(apiTokenEnv, "a-minted-token")
	client, err := newSecretsClient(bootWithAPI(t, "127.0.0.1", 8080, ""), "")
	if err != nil {
		t.Fatalf("an exported credential was refused: %v", err)
	}
	if client.token != "a-minted-token" {
		t.Errorf("the client carries %q", client.token)
	}
}

// A NAME WITH A SLASH IN IT STILL ADDRESSES ONE ROW.
//
// ${VAR} names are conventionally uppercase words, but nothing refuses an odd
// one, and an unescaped slash would silently address a different path — which
// on this surface is a write that goes somewhere else entirely.
func TestAnAwkwardNameIsEscapedIntoThePath(t *testing.T) {
	t.Setenv(apiTokenEnv, cliFixtureToken)
	node := newFakeSecretsNode(t)
	if _, err := node.client(t).Set(t.Context(), "a/b c", "v", secrets.Author{}, "", time.Now()); err != nil {
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
	t.Setenv(apiTokenEnv, cliFixtureToken)
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
	t.Setenv(apiTokenEnv, cliFixtureToken)
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
	t.Setenv(apiTokenEnv, cliFixtureToken)
	node := newFakeSecretsNode(t)
	node.status = http.StatusUnauthorized

	_, err := node.client(t).Set(t.Context(), "T", "v", secrets.Author{}, "", time.Now())
	if err == nil || !strings.Contains(err.Error(), apiTokenEnv) {
		t.Fatalf("err = %v, want it to name %s", err, apiTokenEnv)
	}
}

// THE NODE'S HINT REACHES THE OPERATOR. A refusal this client swallowed would
// leave a 503 with no reason anywhere the person running the command can see.
func TestTheNodesHintIsCarriedIntoTheError(t *testing.T) {
	t.Setenv(apiTokenEnv, cliFixtureToken)
	node := newFakeSecretsNode(t)
	node.status = http.StatusServiceUnavailable
	node.body = `{"error":"no_keyring","hint":"run crewlet secrets keygen"}`

	_, err := node.client(t).Set(t.Context(), "T", "v", secrets.Author{}, "", time.Now())
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
	t.Setenv(apiTokenEnv, cliFixtureToken)
	node := newFakeSecretsNode(t)
	node.body = `{"moved":["A","B"],"engine_keys_moved":3}`

	moved, err := node.client(t).Rekey(t.Context(), "key-2")
	if err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if node.last.query != "key_id=key-2" {
		t.Errorf("query = %q, want the expected key to travel", node.last.query)
	}
	if strings.Join(moved.Moved, ",") != "A,B" || moved.EngineKeys != 3 {
		t.Errorf("moved = %v, want what the node reported", moved)
	}
}

// THE ENGINE'S OWN KEYS ARE REFUSED BY NAME, on both sides of the wire.
//
// The command refuses a reserved name before it opens any store — a stopped
// node's own table never holds one, and a running node refuses it too — and
// a node that answers `reserved_name` comes back as the same sentinel rather
// than as a missing grant, which would send an operator off to ask for one.
func TestTheEnginesOwnKeysAreRefusedByTheCommand(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	const key = "iam/session/018f3a9c-0000-7000-8000-000000000001/refresh"
	for _, args := range [][]string{
		{"get", key, "-reveal"},
		{"set", key, "-value", "stolen"},
		{"unset", key},
	} {
		_, _, err := secretsCmd(t, cfg, args...)
		if !errors.Is(err, secrets.ErrReservedName) {
			t.Errorf("secrets %s answered %v, want ErrReservedName", args[0], err)
		}
	}

	t.Setenv(apiTokenEnv, cliFixtureToken)
	node := newFakeSecretsNode(t)
	node.status = http.StatusForbidden
	node.body = `{"error":"reserved_name","hint":"remove a person with crewlet iam remove"}`
	if _, err := node.client(t).Get(t.Context(), key); !errors.Is(err,
		secrets.ErrReservedName) {
		t.Errorf("the node's reserved_name answered %v, want ErrReservedName", err)
	}
}

// A LISTING SAYS HOW MANY OF THE ENGINE'S KEYS A ROTATION HAS TO MOVE, from
// the count the node carries beside the names — and names none of them.
func TestAListingCountsTheEnginesKeys(t *testing.T) {
	t.Setenv(apiTokenEnv, cliFixtureToken)
	node := newFakeSecretsNode(t)
	node.body = `{"secrets":[],"engine_keys":{"total":4,"by_key":{"k1":3,"k2":1}}}`
	keys, err := node.client(t).EngineKeys(t.Context())
	if err != nil {
		t.Fatalf("EngineKeys: %v", err)
	}
	if keys.Total != 4 || keys.StaleUnder("k2") != 3 {
		t.Errorf("the count reads %+v, want 4 with 3 under a key other than k2", keys)
	}
}

// A LISTING KEEPS ITS TIMESTAMPS, which is half of what a listing is for:
// "when did this last change" is the question an operator brings to it.
func TestAListingParsesWhatTheNodeReported(t *testing.T) {
	t.Setenv(apiTokenEnv, cliFixtureToken)
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
	t.Setenv(apiTokenEnv, cliFixtureToken)
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
	t.Setenv(apiTokenEnv, cliFixtureToken)
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
	t.Setenv(apiTokenEnv, cliFixtureToken)
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

// TestCLIClientsRideTheSharedTransport keeps the operator CLI on the one way
// this tree builds an outbound client. Neither of these has any concurrency
// to gain from the pool — a CLI holds one call in flight — but a second way
// to build a client is the second place the next one gets built with a nil
// Transport, which reads as a default rather than as the omission it is.
// See internal/httpx's package doc.
func TestCLIClientsRideTheSharedTransport(t *testing.T) {
	t.Setenv(apiTokenEnv, cliFixtureToken)
	sc, err := newSecretsClient(bootWithAPI(t, "127.0.0.1", 8080, "ops-token"), "")
	if err != nil {
		t.Fatalf("newSecretsClient: %v", err)
	}
	if sc.http.Transport != httpx.Transport() {
		t.Errorf("secrets client transport = %T, want the one httpx shares", sc.http.Transport)
	}

	nc := &nodeClient{http: httpx.Client(nodeRequestTimeout)}
	patient := nc.patiently(time.Hour)
	if patient.http.Transport != httpx.Transport() {
		t.Errorf("patiently() transport = %T, want the one httpx shares", patient.http.Transport)
	}
	if patient.http.Timeout != time.Hour {
		t.Errorf("patiently(1h) timeout = %v, want 1h", patient.http.Timeout)
	}
}
