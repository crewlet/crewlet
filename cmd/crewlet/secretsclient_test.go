package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/secrets"
)

// secretsList is the run most of these cases stand for: `crewlet secrets list`,
// which reveals nothing, so the bearer it sends is the invocation's own.
var secretsList = storeRun{command: "crewlet secrets list"}

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
		client, err := newSecretsClient(tokenEnviron(cliFixtureToken), bootWithAPI(t, host, 9090, "t"), "", secretsList)
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
	client, err := newSecretsClient(tokenEnviron(cliFixtureToken), bootWithAPI(t, "fd00::1", 9090, "t"), "", secretsList)
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
	_, err := newSecretsClient(tokenEnviron(cliFixtureToken), bootWithAPI(t, "127.0.0.1", 0, "t"), "", secretsList)
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
	t.Parallel()
	client, err := newSecretsClient(tokenEnviron("mine"), bootWithAPI(t, "127.0.0.1", 8080, "shared"), "", secretsList)
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

	// requests counts every request the node was sent.
	requests int
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
	n.server = httptest.NewServer(asEngine(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		n.requests++
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
	})))
	t.Cleanup(n.server.Close)
	return n
}

func (n *fakeSecretsNode) client(t *testing.T) *secretsClient {
	t.Helper()
	c, err := newSecretsClient(tokenEnviron("ops-token"),
		bootWithAPI(t, "127.0.0.1", 1, "ops-token"), n.server.URL, secretsList)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// THE VALUE IS THE BODY, verbatim, and nothing else travels.
//
// A credential is arbitrary bytes — a PEM key has newlines, a token can hold
// anything — and wrapping it in JSON puts an encoding step between the
// operator and the byte sequence the vendor will compare. A value that came
// back re-encoded would fail at the vendor with a 401 that names neither. And
// NO QUERY: the node stamps its route's own source and refuses a write that
// names one, so the `?source=` this sent once would now fail every rotation.
func TestSettingASecretSendsTheRawValue(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	value := "-----BEGIN KEY-----\nline two\n\ttabbed \n"

	if _, err := node.client(t).Set(t.Context(), "GL_TOKEN", value,
		secrets.Author{Name: "ignored"}, secrets.SourceCLI, time.Now()); err != nil {
		t.Fatalf("set: %v", err)
	}
	if node.last.body != value {
		t.Errorf("the node received %q, want the value byte for byte", node.last.body)
	}
	if node.last.method != http.MethodPut || node.last.path != "/secrets/GL_TOKEN" {
		t.Errorf("%s %s, want PUT /secrets/GL_TOKEN", node.last.method, node.last.path)
	}
	if node.last.query != "" {
		t.Errorf("query = %q, want none: the node's route stamps its own source "+
			"and refuses a write that names one", node.last.query)
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
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.body = `{"name":"GL_TOKEN","bytes":5,"key_id":"k1",` +
		`"updated_by":"jane.doe","updated_by_kind":"operator",` +
		`"operator_id":"pat:0192f00d-0000-7000-8000-00000000000a"}`

	written, err := node.client(t).Set(t.Context(), "GL_TOKEN", "value",
		secrets.Author{Name: "shell-user", Kind: "operator"}, secrets.SourceCLI, time.Now())
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	recorded := written.Author
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
	t.Parallel()
	// A node listing a perfectly good token is STILL refused, because the
	// command has none of its own.
	if _, err := newSecretsClient(noEnviron, bootWithAPI(t, "127.0.0.1", 8080, "ops-token"),
		"", secretsList); err == nil {

		t.Fatal("a command with no credential was accepted, so it will send " +
			"nothing and read a 401 from the far end")
	} else if !strings.Contains(err.Error(), apiTokenEnv) {
		t.Errorf("the refusal does not name %s: %v", apiTokenEnv, err)
	}
	// AND THE ENVIRONMENT IS ENOUGH ON ITS OWN, which is the control: a
	// node whose Tier A lists nothing is still reachable by somebody
	// holding a machine token.
	client, err := newSecretsClient(tokenEnviron("a-minted-token"), bootWithAPI(t, "127.0.0.1", 8080, ""), "", secretsList)
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
	t.Parallel()
	node := newFakeSecretsNode(t)
	if _, err := node.client(t).Set(t.Context(), "a/b c", "v", secrets.Author{}, secrets.SourceCLI, time.Now()); err != nil {
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

// A 404 WITH NO not_found BODY IS A MISSING SURFACE, not a missing secret.
//
// A bare 404 — no code of the engine's at all — came from something in front
// of the node or from another service at the address, and reporting that as
// "no such secret" would have an operator set a value over and over against a
// server that will never hold it.
func TestA404WithoutTheBodyIsReportedAsAMissingSurface(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.status, node.body = http.StatusNotFound, `{}`

	_, err := node.client(t).Get(t.Context(), "TOKEN")
	if err == nil || errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("a node with no /secrets surface was reported as a missing "+
			"secret: %v", err)
	}
	if !strings.Contains(err.Error(), "not a Crewlet node's API") {
		t.Errorf("the refusal does not say what answered: %v", err)
	}
}

// A NODE WHOSE ROUTER HAS NO /secrets IS A MISSING SURFACE TOO, and it is
// named for what it is. A node without the ingress role binds api.port for
// its probes alone ([api.Probes]), and its router answers every other
// path `no_route` — never `not_found`, the one this client reads as "no such
// secret" — so it is not taken for a store holding nothing, which the
// provisioning sink would answer by minting a value it could never write. And
// it is not told it is "not a Crewlet node" either: it is one, missing a role.
func TestTheRoutersOwn404IsAMissingSurfaceNotAMissingSecret(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(asEngine(httpjson.Mux(http.NewServeMux())))
	t.Cleanup(server.Close)
	c, err := newSecretsClient(tokenEnviron(cliFixtureToken), bootWithAPI(t, "127.0.0.1", 1, "ops-token"), server.URL, secretsList)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(t.Context(), "TOKEN")
	if err == nil || errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("a node with no /secrets route answered %v, want the missing "+
			"surface", err)
	}
	if !strings.Contains(err.Error(), "no /secrets surface") ||
		!strings.Contains(err.Error(), "ingress") {
		t.Errorf("the refusal does not name the missing surface and the role "+
			"that serves it: %v", err)
	}
	if strings.Contains(err.Error(), "not a Crewlet node") {
		t.Errorf("a Crewlet node's own router was called something else: %v", err)
	}
}

// A REJECTED TOKEN NAMES THE VARIABLE THAT FIXES IT.
func TestARejectedTokenSaysWhatToSet(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.status = http.StatusUnauthorized

	_, err := node.client(t).Set(t.Context(), "T", "v", secrets.Author{}, secrets.SourceCLI, time.Now())
	if err == nil || !strings.Contains(err.Error(), apiTokenEnv) {
		t.Fatalf("err = %v, want it to name %s", err, apiTokenEnv)
	}
}

// THE NODE'S HINT REACHES THE OPERATOR. A refusal this client swallowed would
// leave a failure with no reason anywhere the person running the command can
// see.
func TestTheNodesHintIsCarriedIntoTheError(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.status = http.StatusBadRequest
	node.body = `{"error":"invalid_name","hint":"a name is what a ${VAR} can hold"}`

	_, err := node.client(t).Set(t.Context(), "T", "v", secrets.Author{}, secrets.SourceCLI, time.Now())
	if err == nil {
		t.Fatal("a 400 was accepted as success")
	}
	for _, want := range []string{"invalid_name", "a name is what a ${VAR} can hold"} {
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
	node.body = `{"moved":["A","B"],"engine_keys_moved":3,` +
		`"identity":{"people":2,"values":5}}`

	moved, err := node.client(t).Rekey(t.Context(), "key-2")
	if err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if node.last.query != "key_id=key-2" {
		t.Errorf("query = %q, want the expected key to travel", node.last.query)
	}
	if strings.Join(moved.Moved, ",") != "A,B" || moved.EngineKeys != 3 ||
		moved.Identity == nil || moved.Identity.People != 2 || moved.Identity.Values != 5 {
		t.Errorf("moved = %+v, want what the node reported, the identity "+
			"estate's half included", moved)
	}
}

// THE ENGINE'S OWN KEYS ARE REFUSED BY NAME, on both sides of the wire.
//
// The command refuses a reserved name before it reaches any store — the
// fleet's store refuses one when written directly, and a running node refuses
// it too — and
// a node that answers `reserved_name` comes back as the same sentinel rather
// than as a missing grant, which would send an operator off to ask for one.
func TestTheEnginesOwnKeysAreRefusedByTheCommand(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	const key = "iam/session/018f3a9c-0000-7000-8000-000000000001/refresh"
	for _, name := range []string{key, "iam/blind-index-key"} {
		for _, args := range [][]string{
			{"get", name, "-reveal"},
			{"set", name, "-value", "stolen"},
			{"unset", name},
		} {
			_, _, err := secretsCmd(t, cfg, args...)
			if !errors.Is(err, secrets.ErrReservedName) {
				t.Errorf("secrets %s %s answered %v, want ErrReservedName",
					args[0], name, err)
			}
		}
	}

	node := newFakeSecretsNode(t)
	node.status = http.StatusForbidden
	node.body = `{"error":"reserved_name","hint":"the address index's key belongs to the identity estate"}`
	if _, err := node.client(t).Get(t.Context(), key); !errors.Is(err,
		secrets.ErrReservedName) {
		t.Errorf("the node's reserved_name answered %v, want ErrReservedName", err)
	}
}

// A LISTING SAYS HOW MUCH A ROTATION HAS TO MOVE THAT NO COMMAND NAMES, from
// the counts the node carries beside the names — the engine's keys and the
// identity estate's values, and names none of it, nor the assistant's values,
// which a rotation waits out and the wait-out says so. A node that runs no
// identity or assistant estate says so with a null, which reads back as nil
// rather than as an estate with nothing in it.
func TestAListingCountsWhatNoCommandNames(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.body = `{"secrets":[],"engine_keys":{"total":4,"by_key":{"k1":3,"k2":1}},` +
		`"identity_values":{"people":{"k1":5,"k2":2},"invitations":{"k1":1}},` +
		`"assistant_values":{"values":{"k1":7,"k2":4}}}`
	held, err := node.client(t).EngineKeys(t.Context())
	if err != nil {
		t.Fatalf("EngineKeys: %v", err)
	}
	if held.Keys.Total != 4 || held.Keys.StaleUnder("k2") != 3 {
		t.Errorf("the count reads %+v, want 4 with 3 under a key other than k2",
			held.Keys)
	}
	if held.Identity == nil || held.Identity.Outside("k2") != 5 ||
		held.Identity.InvitationsOutside("k2") != 1 {
		t.Errorf("the identity estate reads %+v, want 5 values and 1 invitation "+
			"under a key other than k2", held.Identity)
	}

	if held.Assistant == nil || held.Assistant.Outside("k2") != 7 {
		t.Errorf("the assistant's values read %+v, want 7 under a key other than k2",
			held.Assistant)
	}
	var out strings.Builder
	waitOut(&out, held, "k2")
	if !strings.Contains(out.String(), "7 values in Dashboard Assistant conversations") ||
		!strings.Contains(out.String(), "1 outstanding invitations") {
		t.Errorf("the rotation's wait-out said %q, want the conversations and the "+
			"invitation still under another key", out.String())
	}

	node.body = `{"secrets":[],"engine_keys":{"total":0},"identity_values":null,` +
		`"assistant_values":null}`
	if held, err = node.client(t).EngineKeys(t.Context()); err != nil ||
		held.Identity != nil || held.Assistant != nil {
		t.Errorf("a node with no identity estate read back as %+v (%v)", held, err)
	}
}

// A LISTING KEEPS ITS TIMESTAMPS, which is half of what a listing is for:
// "when did this last change" is the question an operator brings to it.
func TestAListingParsesWhatTheNodeReported(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.body = `{"secrets":[{"name":"A","key_id":"k1",` +
		`"updated_at":"2026-03-04T05:06:07Z","updated_by":"sam","source":"api"}]}`

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
	if rows[0].Source != secrets.SourceAPI {
		t.Errorf("source = %q, want the path the node reported", rows[0].Source)
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
	if removed.Removed {
		t.Error("removed = true for a row the node said was not there")
	}
}

// AN UNSET THROUGH A NODE SAYS WHAT THE NODE'S RE-ACTIVATION DID (ADR-0070):
// the node it went through, and the revision the running fleet stopped
// resolving the value at, never the stopped engine's note, which would tell
// an operator the removal waits for a start that is not coming.
//
// Mutations, each red: the store line left out, the node's reload unread, and
// the stopped engine's note given through a node.
func TestAnUnsetThroughANodeSaysWhatItsReactivationDid(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.body = `{"removed":true,"read_by":["integrations.gitlab.token"],` +
		`"reload":{"revision_id":"r-9","epoch":12}}`
	cfg := bootstrapWithKeyring(t, "k1")

	var out, errs bytes.Buffer
	err := runIn(invocation{environ: tokenEnviron("ops-token")}, []string{"secrets", "unset", "GL_TOKEN",
		"-config", cfg, "-api", node.server.URL}, &out, &errs)
	if err != nil {
		t.Fatalf("unset: %v\n%s", err, errs.String())
	}
	if node.last.method != http.MethodDelete || node.last.path != "/secrets/GL_TOKEN" {
		t.Fatalf("the node saw %s %s", node.last.method, node.last.path)
	}
	for _, want := range []string{"removed GL_TOKEN\n", "written to ", node.server.URL,
		removalReloadNote(&configapi.Reloaded{RevisionID: "r-9", Epoch: 12})} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("unset said %q, want it to carry %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), secretsStoppedRemovalNote) {
		t.Errorf("an unset through a running node was told this node's engine "+
			"starts without it: %q", out.String())
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
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.body = `{"read_by":["integrations.gitlab.token"],"reload":{"revision_id":"r-9","epoch":12}}`
	cfg := bootstrapWithKeyring(t, "k1")

	var out, errs bytes.Buffer
	err := runIn(invocation{environ: tokenEnviron("ops-token")}, []string{"secrets", "set", "GL_TOKEN", "-value", "glpat-x",
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
	// AND WHEN THE RUNNING FLEET READS IT: now, at the revision and epoch
	// the node's re-activation produced (ADR-0070), never the stopped
	// engine's note, which would tell an operator the value waits for a
	// start that is not coming. Said by nothing, a rotated key the engine
	// went on using read as a write that failed.
	if !strings.Contains(out.String(), "revision r-9, epoch 12") {
		t.Errorf("a write through a node does not say the revision the running "+
			"fleet reads it at: %q", out.String())
	}
	if strings.Contains(out.String(), secretsStoppedNote) {
		t.Errorf("a write through a running node was told the engine reads it "+
			"when it starts: %q", out.String())
	}
	// Nothing of this machine's was reached: not the database lock the
	// stopped-engine route asks, nor a broker's store directory.
	for _, path := range []string{
		filepath.Join(filepath.Dir(cfg), "index.db.lock"),
		filepath.Join(filepath.Dir(cfg), "stream"),
	} {
		if _, statErr := os.Stat(path); statErr == nil {
			t.Errorf("%s was touched by a write through a remote node", path)
		}
	}
}

// A MACHINE THAT IS NOT A NODE RUNS `crewlet secrets` THROUGH -api WITH NO TIER
// A FILE, and creates nothing there.
//
// Through a named node this command seals and opens nothing — the node does
// both, under its own keyring — so the Tier A file it reads for a keyring has
// nothing to give it, and requiring one sent an operator on a laptop to put a
// Tier A file carrying a keyring on it (in practice a copy of a node's) to
// take a listing. With no file, nothing names a store on this machine, so
// nothing is opened or created on it either.
//
// rekey is the one subcommand still refused without the file, and before it
// asks the node anything: it judges the rows against the key it expects to
// re-seal under, which is Tier A's, and its real pass sends that key so that a
// node sealing under another refuses it. And with no -api either there is no
// store to reach at all, so the run is refused naming both ways to one —
// except a rekey's, which names only -config, since -api would not spare it
// the file — rather than going on with none.
func TestTheSecretsCommandReachesANamedNodeWithNoTierAHere(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "crewlet.yaml")
	secretsVia := func(answer string, args ...string) (string, error) {
		t.Helper()
		node.body = answer
		var out, errs bytes.Buffer
		full := append(append([]string{"secrets"}, args...), "-config", missing)
		err := runIn(invocation{environ: tokenEnviron("ops-token")}, full, &out, &errs)
		return out.String(), err
	}

	for _, step := range []struct {
		args                []string
		answer              string
		method, path, query string
		says                string
	}{
		{[]string{"list"}, `{"secrets":[{"name":"GL_TOKEN","key_id":"k1"}]}`,
			http.MethodGet, "/secrets", "", "GL_TOKEN"},
		{[]string{"set", "GL_TOKEN", "-value", "glpat-new"},
			`{"updated_by":"token:ops","updated_by_kind":"operator"}`,
			http.MethodPut, "/secrets/GL_TOKEN", "", "stored GL_TOKEN"},
		{[]string{"get", "GL_TOKEN", "-reveal"}, `{"name":"GL_TOKEN","value":"glpat-new"}`,
			http.MethodGet, "/secrets/GL_TOKEN", "reveal=true", "glpat-new"},
		{[]string{"unset", "GL_TOKEN"}, `{"removed":true}`,
			http.MethodDelete, "/secrets/GL_TOKEN", "", "removed GL_TOKEN"},
	} {
		out, err := secretsVia(step.answer, append(step.args, "-api", node.server.URL)...)
		if err != nil {
			t.Fatalf("secrets %s through -api with no Tier A here: %v", step.args[0], err)
		}
		if !strings.Contains(out, step.says) {
			t.Errorf("secrets %s said %q, want %q", step.args[0], out, step.says)
		}
		if got := node.last; got.method != step.method || got.path != step.path ||
			got.query != step.query || got.auth != "Bearer ops-token" {
			t.Errorf("secrets %s sent %s %s?%s (%s), want %s %s?%s with the "+
				"environment's token", step.args[0], got.method, got.path, got.query,
				got.auth, step.method, step.path, step.query)
		}
	}

	asked := node.requests
	_, err := secretsVia(`{}`, "rekey", "-api", node.server.URL)
	if err == nil || !strings.Contains(err.Error(), "-config") ||
		!strings.Contains(err.Error(), "secrets.active_key_id") {
		t.Errorf("a rekey with no Tier A to name its key was not refused naming "+
			"-config and the key it reads there: %v", err)
	}
	_, err = secretsVia(`{}`, "list")
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "-config") ||
		!strings.Contains(err.Error(), "-api URL") {
		t.Errorf("a run with neither a Tier A file nor -api was not refused naming "+
			"both: %v", err)
	}
	_, err = secretsVia(`{}`, "rekey", "-dry-run")
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "-config") ||
		!strings.Contains(err.Error(), "secrets.active_key_id") ||
		strings.Contains(err.Error(), "-api URL naming") {
		t.Errorf("a rekey with neither a Tier A file nor -api was not refused naming "+
			"-config alone, and the key it reads there: %v", err)
	}
	if node.requests != asked {
		t.Errorf("the node was sent %d requests by runs refused before they reach "+
			"a store", node.requests-asked)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("a run with no Tier A here created %s", filepath.Join(dir, entry.Name()))
	}
}

// A WRITE WITH THE ENGINE STOPPED SAYS WHERE IT WENT AND WHO READS IT WHEN.
//
// It went to the fleet's store, through this node's own broker, and a node
// reads it through its snapshot: this one's engine when it starts, one already
// running at its next apply. An operator who rotated a key and saw a peer go on
// using the old one would otherwise conclude the write failed.
func TestAWriteWithTheEngineStoppedSaysWhereItWentAndWhoReadsIt(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	var out, errs bytes.Buffer
	err := run([]string{"secrets", "set", "GL_TOKEN", "-value", "v", "-config", cfg},
		&out, &errs)
	if err != nil {
		t.Fatalf("set: %v\n%s", err, errs.String())
	}
	for _, want := range []string{
		filepath.Join(filepath.Dir(cfg), "stream"), // which broker
		"engine is stopped",                        // and why this way in
		"reads it when it starts",                  // who reads it, when
		"POST /config/reload",                      // and how to make a running one read it now
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
	t.Parallel()
	sc, err := newSecretsClient(tokenEnviron(cliFixtureToken), bootWithAPI(t, "127.0.0.1", 8080, "ops-token"), "", secretsList)
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

// A FLEET OF HUNDREDS OF STORED VALUES IS LISTED WHOLE. The listing names
// every stored secret and holds no value, and it used to share the bound of
// one credential's answer: about 300 rows passed it, and every command that
// resolves a company through a running node — they all start with the
// listing — failed before reading one, blaming a credential. `crewlet secrets
// rekey` counts the engine's keys from the same listing, and was refused with
// it. A credential's answer keeps its own bound
// ([secretsapi.RevealAnswerBytes]), since a clipped one would be worse than
// none.
func TestAListingOfHundredsOfSecretsIsReadWhole(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	var rows []string
	for i := range 2000 {
		rows = append(rows, fmt.Sprintf(`{"name":"SEAT_TOKEN_%04d","key_id":"k1",`+
			`"updated_at":"2026-01-01T00:00:00Z","updated_by":"token:ops",`+
			`"updated_by_kind":"operator","source":"api"}`, i))
	}
	node.body = `{"secrets":[` + strings.Join(rows, ",") + `]}`
	if len(node.body) <= maxRowAnswerBytes {
		t.Fatalf("the premise: a listing of %d bytes is within a row answer's bound", len(node.body))
	}
	listed, err := node.client(t).List(t.Context())
	if err != nil {
		t.Fatalf("a listing of %d rows was refused: %v", len(rows), err)
	}
	if len(listed) != len(rows) || listed[len(listed)-1].Name != "SEAT_TOKEN_1999" {
		t.Errorf("listed %d rows, want all %d", len(listed), len(rows))
	}
	if _, err := node.client(t).EngineKeys(t.Context()); err != nil {
		t.Errorf("the count a rekey reads was refused: %v", err)
	}

	node.body = `{"value":"` + strings.Repeat("x", secretsapi.RevealAnswerBytes("TOO_LONG")) + `"}`
	if _, err := node.client(t).Get(t.Context(), "TOO_LONG"); err == nil ||
		!strings.Contains(err.Error(), "a credential this long") {
		t.Errorf("a reveal past one credential's bound = %v, want it refused as one", err)
	}
}

// A REKEY OF THOUSANDS OF SECRETS IS READ WHOLE. Its answer names every row it
// moved, so it grows with the company as the listing does, and it was held to
// the bound every one-value answer shared (68 KiB): a rotation the node had
// finished came back to `crewlet secrets rekey` as a failure.
//
// Mutation: read the rekey to the row bound, and its answer is refused.
func TestARekeyOfThousandsOfSecretsIsReadWhole(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	var names []string
	for i := range 5000 {
		names = append(names, fmt.Sprintf(`"SEAT_TOKEN_%04d"`, i))
	}
	node.body = `{"moved":[` + strings.Join(names, ",") +
		`],"engine_keys_moved":0,"identity":null}`
	if len(node.body) <= maxRowAnswerBytes {
		t.Fatalf("the premise: a rekey answer of %d bytes is within a row answer's bound",
			len(node.body))
	}
	moved, err := node.client(t).Rekey(t.Context(), "k2")
	if err != nil {
		t.Fatalf("a rekey that moved %d rows was refused: %v", len(names), err)
	}
	if len(moved.Moved) != len(names) || moved.Moved[len(moved.Moved)-1] != "SEAT_TOKEN_4999" {
		t.Errorf("the rekey reported %d moved, want all %d", len(moved.Moved), len(names))
	}
}

// A SET OF VALUES IS READ IN REQUESTS OF THE NODE'S SIZE: a run reading the
// variables a command resolves asks for them together, in requests of at most
// secretsapi.MaxRevealNames — never one request per name, and never one the
// node refuses for its length — each name once, as JSON, and only the names
// the node holds come back.
//
// Mutation: send the whole set in one request, and the node's limit is broken;
// send one name a request, and the count is 130.
func TestASetOfValuesIsReadInRequestsOfTheNodesSize(t *testing.T) {
	t.Parallel()
	held := map[string]string{}
	var names []string
	for i := range 130 {
		name := fmt.Sprintf("VALUE_%03d", i)
		names = append(names, name)
		if i%2 == 0 {
			held[name] = "v-" + name
		}
	}
	node := newRevealNode(t, held)
	client, err := newSecretsClient(tokenEnviron("ops-token"), bootWithAPI(t, "127.0.0.1", 1, ""),
		node.server.URL, storeRun{command: "crewlet gitlab provision", reveals: true})
	if err != nil {
		t.Fatal(err)
	}
	values, err := client.Values(t.Context(), names)
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	asked, gets := node.batches()
	if len(asked) != 3 || len(gets) != 0 {
		t.Fatalf("read 130 names in %d batches and %d GETs, want 3 batches", len(asked), len(gets))
	}
	var sent []string
	for _, batch := range asked {
		if len(batch) > secretsapi.MaxRevealNames {
			t.Errorf("a batch asked for %d names, past the node's %d", len(batch),
				secretsapi.MaxRevealNames)
		}
		sent = append(sent, batch...)
	}
	if !slices.Equal(sent, names) {
		t.Errorf("the batches asked %v, want each name once, in order", sent)
	}
	if !maps.Equal(values, held) {
		t.Errorf("read %d values, want exactly the %d the node holds", len(values), len(held))
	}
	for _, contentType := range node.contentTypes {
		if contentType != "application/json" {
			t.Errorf("a batch was sent as %q, not JSON", contentType)
		}
	}
}

// A REFUSED BATCH FAILS THE WHOLE READ: a value missing because the node would
// not answer is not one the fleet does not hold, so no partial answer comes
// back for a caller to resolve the rest from the environment — and the
// refusal is the node's, so a refused token is told from a failing node.
func TestARefusedBatchFailsTheWholeRead(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"A": "a"})
	node.refuse = http.StatusForbidden
	client, err := newSecretsClient(tokenEnviron("ops-token"), bootWithAPI(t, "127.0.0.1", 1, ""),
		node.server.URL, storeRun{command: "crewlet gitlab provision", reveals: true})
	if err != nil {
		t.Fatal(err)
	}
	values, err := client.Values(t.Context(), []string{"A", "B"})
	if err == nil || values != nil {
		t.Fatalf("a refused batch answered %v, %v", values, err)
	}
	if !refusedTheToken(err) {
		t.Errorf("the node's refusal of the token was lost: %v", err)
	}
}

// secretsNode is a real /secrets surface over a memory fleet, served the way a
// node serves it to a Tier A token holding admin — so a case about what the
// client reads back reads exactly what the node writes, escaping included.
func secretsNode(t *testing.T) *secretsClient {
	t.Helper()
	sum := make([]byte, 32)
	sum[0] = 1
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": sum}})
	if err != nil {
		t.Fatal(err)
	}
	return secretsNodeOver(t, coordmem.NewFleet(), cipher)
}

// secretsNodeOver is [secretsNode] over a fleet and a keyring the case holds,
// so it can put rows on the fleet the way a node's start does before the
// client reads them. The keyring's active key is k1.
func secretsNodeOver(t *testing.T, fleet coord.Fleet, cipher secrets.Cipher) *secretsClient {
	t.Helper()
	return secretsNodeReading(t, fleet, cipher, &nodeCompany{})
}

// nodeCompany is the company a real /secrets surface asks about a write:
// which paths read each name, and the re-activation, which it counts.
type nodeCompany struct {
	mu      sync.Mutex
	reads   map[string][]string
	reloads []string
	// refuse is what each reload answers in turn before they land, for a
	// case about a re-activation that fails and is retried.
	refuse []error
}

func (c *nodeCompany) Reads(_ context.Context, names []string) (map[string][]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]string{}
	for _, name := range names {
		if paths, ok := c.reads[name]; ok {
			out[name] = paths
		}
	}
	return out, nil
}

func (c *nodeCompany) Reload(_ context.Context, summary string, _ iam.Actor,
	_ string) (configapi.Applied, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reloads = append(c.reloads, summary)
	if len(c.refuse) > 0 {
		err := c.refuse[0]
		c.refuse = c.refuse[1:]
		return configapi.Applied{}, err
	}
	return configapi.Applied{RevisionID: "r-" + strconv.Itoa(len(c.reloads)),
		Epoch: int64(100 + len(c.reloads))}, nil
}

// secretsNodeReading is [secretsNodeOver] over a company that reads what c
// says, so a case can watch what the node re-activates.
func secretsNodeReading(t *testing.T, fleet coord.Fleet, cipher secrets.Cipher,
	c *nodeCompany) *secretsClient {
	t.Helper()
	svc, err := secretsapi.New(secretsapi.Options{Fleet: fleet,
		Cipher: cipher, ActiveKeyID: "k1", Config: c})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	// AND THE RE-ACTIVATION, as the config surface answers it, over the
	// same company: what a provisioning run's flush sends.
	mux.HandleFunc("POST /config/reload", func(w http.ResponseWriter, r *http.Request) {
		applied, err := c.Reload(r.Context(), r.Header.Get("X-Summary"), iam.Actor{}, "")
		var raced *configapi.RacedError
		var invalid *configapi.ValidationError
		switch {
		case errors.As(err, &raced):
			httpjson.Fail(w, http.StatusConflict, httpjson.CodeRevisionAdvanced)
		case errors.As(err, &invalid):
			httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeValidationError)
		case err != nil:
			httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		default:
			httpjson.Write(w, http.StatusCreated, map[string]any{
				"revision_id": applied.RevisionID, "epoch": applied.Epoch})
		}
	})
	caller := iam.Principal{
		ID:    uuid.NewSHA1(auth.TokenNamespace, []byte("ops")),
		Login: iam.TokenLogin("ops"), Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour),
	}
	server := httptest.NewServer(asEngine(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), caller)))
	})))
	t.Cleanup(server.Close)
	client, err := newSecretsClient(tokenEnviron("ops-token"),
		bootWithAPI(t, "127.0.0.1", 1, "ops-token"), server.URL, secretsList)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// A VALUE THE STORE TOOK COMES BACK, HOWEVER JSON SPELLS IT.
