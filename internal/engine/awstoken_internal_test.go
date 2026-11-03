package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	awsint "github.com/crewlet/crewlet/internal/aws"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// awsConnection is a stack output of the company acme in us-east-2.
const awsConnection = "arn:aws:rolesanywhere:us-east-2:123456789012:trust-anchor/0f0e0d0c-0b0a-4908-8706-050403020100," +
	"arn:aws:rolesanywhere:us-east-2:123456789012:profile/00010203-0405-4607-8809-0a0b0c0d0e0f," +
	"arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-provisioner," +
	"arn:aws:rolesanywhere:us-east-2:123456789012:profile/10010203-0405-4607-8809-0a0b0c0d0e0f," +
	"arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-broker"

// toServer sends every request, whatever AWS host it names, to a fake.
type toServer struct{ to *url.URL }

func (r toServer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

// awsRig is the AWS issuer on a company where builder's seat is on and
// planner's is not yet: Roles Anywhere opens the broker, or refuses the
// certificate when refused, and STS assumes the seat's role.
func awsRig(t *testing.T, refused bool) issuerRig {
	t.Helper()
	if refused {
		return awsRigIn(t, "refused")
	}
	return awsRigIn(t, "ok")
}

// awsRigIn is [awsRig] in mode: "ok", "refused" (Roles Anywhere refuses the
// broker's sign-in, a person's fix), or "settling" (STS will not assume a role
// IAM is still propagating).
func awsRigIn(t *testing.T, mode string) issuerRig {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		_, _ = io.ReadAll(r.Body)
		if r.URL.Path == "/sessions" {
			if mode == "refused" {
				w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"Untrusted certificate. Insufficient certificate"}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"credentialSet": []any{map[string]any{
				"credentials": map[string]any{"accessKeyId": "ASIABROKER", "secretAccessKey": "s",
					"sessionToken": "broker", "expiration": exp}}}})
			return
		}
		if mode == "settling" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code>`+
				`<Message>not authorized to perform sts:AssumeRole</Message></Error></ErrorResponse>`)
			return
		}
		_, _ = fmt.Fprintf(w, `<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>ASIASEAT</AccessKeyId>`+
			`<SecretAccessKey>x</SecretAccessKey><SessionToken>t</SessionToken><Expiration>%s</Expiration>`+
			`</Credentials></AssumeRoleResult></AssumeRoleResponse>`, exp)
	}))
	t.Cleanup(srv.Close)
	to, _ := url.Parse(srv.URL)
	c := specCompany(t, `
name: Acme
providers:
  llm:
    zulu:
      model: claude-sonnet-5
  sandbox: {fake: true}
integrations:
  aws:
    enabled: true
    provisioning:
      slug: acme
      home_region: us-east-2
      certificate_authority: "${AWS_CERTIFICATE_AUTHORITY}"
      connection: "`+awsConnection+`"
mcp_servers:
  - name: aws
    transport: http
    shared: false
    url: "https://aws-mcp.us-east-1.api.aws/mcp"
roles:
  - name: Owner
    handle: owner
    kind: human
  - name: Builder
    handle: builder
    llm: zulu
    integrations:
      aws: {enabled: true, level: operate}
  - name: Planner
    handle: planner
    llm: zulu
`)
	bundle, err := setup.GenerateCertificateAuthority(setup.CertificateAuthorityName(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{environ: config.MapSource{"AWS_CERTIFICATE_AUTHORITY": bundle}}
	e.awsIssuer.HTTP = &http.Client{Transport: toServer{to: to}}
	e.epoch.current.Store(c)
	return issuerRig{e: e, c: c, kind: integration.KindAWS, ready: "builder", waiting: "planner",
		says: "Untrusted certificate"}
}

// A SEAT'S AWS SERVER IS SIGNED WITH ITS OWN SESSION: the seat on at AWS gets a
// child of the recognised server that signs as it, holding no header, and a
// seat that is not on gets none.
func TestASeatsAWSServerIsSignedWithItsOwnSession(t *testing.T) {
	t.Parallel()
	r := awsRig(t, false)
	for _, tc := range []struct {
		handle string
		signed bool
	}{{"builder", true}, {"planner", false}} {
		seat := r.c.Org.AgentSeatByHandle(tc.handle)
		env, tokens := r.e.seatResolver(t.Context(), r.c, seat, toolTokenLife)
		specs, outcomes := seatSpecs(r.c, seat, env, &seatMint{kinds: r.e.mintedFor(r.c, tc.handle), token: tokens})
		var signer awsint.MCPSigner
		for _, spec := range specs {
			if s, ok := spec.Signer.(awsint.MCPSigner); ok {
				signer = s
				if len(spec.Headers) != 0 {
					t.Errorf("%s: the signed server carries headers %v", tc.handle, spec.Headers)
				}
			}
		}
		if got := signer != (awsint.MCPSigner{}); got != tc.signed || len(outcomes) != 0 {
			t.Errorf("%s: signed %v, want %v (outcomes %v)", tc.handle, got, tc.signed, outcomes)
		}
	}
}

// EACH ISSUER IS ASKED FOR ITS OWN LIVES: one that declares none keeps the hour
// the shared paths have always asked (a launch 50 minutes, a tool or a refresh
// 10), and AWS's 15-minute session is asked for 10 at a launch and 5 at a
// tool's start, so a session never has to outlast what AWS issues.
func TestEachIssuerIsAskedForItsOwnLives(t *testing.T) {
	t.Parallel()
	gh, aws := livesOf(githubSeatIssuer{}), livesOf(awsSeatIssuer{})
	for _, tc := range []struct {
		name      string
		lives     issuedLives
		ask, want time.Duration
	}{
		{"github at a launch", gh, runTokenLife, 50 * time.Minute},
		{"github at a tool's start", gh, toolTokenLife, 10 * time.Minute},
		{"github at a refresh", gh, refreshTokenLife, 10 * time.Minute},
		{"aws at a launch", aws, runTokenLife, 10 * time.Minute},
		{"aws at a tool's start", aws, toolTokenLife, 5 * time.Minute},
		{"aws at a refresh", aws, refreshTokenLife, 5 * time.Minute},
	} {
		if got := tc.lives.ask(tc.ask); got != tc.want {
			t.Errorf("%s: asked for %v, want %v", tc.name, got, tc.want)
		}
	}
	if gh != defaultLives || aws.lifetime != 15*time.Minute || aws.renewBefore != 5*time.Minute {
		t.Errorf("lives: github %+v, aws %+v", gh, aws)
	}
}

// A ROLE IAM IS STILL PROPAGATING SETTLES: STS refusing to assume a seat's role
// is told as the integration still settling, which a wait clears, never as a
// failure a person owes a step for.
func TestAnAWSRoleStillPropagatingSettles(t *testing.T) {
	t.Parallel()
	r := awsRigIn(t, "settling")
	issuer := r.issuerOf(t)
	_, tokens := r.e.seatResolver(t.Context(), r.c, r.c.Org.AgentSeatByHandle("builder"), toolTokenLife)
	if v, _ := tokens.Lookup(issuer.variable(r.c, "builder")); v != "" {
		t.Fatal("a role STS refused was issued a session")
	}
	if !tokens.settling() || tokens.refused() {
		t.Errorf("settling = %v, refused = %v: want the role settling", tokens.settling(), tokens.refused())
	}
}
