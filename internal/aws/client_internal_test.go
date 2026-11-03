package aws

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sigv4"
)

const (
	testAnchor      = "arn:aws:rolesanywhere:us-east-2:123456789012:trust-anchor/0f0e0d0c-0b0a-4908-8706-050403020100"
	testProvProfile = "arn:aws:rolesanywhere:us-east-2:123456789012:profile/00010203-0405-4607-8809-0a0b0c0d0e0f"
	testSeatProfile = "arn:aws:rolesanywhere:us-east-2:123456789012:profile/10010203-0405-4607-8809-0a0b0c0d0e0f"
	testProv        = "arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-provisioner"
	testBroker      = "arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-broker"
)

// testProvisioning is acme's block in us-east-2, its stack output pasted.
func testProvisioning(t *testing.T) *config.AWSProvisioning {
	t.Helper()
	return &config.AWSProvisioning{Slug: "acme", HomeRegion: "us-east-2", CertificateAuthority: mustBundle(t),
		Connection: strings.Join([]string{testAnchor, testProvProfile, testProv, testSeatProfile, testBroker}, ",")}
}

func mustConnection(t *testing.T) *Connection {
	t.Helper()
	conn, err := NewConnection(testProvisioning(t))
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestNewConnectionReadsTheProvisioningBlock(t *testing.T) {
	t.Parallel()
	conn := mustConnection(t)
	if conn.Company != "acme" || conn.Region != "us-east-2" || conn.Account != "123456789012" ||
		conn.Partition != "aws" || conn.BrokerRoleARN != testBroker || conn.CA == nil {
		t.Fatalf("%+v", conn)
	}
	for name, mutate := range map[string]func(*config.AWSProvisioning){
		"no output":     func(p *config.AWSProvisioning) { p.Connection = " " },
		"another slug":  func(p *config.AWSProvisioning) { p.Slug = "other" },
		"another home":  func(p *config.AWSProvisioning) { p.HomeRegion = "eu-west-1" },
		"no key":        func(p *config.AWSProvisioning) { p.CertificateAuthority = "not a bundle" },
		"not a slug":    func(p *config.AWSProvisioning) { p.Slug = "Acme" },
		"not a region":  func(p *config.AWSProvisioning) { p.HomeRegion = "ohio" },
		"two ARNs only": func(p *config.AWSProvisioning) { p.Connection = testAnchor + "," + testProv },
	} {
		p := testProvisioning(t)
		mutate(p)
		_, err := NewConnection(p)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if (name == "no output") != errors.Is(err, ErrNotConnected) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

var x509Auth = regexp.MustCompile(`^AWS4-X509-ECDSA-SHA256 Credential=(\d+)/(\d{8})/us-east-2/rolesanywhere/aws4_request, SignedHeaders=([a-z0-9;-]+), Signature=([0-9a-f]+)$`)

// A fake of Roles Anywhere that verifies every signature the way the service
// does, then STS, whose AssumeRole must come from the broker's session.
func TestASeatSessionGoesThroughTheBrokerUnderTheSeatsName(t *testing.T) {
	t.Parallel()
	conn := mustConnection(t)
	var throttled atomic.Int32
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/sessions":
			if throttled.Add(1) == 1 {
				w.Header().Set("X-Amzn-Errortype", "ThrottlingException")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			der, _ := base64.StdEncoding.DecodeString(r.Header.Get(sigv4.HeaderX509))
			leaf, err := x509.ParseCertificate(der)
			if err != nil || leaf.CheckSignatureFrom(conn.CA.Cert) != nil {
				t.Error("the leaf is not the company CA's")
			}
			m := x509Auth.FindStringSubmatch(r.Header.Get("Authorization"))
			if m == nil {
				t.Fatalf("Authorization %q", r.Header.Get("Authorization"))
			}
			if serial, _ := new(big.Int).SetString(m[1], 10); serial.Cmp(leaf.SerialNumber) != 0 {
				t.Error("the credential is not the leaf's serial")
			}
			at, _ := time.Parse(sigv4.DateFormat, r.Header.Get("X-Amz-Date"))
			hdr := http.Header{"Content-Type": r.Header.Values("Content-Type"), "X-Amz-Date": r.Header.Values("X-Amz-Date"),
				sigv4.HeaderX509: r.Header.Values(sigv4.HeaderX509)}
			canonical, _ := sigv4.CanonicalRequest(sigv4.Request{Method: r.Method, Host: r.Host, Path: r.URL.Path, Header: hdr, PayloadHash: sigv4.PayloadHash(body)})
			digest := sha256.Sum256([]byte(sigv4.StringToSign(sigv4.AlgX509ECDSA, at, sigv4.Scope(at, "us-east-2", "rolesanywhere"), canonical)))
			sig, _ := hex.DecodeString(m[4])
			if !ecdsa.VerifyASN1(leaf.PublicKey.(*ecdsa.PublicKey), digest[:], sig) {
				t.Error("the signature does not verify")
			}
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			if leaf.Subject.CommonName != "crewlet-acme-seat-sre" || in["roleArn"] != testBroker || in["profileArn"] != testSeatProfile {
				t.Errorf("a seat must open the broker through the seats' profile, got CN %q and %v", leaf.Subject.CommonName, in)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"credentialSet": []any{map[string]any{
				"credentials":    map[string]any{"accessKeyId": "ASIABROKEREXAMPLE001", "secretAccessKey": "s", "sessionToken": "broker-token", "expiration": exp.Format(time.RFC3339)},
				"sourceIdentity": "CN=crewlet-acme-seat-sre",
			}}})
		default:
			form, _ := url.ParseQuery(string(body))
			if form.Get("Action") != "AssumeRole" || form.Get("RoleSessionName") != "crewlet-acme-seat-sre" || form.Get("DurationSeconds") != "3600" {
				t.Errorf("AssumeRole %v", form)
			}
			if r.Header.Get("X-Amz-Security-Token") != "broker-token" {
				t.Error("AssumeRole was not signed by the broker's session")
			}
			_, _ = io.WriteString(w, `<AssumeRoleResponse><AssumeRoleResult><SourceIdentity>CN=crewlet-acme-seat-sre</SourceIdentity><Credentials><AccessKeyId>ASIASEATEXAMPLE00001</AccessKeyId><SecretAccessKey>x</SecretAccessKey><SessionToken>seat-token</SessionToken><Expiration>`+exp.Format(time.RFC3339)+`</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`)
		}
	}))
	defer srv.Close()
	c := newClient(conn, srv.Client(), srv.URL)
	c.ra.wait = func(int) time.Duration { return time.Millisecond }
	creds, source, err := c.SeatSession(context.Background(), "sre", "arn:aws:iam::123456789012:role/crewlet/acme/seat/crewlet-acme-sre-0a1b", SeatSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if creds.SessionToken != "seat-token" || source != "CN=crewlet-acme-seat-sre" || throttled.Load() != 2 {
		t.Fatalf("creds %+v source %q attempts %d", creds, source, throttled.Load())
	}
}

func TestRolesAnywhereRefusalsAreReadAndNotRetried(t *testing.T) {
	t.Parallel()
	conn := mustConnection(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Amzn-Errortype", "AccessDeniedException:http://internal")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Unable to assume role"}`)
	}))
	defer srv.Close()
	ra := newRolesAnywhere(srv.Client(), "us-east-2")
	ra.endpoint = func(string) string { return srv.URL }
	_, err := ra.createSession(context.Background(), conn.CA, "crewlet-acme-provisioner", LeafECDSA, testProvProfile, testProv, testAnchor, 3600)
	if api, ok := err.(*APIError); !ok || api.Code != "AccessDeniedException" || api.Message != "Unable to assume role" || calls.Load() != 1 {
		t.Fatalf("err %v after %d calls", err, calls.Load())
	}
	if !(&APIError{Status: 400, Code: "ThrottlingException"}).transient() || (&APIError{Status: 403, Code: "AccessDeniedException"}).transient() {
		t.Fatal("transient misclassifies")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("AccessDeniedException")) {
		t.Fatal("the refusal lost its code")
	}
}
