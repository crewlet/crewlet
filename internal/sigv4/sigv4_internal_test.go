package sigv4

import (
	"bufio"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The AWS Signature Version 4 test suite's fixed inputs.
var (
	suiteTime  = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	suiteCreds = Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
)

// parseSuiteRequest reads a test-suite .req file: a request line, headers, a
// blank line and an optional body.
func parseSuiteRequest(t *testing.T, path string) (Request, []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head, body, _ := strings.Cut(string(raw), "\n\n")
	sc := bufio.NewScanner(strings.NewReader(head))
	sc.Scan()
	parts := strings.SplitN(sc.Text(), " ", 3)
	if len(parts) != 3 {
		t.Fatalf("%s: bad request line %q", path, sc.Text())
	}
	target, _ := url.Parse(parts[1])
	q, err := url.ParseQuery(target.RawQuery)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	r := Request{Method: parts[0], Path: target.Path, Query: q, Header: http.Header{}}
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			t.Fatalf("%s: bad header %q", path, sc.Text())
		}
		if strings.EqualFold(k, "host") {
			r.Host = v
			continue
		}
		r.Header[k] = append(r.Header[k], v)
	}
	r.PayloadHash = PayloadHash([]byte(body))
	return r, []byte(body)
}

// AWS'S PUBLISHED SIGNATURE VERSION 4 TEST SUITE, every vector that needs no
// path normalization (the copy botocore ships, Apache 2.0, with AWS's NOTICE
// and LICENSE in testdata). A signer that agrees only with itself certifies nothing.
func TestAWSSignatureV4TestSuite(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("testdata/aws4_testsuite")
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join("testdata/aws4_testsuite", e.Name()))
		}
	}
	if err != nil || len(dirs) < 20 {
		t.Fatalf("expected the suite's vectors in testdata, found %d (%v)", len(dirs), err)
	}
	for _, dir := range dirs {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, _ := parseSuiteRequest(t, filepath.Join(dir, name+".req"))
			creds := suiteCreds
			if sreq, err := os.ReadFile(filepath.Join(dir, name+".sreq")); err == nil {
				// The session-token vector carries its token only in the signed request.
				m := regexp.MustCompile(`(?m)^X-Amz-Security-Token:(.+)$`).FindStringSubmatch(string(sreq))
				if m == nil {
					t.Fatal("no token in the .sreq")
				}
				creds.SessionToken = strings.TrimSpace(m[1])
				r.Header.Set("X-Amz-Security-Token", creds.SessionToken)
			}
			want, err := os.ReadFile(filepath.Join(dir, name+".authz"))
			if err != nil {
				t.Fatal(err)
			}
			if got := Sign(r, creds, "us-east-1", "service", suiteTime); got != strings.TrimSpace(string(want)) {
				t.Fatalf("\n got %s\nwant %s", got, want)
			}
		})
	}
}

// The IAM example in AWS's signing guide, and an S3 request with a session
// token checked against aws-sdk-go-v2's signer (S3 escapes paths once).
func TestReferenceSignatures(t *testing.T) {
	t.Parallel()
	iam := Request{Method: "GET", Host: "iam.amazonaws.com", Path: "/",
		Query:       url.Values{"Action": {"ListUsers"}, "Version": {"2010-05-08"}},
		Header:      http.Header{"X-Amz-Date": {"20150830T123600Z"}, "Content-Type": {"application/x-www-form-urlencoded; charset=utf-8"}},
		PayloadHash: PayloadHash(nil)}
	if got, want := Sign(iam, suiteCreds, "us-east-1", "iam", suiteTime),
		"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"; got != want {
		t.Errorf("IAM example\n got %s\nwant %s", got, want)
	}
	s3 := Request{Method: "GET", Host: "s3.us-east-2.amazonaws.com", Path: "/crewlet-probe-x", S3: true,
		Query: url.Values{"list-type": {"2"}, "prefix": {"a b/c"}},
		Header: http.Header{"X-Amz-Date": {"20150830T123600Z"}, "X-Amz-Content-Sha256": {PayloadHash(nil)},
			"X-Amz-Security-Token": {"FQoGZXIvYXdzEXAMPLETOKEN"}},
		PayloadHash: PayloadHash(nil)}
	creds := suiteCreds
	creds.SessionToken = "FQoGZXIvYXdzEXAMPLETOKEN"
	if got, want := Sign(s3, creds, "us-east-2", "s3", suiteTime),
		"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-2/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=6ce228866c4afbae965e31edf9554d7e34b1fadf4af3cdfa1601552ec51ab669"; got != want {
		t.Errorf("S3 with a token\n got %s\nwant %s", got, want)
	}
}