//
// Both reveals answer a value as a JSON string, which spells `&`, `<`, `>` and
// every control byte as a six-byte escape — so the largest value the write
// admits can come back six times as long. The single reveal was read to the
// write's limit plus room for the JSON around it (68 KiB) and refused that
// value as "a credential this long is not one this build stores":
// `crewlet secrets get -reveal`, the break-glass read, could not hand back a
// credential the store had taken.
func TestTheLargestValueTheStoreTakesIsRevealedBothWays(t *testing.T) {
	t.Parallel()
	client := secretsNode(t)
	for _, tc := range []struct{ name, value string }{
		{"AMPERSANDS", strings.Repeat("&", secrets.MaxValueBytes)},
		{"CONTROL_BYTES", strings.Repeat("\x01", secrets.MaxValueBytes)},
	} {
		if _, err := client.Set(t.Context(), tc.name, tc.value, secrets.Author{},
			"", time.Time{}); err != nil {
			t.Fatalf("set %s: %v", tc.name, err)
		}
		got, err := client.Get(t.Context(), tc.name)
		if err != nil || got != tc.value {
			t.Errorf("GET reveal of %s: %d bytes, %v; want the %d stored",
				tc.name, len(got), err, len(tc.value))
		}
		values, err := client.Values(t.Context(), []string{tc.name})
		if err != nil || values[tc.name] != tc.value {
			t.Errorf("POST reveal of %s: %d bytes, %v; want the %d stored",
				tc.name, len(values[tc.name]), err, len(tc.value))
		}
	}
}

