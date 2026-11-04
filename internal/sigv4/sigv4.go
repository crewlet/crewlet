// Package sigv4 signs AWS requests: Signature Version 4 with an access key,
// and the IAM Roles Anywhere variant that signs the same canonical request
// with an X.509 certificate's own RSA or ECDSA key.
//
// It exists because AWS ships the X.509 form (Roles Anywhere's CreateSession)
// in no SDK, and the one client AWS publishes for it needs cgo for its PKCS #11
// and TPM signers, which a CGO_ENABLED=0 build cannot link. Both forms share
// one canonical request here, so the two can never drift apart, and the access
// key form also signs what the SDK does not reach: each request to AWS's MCP
// Server, which takes no bearer header. The SDK's own signer stays where the
// SDK sends the request.
//
// A leaf: the standard library only. Its tests run AWS's published
// Signature Version 4 test suite (testdata/aws4_testsuite, with AWS's NOTICE
// and LICENSE beside it) and the Roles Anywhere guide's own example request.
package sigv4

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	// DateFormat is X-Amz-Date's ISO 8601 basic form.
	DateFormat = "20060102T150405Z"
	dayFormat  = "20060102"

	// AlgHMAC signs with an access key's secret.
	AlgHMAC = "AWS4-HMAC-SHA256"
	// AlgX509RSA signs with an X.509 certificate's RSA key (Roles Anywhere).
	AlgX509RSA = "AWS4-X509-RSA-SHA256"
	// AlgX509ECDSA signs with an X.509 certificate's ECDSA key (Roles Anywhere).
	AlgX509ECDSA = "AWS4-X509-ECDSA-SHA256"

	// HeaderX509 carries the signing certificate as base64 DER; it must be signed.
	HeaderX509 = "X-Amz-X509"
)

// Credentials is an access key, with the session token of temporary credentials.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Request is what signing reads. The body is already hashed.
type Request struct {
	Method string
	Host   string
	// Path is unescaped; empty means "/".
	Path   string
	Query  url.Values
	Header http.Header // every header to sign besides Host
	// PayloadHash is the lowercase hex SHA-256 of the body (PayloadHash).
	PayloadHash string
	// S3 escapes the path once; every other service escapes it twice.
	S3 bool
}

// PayloadHash is the lowercase hex SHA-256 of a body.
func PayloadHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// EncodeURI escapes per RFC 3986: only A-Z a-z 0-9 - _ . ~ stay literal,
// never url.QueryEscape's "+" for a space, which AWS reads as a plus.
func EncodeURI(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// URIPath is the path as sent on the wire: escaped once.
func URIPath(path string) string {
	if path == "" {
		return "/"
	}
	return EncodeURI(path, true)
}

// canonicalURI escapes the wire path again for every service but S3, as
// AWS's own signers do.
func canonicalURI(path string, s3 bool) string {
	if s3 {
		return URIPath(path)
	}
	return EncodeURI(URIPath(path), true)
}

// CanonicalQuery sorts by name, then value, and escapes both.
func CanonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, EncodeURI(k, false)+"="+EncodeURI(v, false))
		}
	}
	return strings.Join(parts, "&")
}

// CanonicalRequest returns the canonical request and its signed-headers list.
// A header's value is trimmed and its inner runs of spaces collapsed; a
// repeated header joins its values with commas in the order they came.
func CanonicalRequest(r Request) (string, string) {
	vals := map[string][]string{"host": {r.Host}}
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if lk == "host" {
			continue
		}
		vals[lk] = append(vals[lk], v...)
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	var hdrs strings.Builder
	for _, k := range names {
		trimmed := make([]string, len(vals[k]))
		for i, v := range vals[k] {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		hdrs.WriteString(k + ":" + strings.Join(trimmed, ",") + "\n")
	}
	signed := strings.Join(names, ";")
	return strings.Join([]string{
		r.Method, canonicalURI(r.Path, r.S3), CanonicalQuery(r.Query), hdrs.String(), signed, r.PayloadHash,
	}, "\n"), signed
}

// Scope is the credential scope: date/region/service/aws4_request.
func Scope(t time.Time, region, service string) string {
	return t.UTC().Format(dayFormat) + "/" + region + "/" + service + "/aws4_request"
}

// StringToSign is the algorithm, the time, the scope and the hashed canonical request.
func StringToSign(alg string, t time.Time, scope, canonical string) string {
	return alg + "\n" + t.UTC().Format(DateFormat) + "\n" + scope + "\n" + PayloadHash([]byte(canonical))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// Sign returns the Authorization header for an access key. The caller has
// already put X-Amz-Date, and the session token if any, in r.Header.
func Sign(r Request, c Credentials, region, service string, t time.Time) string {
	scope := Scope(t, region, service)
	canonical, signed := CanonicalRequest(r)
	k := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), t.UTC().Format(dayFormat))
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, StringToSign(AlgHMAC, t, scope, canonical)))
	return AlgHMAC + " Credential=" + c.AccessKeyID + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig
}

// X509Algorithm names the algorithm a key's type requires.
func X509Algorithm(pub crypto.PublicKey) (string, error) {
	switch pub.(type) {
	case *ecdsa.PublicKey:
		return AlgX509ECDSA, nil
	case *rsa.PublicKey:
		return AlgX509RSA, nil
	}
	return "", fmt.Errorf("sigv4: Roles Anywhere signs with an RSA or ECDSA key, not %T", pub)
}

// SignX509 returns the Authorization header for a Roles Anywhere request,
// signed with the certificate's key over SHA-256 (PKCS #1 v1.5 for RSA, ASN.1
// for ECDSA). The credential is the certificate's serial number in decimal.
// The caller has already put X-Amz-Date and X-Amz-X509 in r.Header.
func SignX509(r Request, cert *x509.Certificate, key crypto.Signer, region string, t time.Time) (string, error) {
	if cert == nil || key == nil {
		return "", errors.New("sigv4: no certificate or key")
	}
	if r.Header.Get(HeaderX509) != X509Header(cert) {
		return "", errors.New("sigv4: X-Amz-X509 must carry the signing certificate, and be signed")
	}
	alg, err := X509Algorithm(key.Public())
	if err != nil {
		return "", err
	}
	if certAlg, _ := X509Algorithm(cert.PublicKey); certAlg != alg {
		return "", errors.New("sigv4: the key is not the certificate's")
	}
	scope := Scope(t, region, "rolesanywhere")
	canonical, signed := CanonicalRequest(r)
	digest := sha256.Sum256([]byte(StringToSign(alg, t, scope, canonical)))
	sig, err := key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return "", err
	}
	return alg + " Credential=" + cert.SerialNumber.String() + "/" + scope +
		", SignedHeaders=" + signed + ", Signature=" + hex.EncodeToString(sig), nil
}

// X509Header is the X-Amz-X509 value for a certificate.
func X509Header(cert *x509.Certificate) string { return base64.StdEncoding.EncodeToString(cert.Raw) }
