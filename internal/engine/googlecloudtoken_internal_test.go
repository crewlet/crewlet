package engine

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// The fake connection's fixed identities. Obviously fake values throughout.
const (
	gcpProvisioner = "crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com"
	gcpProvider    = "projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine"
	gcpBuilderID   = "100000000000000000042"
)

// seatGoogle stands in for Google's token services as the engine reaches them:
// STS exchanges any federation token, the provisioner is impersonated with
// that, and each agent's account with the provisioner's token, unless status
// names what the agent's mint answers instead.
type seatGoogle struct {
	mu     sync.Mutex
	status int
	mints  int
	client *http.Client
}

func newSeatGoogle(t *testing.T) *seatGoogle {
	t.Helper()
	g := &seatGoogle{}
	answer := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		account, minting := strings.CutPrefix(r.URL.Path, "/v1/projects/-/serviceAccounts/")
		account = strings.TrimSuffix(account, ":generateAccessToken")
		expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		switch {
		case r.URL.Path == "/v1/token":
			answer(w, http.StatusOK, map[string]any{"access_token": "ya29.federated", "expires_in": 3600})
		case !minting:
			http.NotFound(w, r)
		case account == gcpProvisioner && r.Header.Get("Authorization") == "Bearer ya29.federated":
			answer(w, http.StatusOK, map[string]string{"accessToken": "ya29.provisioner", "expireTime": expires})
		case r.Header.Get("Authorization") != "Bearer ya29.provisioner":
			answer(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"status": "UNAUTHENTICATED", "message": "Request had invalid authentication credentials."}})
		case g.status != 0:
			answer(w, g.status, map[string]any{"error": map[string]any{"status": "PERMISSION_DENIED",
				"message": "Permission 'iam.serviceAccounts.getAccessToken' denied on " + account}})
		default:
			g.mints++
			answer(w, http.StatusOK, map[string]string{
				"accessToken": "ya29.seat-" + account + "-" + strconv.Itoa(g.mints), "expireTime": expires})
		}
	}))
	t.Cleanup(srv.Close)
	g.client = &http.Client{Transport: httpxtest.Rewrite(t, srv)}
	return g
}

// minted is how many agents' tokens the fake has issued.
func (g *seatGoogle) minted() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mints
}

// refuse makes every agent's mint answer status.
func (g *seatGoogle) refuse(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status = status
}

// testFederationKey is a fresh federation key, as Save mints one.
func testFederationKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// googleCloudCompany is a company on Google Cloud whose `builder` and
// `planner` are on, and whose `idle` is off. Each names its own token for
// Google's Monitoring server, which is recognised by its host.
func googleCloudCompany(t *testing.T) *Company {
	t.Helper()
	seat := func(name, handle, enabled string) string {
		return `  - name: ` + name + `
    handle: ` + handle + `
    llm: zulu
    integrations:
      google_cloud:
        enabled: ` + enabled + `
        projects: [acme-web]
`
	}
	return specCompany(t, `
name: Acme
providers:
  llm:
    zulu:
      model: claude-sonnet-5
  sandbox: {fake: true}
integrations:
  google_cloud:
    enabled: true
    provisioning:
      provisioner: `+gcpProvisioner+`
      provider: `+gcpProvider+`
      federation_key: "${`+googlecloud.FederationKeySecret+`}"
mcp_servers:
  - name: google-cloud-monitoring
    transport: http
    shared: false
    url: "https://monitoring.googleapis.com/mcp"
roles:
  - name: Owner
    handle: owner
    kind: human
`+seat("Builder", "builder", "true")+seat("Planner", "planner", "true")+seat("Idle", "idle", "false"))
}

// googleCloudEngine is an engine on c whose secret store holds the federation
// key and whose ledger records builder's account as made at made, issuing
// through g and nothing else.
func googleCloudEngine(t *testing.T, c *Company, g *seatGoogle, made time.Time) *Engine {
	t.Helper()
	e := &Engine{environ: config.MapSource{googlecloud.FederationKeySecret: testFederationKey(t)},
		backends: &Backends{Fleet: coordmemory.NewFleet()}}
	e.epoch.current.Store(c)
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Put(t.Context(), seataccess.Record{
		Kind: integration.KindGoogleCloud, Handle: "builder",
		Account: &seataccess.Account{ID: gcpBuilderID, Name: "crewlet-builder-0a1b@acme-crewlet-id.iam.gserviceaccount.com",
			CreatedAt: made, CreatedByPass: "test", State: seataccess.AccountActive},
	}); err != nil {
		t.Fatal(err)
	}
	e.issuers = []seatIssuer{googleCloudSeatIssuer{engine: e, tokens: &googlecloud.SeatIssuer{}, http: g.client}}
	return e
}