// The Roles Anywhere guide's own canonical request example, byte for byte.
func TestRolesAnywhereDocsCanonicalRequest(t *testing.T) {
	t.Parallel()
	r := Request{Method: "POST", Host: "rolesanywhere.us-east-1.amazonaws.com", Path: "/sessions",
		Header: http.Header{"Content-Type": {"application/json"}, "X-Amz-Date": {"20211103T120000Z"},
			"X-Amz-X509": {"{base64-encoded DER data}"}},
		PayloadHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
	want := "POST\n/sessions\n\n" +
		"content-type:application/json\nhost:rolesanywhere.us-east-1.amazonaws.com\nx-amz-date:20211103T120000Z\nx-amz-x509:{base64-encoded DER data}\n\n" +
		"content-type;host;x-amz-date;x-amz-x509\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	got, signed := CanonicalRequest(r)
	if got != want || signed != "content-type;host;x-amz-date;x-amz-x509" {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	at := time.Date(2020, 11, 5, 0, 0, 0, 0, time.UTC)
	if s := Scope(at, "us-east-1", "rolesanywhere"); s != "20201105/us-east-1/rolesanywhere/aws4_request" {
		t.Fatalf("scope %q", s)
	}
}

func selfSigned(t *testing.T, key crypto.Signer, serial *big.Int) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "crewlet-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

var authRE = regexp.MustCompile(`^(AWS4-X509-(?:RSA|ECDSA)-SHA256) Credential=(\d+)/(\d{8}/[a-z0-9-]+/rolesanywhere/aws4_request), SignedHeaders=([a-z0-9;-]+), Signature=([0-9a-f]+)$`)

// TestSignX509RSAAndECDSA signs the guide's example request with each key
// type and verifies the signature the way the service does. AWS publishes no
// fixed X.509 signature (ECDSA is randomized), so the live probe proves both.
func TestSignX509RSAAndECDSA(t *testing.T) {
	t.Parallel()
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, tc := range []struct {
		alg    string
		key    crypto.Signer
		verify func(pub crypto.PublicKey, digest, sig []byte) bool
	}{
		{AlgX509ECDSA, ecKey, func(pub crypto.PublicKey, d, s []byte) bool { return ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), d, s) }},
		{AlgX509RSA, rsaKey, func(pub crypto.PublicKey, d, s []byte) bool {
			return rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, d, s) == nil
		}},
	} {
		t.Run(tc.alg, func(t *testing.T) {
			// The guide's example serial, larger than 64 bits, must print in decimal.
			serial, _ := new(big.Int).SetString("11111222223333344444", 10)
			cert := selfSigned(t, tc.key, serial)
			at := time.Date(2021, 11, 3, 12, 0, 0, 0, time.UTC)
			r := Request{Method: "POST", Host: "rolesanywhere.us-east-1.amazonaws.com", Path: "/sessions",
				Header:      http.Header{"Content-Type": {"application/json"}, "X-Amz-Date": {at.Format(DateFormat)}, HeaderX509: {X509Header(cert)}},
				PayloadHash: PayloadHash([]byte(`{"durationSeconds":3600}`))}
			auth, err := SignX509(r, cert, tc.key, "us-east-1", at)
			if err != nil {
				t.Fatal(err)
			}
			m := authRE.FindStringSubmatch(auth)
			if m == nil || m[1] != tc.alg || m[2] != cert.SerialNumber.String() || m[3] != "20211103/us-east-1/rolesanywhere/aws4_request" ||
				m[4] != "content-type;host;x-amz-date;x-amz-x509" {
				t.Fatalf("Authorization %q", auth)
			}
			canonical, _ := CanonicalRequest(r)
			digest := sha256.Sum256([]byte(StringToSign(tc.alg, at, Scope(at, "us-east-1", "rolesanywhere"), canonical)))
			sig, _ := hex.DecodeString(m[5])
			if !tc.verify(cert.PublicKey, digest[:], sig) {
				t.Fatal("the signature does not verify against the certificate's key")
			}
			// A changed signed header must break it.
			r.Header.Set("X-Amz-Date", at.Add(time.Second).Format(DateFormat))
			c2, _ := CanonicalRequest(r)
			d2 := sha256.Sum256([]byte(StringToSign(tc.alg, at, Scope(at, "us-east-1", "rolesanywhere"), c2)))
			if tc.verify(cert.PublicKey, d2[:], sig) {
				t.Fatal("the signature survived a changed header")
			}
		})
	}
}

func TestSignX509Refusals(t *testing.T) {
	t.Parallel()
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	cert := selfSigned(t, ecKey, big.NewInt(7))
	r := Request{Method: "POST", Host: "h", Path: "/sessions", Header: http.Header{"X-Amz-Date": {"20211103T120000Z"}}}
	if _, err := SignX509(r, cert, ecKey, "us-east-1", time.Now()); err == nil {
		t.Error("signed without X-Amz-X509")
	}
	r.Header.Set(HeaderX509, X509Header(cert))
	if _, err := SignX509(r, cert, other, "us-east-1", time.Now()); err == nil {
		t.Error("signed with a key of another type than the certificate's")
	}
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := SignX509(r, cert, edKey, "us-east-1", time.Now()); err == nil {
		t.Error("signed with an Ed25519 key")
	}
}

func TestEncodingRules(t *testing.T) {
	t.Parallel()
	if got := EncodeURI("a b/c~-_.ሴ=", false); got != "a%20b%2Fc~-_.%E1%88%B4%3D" {
		t.Errorf("EncodeURI %q", got)
	}
	if got := canonicalURI("/a b", false); got != "/a%2520b" {
		t.Errorf("non-S3 paths are escaped twice, got %q", got)
	}
	if got := canonicalURI("/a b", true); got != "/a%20b" {
		t.Errorf("S3 paths are escaped once, got %q", got)
	}
	if got := CanonicalQuery(url.Values{"b": {"2", "1"}, "A": {"x"}}); got != "A=x&b=1&b=2" {
		t.Errorf("CanonicalQuery %q", got)
	}
}