// A VALUE SET WITH THE ENGINE STOPPED IS READ BACK WHOLE THROUGH A NODE.
//
// The direct write is the one path no node's route stands in front of: the
// command writes the fleet's store itself, and a node reads the row back later
// through its reveals. Nothing held that write to the size the reveals are read
// to, so a value the store took could be one no read through a node would give
// back. The largest value every store takes, in the character JSON spells
// longest, goes the whole way here: set with the engine stopped, then read back
// from the same store through a node by both reveals.
func TestAValueSetWithTheEngineStoppedAtTheLimitIsReadBackThroughANode(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	value := strings.Repeat("&", secrets.MaxValueBytes)
	if _, errs, err := secretsCmd(t, cfg, "set", "BIG", "-value", value); err != nil {
		t.Fatalf("a set of a value at the limit with the engine stopped: %v (%s)", err, errs)
	}

	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		t.Fatal(err)
	}
	fs, err := engine.OpenFleetStore(t.Context(), boot)
	if err != nil {
		t.Fatalf("open the fleet's store as the engine does: %v", err)
	}
	defer fs.Close()

	client := secretsNodeOver(t, fs.Fleet, cipher)
	if got, err := client.Get(t.Context(), "BIG"); err != nil || got != value {
		t.Errorf("GET reveal: %d bytes, %v; want the %d set", len(got), err, len(value))
	}
	values, err := client.Values(t.Context(), []string{"BIG"})
	if err != nil || values["BIG"] != value {
		t.Errorf("POST reveal: %d bytes, %v; want the %d set",
			len(values["BIG"]), err, len(value))
	}
}

