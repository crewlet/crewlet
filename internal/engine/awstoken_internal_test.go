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
// planner's is not yet: Roles Anywhere opens the broker, and STS assumes the
// seat's role, or refuses it when refused.
func awsRig(t *testing.T, refused bool) issuerRig {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		_, _ = io.ReadAll(r.Body)
		if r.URL.Path == "/sessions" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"credentialSet": []any{map[string]any{
				"credentials": map[string]any{"accessKeyId": "ASIABROKER", "secretAccessKey": "s",
					"sessionToken": "broker", "expiration": exp}}}})
			return
		}
		if refused {
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
		says: "AccessDenied"}
}