// googleCloudRig is Google Cloud's row of the conformance table: `builder`'s
// account was made an hour ago, so a refusal now is Google's answer and not
// its delay, and `idle` waits on a person turning it on.
func googleCloudRig(t *testing.T, refused bool) issuerRig {
	g := newSeatGoogle(t)
	if refused {
		g.refuse(http.StatusForbidden)
	}
	c := googleCloudCompany(t)
	return issuerRig{e: googleCloudEngine(t, c, g, time.Now().Add(-time.Hour)), c: c,
		kind: integration.KindGoogleCloud, ready: "builder", waiting: "idle", says: "PERMISSION_DENIED"}
}

// AN OFF SEAT IS ISSUED NOTHING, and Google refusing a fresh account's token
// is Google still applying the grant, which waits on nobody, until the grant's
// window ends, when it is a person's step that names the fix.
func TestGoogleCloudTellsItsDelayFromItsRefusal(t *testing.T) {
	t.Parallel()
	g := newSeatGoogle(t)
	g.refuse(http.StatusForbidden)
	c := googleCloudCompany(t)
	e := googleCloudEngine(t, c, g, time.Now())
	issuer := e.seatIssuers()[0]
	if issue := issuer.issuing(c, c.Org.AgentSeatByHandle("idle"), e.resolver()); issue != nil {
		t.Error("an off seat is issued a token")
	}
	if name := issuer.variable(c, "idle"); name != "GOOGLE_CLOUD_ACCESS_TOKEN_IDLE" {
		t.Errorf("an off seat's variable is %q, want it declared all the same", name)
	}

	for _, handle := range []string{"builder", "planner"} {
		issue := issuer.issuing(c, c.Org.AgentSeatByHandle(handle), e.resolver())
		if issue == nil {
			t.Fatalf("%s is on and issued nothing to retry", handle)
		}
		_, err := issue(t.Context(), toolTokenLife)
		if said, passes := issuer.refusal(err); said != "" || !passes || !errors.Is(err, errIssueSettling) {
			t.Fatalf("%s, its grant fresh or its account not made, is told %q (passes %v), want a wait "+
				"nobody owes", handle, said, passes)
		}
	}

	e = googleCloudEngine(t, c, g, time.Now().Add(-googlecloud.GrantSettle-time.Minute))
	issuer = e.seatIssuers()[0]
	_, err := issuer.issuing(c, c.Org.AgentSeatByHandle("builder"), e.resolver())(t.Context(), toolTokenLife)
	said, passes := issuer.refusal(err)
	if passes || !strings.Contains(said, "PERMISSION_DENIED") || !strings.Contains(said, "Agents tab") {
		t.Fatalf("a refusal past the grant's window is told %q (passes %v), want a person's step naming the fix",
			said, passes)
	}

	g.refuse(http.StatusServiceUnavailable)
	e = googleCloudEngine(t, c, g, time.Now().Add(-time.Hour))
	issuer = e.seatIssuers()[0]
	_, err = issuer.issuing(c, c.Org.AgentSeatByHandle("builder"), e.resolver())(t.Context(), toolTokenLife)
	if said, passes := issuer.refusal(err); said == "" || !passes {
		t.Fatalf("Google not answering is told %q (passes %v), want a wait with its reason", said, passes)
	}
}

// ONE MINT SERVES EVERY READER WHILE IT IS GOOD: a second resolution is handed
// the token the first was, and a token is renewed with toolTokenLife to spare.
func TestOneGoogleCloudMintServesEveryReader(t *testing.T) {
	t.Parallel()
	g := newSeatGoogle(t)
	c := googleCloudCompany(t)
	e := googleCloudEngine(t, c, g, time.Now().Add(-time.Hour))
	builder := c.Org.AgentSeatByHandle("builder")
	first, _ := e.seatResolver(t.Context(), c, builder, toolTokenLife)
	again, _ := e.seatResolver(t.Context(), c, builder, runTokenLife)
	one, two := first.Value("${GOOGLE_CLOUD_ACCESS_TOKEN_BUILDER}"), again.Value("${GOOGLE_CLOUD_ACCESS_TOKEN_BUILDER}")
	if !strings.HasPrefix(one, "ya29.seat-"+gcpBuilderID) || one != two || g.minted() != 1 {
		t.Fatalf("two readers were handed %q and %q in %d mints, want one token minted once", one, two, g.minted())
	}
}

// THE BUILD ISSUES GOOGLE CLOUD'S TOKEN, through the engine's one cache, which
// every reader of a seat's token shares.
func TestTheBuildIssuesGoogleCloudsToken(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	for _, issuer := range e.seatIssuers() {
		if g, ok := issuer.(googleCloudSeatIssuer); ok {
			if g.engine != e || g.tokens != &e.googleCloudIssuer || g.http != nil {
				t.Errorf("the build's Google Cloud issuer is %+v, want the engine's own cache over Google", g)
			}
			return
		}
	}
	t.Fatal("the build lists no Google Cloud issuer")
}