// A WRITE THROUGH A NODE PAST THE SIZE EVERY STORE KEEPS IS REFUSED BEFORE IT
// IS SENT, in the store's own words: the node would refuse it too, with a 413
// naming a request body, after the whole of it had crossed the network.
func TestAWritePastTheSizeEveryStoreKeepsIsNotSentToTheNode(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(asEngine(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		httpjson.Fail(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge)
	})))
	t.Cleanup(server.Close)
	client, err := newSecretsClient(tokenEnviron("ops-token"),
		bootWithAPI(t, "127.0.0.1", 1, "ops-token"), server.URL, secretsList)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Set(t.Context(), "BIG", strings.Repeat("x", secrets.MaxValueBytes+1),
		secrets.Author{}, "", time.Time{})
	if !errors.Is(err, secrets.ErrValueTooLarge) {
		t.Errorf("a write past the limit answered %v, want ErrValueTooLarge", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the client sent %d request(s) carrying a value no store keeps", n)
	}
}

// SEVERAL SECRETS SET TOGETHER ARE ONE ACTIVATION, and each value arrives by
// its name.
//
// A rotation of every credential one model reads, one command each, moved the
// fleet through an epoch per credential, each running a mix of the old and the
// new values. `crewlet secrets set A B` reads NAME=value lines and sends them
// as one `PUT /secrets`, which the node re-activates once, and says the epoch.
func TestSettingSeveralSecretsIsOneActivation(t *testing.T) {
	t.Parallel()
	sum := make([]byte, 32)
	sum[0] = 1
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": sum}})
	if err != nil {
		t.Fatal(err)
	}
	company := &nodeCompany{reads: map[string][]string{
		"AWS_ACCESS_KEY_ID":     {"providers.llm.bedrock.env.AWS_ACCESS_KEY_ID"},
		"AWS_SECRET_ACCESS_KEY": {"providers.llm.bedrock.env.AWS_SECRET_ACCESS_KEY"},
	}}
	client := secretsNodeReading(t, coordmem.NewFleet(), cipher, company)
	target := &secretTarget{secretBackend: client, throughNode: true, where: "the node"}

	var out, errs bytes.Buffer
	err = setSecrets(t.Context(), target, []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"},
		strings.NewReader("AWS_SECRET_ACCESS_KEY=s3cr=et\n\nAWS_ACCESS_KEY_ID=AKIA-not-real\n"),
		&out, &errs)
	if err != nil {
		t.Fatalf("set: %v\n%s", err, errs.String())
	}
	if len(company.reloads) != 1 {
		t.Fatalf("reloads = %q, want ONE for both", company.reloads)
	}
	if !strings.Contains(out.String(), "revision r-1, epoch 101") {
		t.Errorf("the output does not name the epoch it landed at: %q", out.String())
	}
	values, err := client.Values(t.Context(), []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	// BY NAME, whatever the order, and a value keeps an = of its own.
	if values["AWS_SECRET_ACCESS_KEY"] != "s3cr=et" || values["AWS_ACCESS_KEY_ID"] != "AKIA-not-real" {
		t.Errorf("stored %v", values)
	}

	// ANY FAULT STORES NOTHING: a name not asked for, one missing, one twice.
	for _, in := range []string{
		"AWS_ACCESS_KEY_ID=a\nOTHER=b\n",
		"AWS_ACCESS_KEY_ID=a\n",
		"AWS_ACCESS_KEY_ID=a\nAWS_ACCESS_KEY_ID=b\nAWS_SECRET_ACCESS_KEY=c\n",
		"AWS_ACCESS_KEY_ID a\nAWS_SECRET_ACCESS_KEY=c\n",
	} {
		if err := setSecrets(t.Context(), target, []string{"AWS_ACCESS_KEY_ID",
			"AWS_SECRET_ACCESS_KEY"}, strings.NewReader(in), &out, &errs); err == nil {
			t.Errorf("stdin %q was stored", in)
		}
	}
	if len(company.reloads) != 1 {
		t.Errorf("a refused set re-activated: %q", company.reloads)
	}
}

// A PROVISIONING RUN THROUGH A NODE RE-ACTIVATES ONCE, HOWEVER MANY VALUES IT
// MINTS, AND SEALS NOTHING TWICE.
//
// Each minted credential is sealed the moment it exists (the sink's contract:
// a token live at the vendor and recorded nowhere cannot be revoked), with its
// re-activation deferred; the run's flush re-activates once, and seals nothing,
// so a value rotated while the run went on is not written back over.
func TestAProvisioningRunThroughANodeReactivatesOnce(t *testing.T) {
	t.Parallel()
	names := []string{"GITLAB_TOKEN_CEO", "GITLAB_TOKEN_QA", "GITLAB_TOKEN_SWE"}
	company := &nodeCompany{reads: map[string][]string{}}
	for _, name := range names {
		company.reads[name] = []string{"roles.integrations.gitlab.token"}
	}
	client, sink := nodeSinkOver(t, company)
	for _, name := range names {
		if err := sink.Record(t.Context(), name, "glpat-"+name); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if len(company.reloads) != 0 {
		t.Fatalf("a recorded value re-activated before the run ended: %q", company.reloads)
	}
	held, err := client.Values(t.Context(), names)
	if err != nil || len(held) != len(names) {
		t.Fatalf("the minted values are not durable before the flush: (%v, %v)", held, err)
	}
	// SOMEBODY ROTATES ONE meanwhile, and the flush must not undo it.
	if _, err := client.Set(t.Context(), "GITLAB_TOKEN_QA", "glpat-rotated", secrets.Author{},
		secrets.SourceCLI, time.Now()); err != nil {
		t.Fatal(err)
	}
	reloadsBefore := len(company.reloads)
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(company.reloads) - reloadsBefore; got != 1 {
		t.Errorf("the flush made %d re-activations, want ONE for the whole run", got)
	}
	if held, _ := client.Values(t.Context(), []string{"GITLAB_TOKEN_QA"}); held["GITLAB_TOKEN_QA"] != "glpat-rotated" {
		t.Errorf("the flush wrote back over a rotation: %q", held["GITLAB_TOKEN_QA"])
	}
	if step := sink.NextStep(); !strings.Contains(step, "re-activated the company once") {
		t.Errorf("next step = %q", step)
	}
	if err := sink.Flush(t.Context()); err != nil || len(company.reloads)-reloadsBefore != 1 {
		t.Errorf("a second flush with nothing new re-activated: %q (%v)", company.reloads, err)
	}
}

// nodeSinkOver is a provisioning run's sink through one node's /secrets and
// its re-activation, waiting nothing between retries, beside the node's
// client.
func nodeSinkOver(t *testing.T, company *nodeCompany) (*secretsClient, *nodeSecretSink) {
	t.Helper()
	sum := make([]byte, 32)
	sum[0] = 3
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": sum}})
	if err != nil {
		t.Fatal(err)
	}
	client := secretsNodeReading(t, coordmem.NewFleet(), cipher, company)
	target := &secretTarget{secretBackend: client, throughNode: true, where: "the node"}
	store := sinkStore{target: target,
		values: &storedValues{values: map[string]string{}, held: map[string]bool{}}}
	return client, &nodeSecretSink{SecretStoreSink: provision.NewSecretStoreSink(store, hostAuthor()),
		reload: client.Reload, retries: []time.Duration{0, 0}}
}

// A RUN THAT DIES BEFORE ITS FLUSH LEAVES ITS VALUES SEALED AND INACTIVE until
// the company is next re-activated: nothing that runs reads them before.
func TestARunThatDiesBeforeItsFlushLeavesItsValuesSealedAndInactive(t *testing.T) {
	t.Parallel()
	company := &nodeCompany{reads: map[string][]string{
		"GITLAB_TOKEN_A": {"roles[0].integrations.gitlab.token"},
		"GITLAB_TOKEN_B": {"roles[1].integrations.gitlab.token"},
	}}
	client, died := nodeSinkOver(t, company)
	for _, name := range []string{"GITLAB_TOKEN_A", "GITLAB_TOKEN_B"} {
		if err := died.Record(t.Context(), name, "glpat-"+name); err != nil {
			t.Fatal(err)
		}
	}
	// THE PROCESS DIES HERE: no flush.
	held, err := client.Values(t.Context(), []string{"GITLAB_TOKEN_A", "GITLAB_TOKEN_B"})
	if err != nil || len(held) != 2 {
		t.Fatalf("the values of a run that died = (%v, %v), want both sealed", held, err)
	}
	if len(company.reloads) != 0 {
		t.Fatalf("a run that never flushed re-activated: %q", company.reloads)
	}
	// THE NEXT RE-ACTIVATION, whoever makes it, is what the company reads
	// them at.
	if _, err := client.Reload(t.Context(), "reload"); err != nil || len(company.reloads) != 1 {
		t.Errorf("a reload after the run died = %v, reloads %q", err, company.reloads)
	}
}

// A FLUSH WHOSE RE-ACTIVATION FAILS NEVER ROLLS THE RUN BACK.
//
// The values are sealed and the accounts exist: an error from Flush would have
// the run revoke and delete good credentials. A transient failure is retried;
// a refusal is said in the next step, with the reload to make, and Flush
// answers no error.
func TestAFailedFlushRevokesNothing(t *testing.T) {
	t.Parallel()
	company := &nodeCompany{reads: map[string][]string{"TOKEN": {"roles[0].x"}},
		refuse: []error{&configapi.RacedError{Base: "r-1", Current: "r-2"}}}
	_, sink := nodeSinkOver(t, company)
	if err := sink.Record(t.Context(), "TOKEN", "v"); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("a flush that lost one race = %v, want it retried", err)
	}
	if len(company.reloads) != 2 || !strings.Contains(sink.NextStep(), "re-activated the company once") {
		t.Errorf("reloads = %q, next step %q, want the raced one and its retry", company.reloads,
			sink.NextStep())
	}

	refused := &nodeCompany{reads: map[string][]string{"TOKEN": {"roles[0].x"}},
		refuse: []error{&configapi.ValidationError{Err: errors.New("seat x names no model")}}}
	client, sink := nodeSinkOver(t, refused)
	if err := sink.Record(t.Context(), "TOKEN", "v"); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("a refused re-activation failed the flush (%v): the run would roll back", err)
	}
	if len(refused.reloads) != 1 {
		t.Errorf("a refusal was retried %d times", len(refused.reloads))
	}
	if step := sink.NextStep(); !strings.Contains(step, "NOT re-activated") ||
		!strings.Contains(step, "POST /config/reload") {
		t.Errorf("next step = %q, want the reload left to make", step)
	}
	if held, _ := client.Values(t.Context(), []string{"TOKEN"}); held["TOKEN"] != "v" {
		t.Errorf("a refused flush left %v, want the value sealed", held)
	}
}
