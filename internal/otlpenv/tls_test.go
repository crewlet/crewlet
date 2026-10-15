package otlpenv_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/otlpenv"
)

// pemFiles writes a self-signed certificate and its key as PEM files, and
// returns their paths.
func pemFiles(t *testing.T, name string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// THE COMPRESSION RESOLVES PER SIGNAL, as the SDK applies it: the signal's own
// variable, else the base, else none — in any case.
//
// Mutation: read the base variable alone and the per-signal case goes red.
func TestTheCompressionResolvesPerSignal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  map[string]string
		want otlpenv.Compression
	}{
		{"nothing says, none", nil, otlpenv.NoCompression},
		{"the base", map[string]string{otlpenv.CompressionVar: "gzip"}, otlpenv.Gzip},
		{"in any case", map[string]string{otlpenv.CompressionVar: "GZIP"}, otlpenv.Gzip},
		{"the signal's own wins", map[string]string{
			otlpenv.CompressionVar:          "gzip",
			otlpenv.Traces.CompressionVar(): "none",
		}, otlpenv.NoCompression},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolve(t, otlpenv.Traces, tc.env).Compression; got != tc.want {
				t.Errorf("compression = %q, want %q", got, tc.want)
			}
		})
	}
}

// A COMPRESSION THE EXPORTER DOES NOT APPLY IS REFUSED BY ITS VARIABLE. The
// SDK reads any value but `gzip` as none, so a typo exported uncompressed.
func TestAnUnknownCompressionIsRefused(t *testing.T) {
	t.Parallel()
	for _, name := range []string{otlpenv.CompressionVar, otlpenv.Metrics.CompressionVar()} {
		_, err := otlpenv.Resolve(envOf(map[string]string{name: "zstd"}), otlpenv.Metrics)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=zstd: %v, want a refusal naming the variable", name, err)
		}
	}
}

// TLS MATERIAL RESOLVES AS THE SDK APPLIES IT: the signal's certificate over
// the base one, and the signal's client pair over the base pair, a pair taken
// whole from one level. And it reaches a client: the configuration carries the
// authority and the client certificate it names.
//
// Mutation: prefer the base certificate to the signal's and the first case goes
// red; take a pair across levels and the mixed case does.
func TestTLSMaterialResolvesPerSignal(t *testing.T) {
	t.Parallel()
	baseCA, _ := pemFiles(t, "base-ca")
	signalCA, _ := pemFiles(t, "traces-ca")
	baseCert, baseKey := pemFiles(t, "base-client")
	signalCert, signalKey := pemFiles(t, "traces-client")
	const endpoint = "https://collector.example.com:4318"

	d := resolve(t, otlpenv.Traces, map[string]string{
		otlpenv.EndpointVar:                   endpoint,
		otlpenv.CertificateVar:                baseCA,
		otlpenv.Traces.CertificateVar():       signalCA,
		otlpenv.ClientCertificateVar:          baseCert,
		otlpenv.ClientKeyVar:                  baseKey,
		otlpenv.Traces.ClientCertificateVar(): signalCert,
		otlpenv.Traces.ClientKeyVar():         signalKey,
	})
	if d.TLS.Certificate != signalCA || d.TLS.ClientCertificate != signalCert ||
		d.TLS.ClientKey != signalKey {
		t.Errorf("traces took %+v, want its own certificate and pair", d.TLS)
	}

	// Another signal reads the base.
	m := resolve(t, otlpenv.Metrics, map[string]string{
		otlpenv.EndpointVar:                   endpoint,
		otlpenv.CertificateVar:                baseCA,
		otlpenv.ClientCertificateVar:          baseCert,
		otlpenv.ClientKeyVar:                  baseKey,
		otlpenv.Traces.ClientCertificateVar(): signalCert,
		otlpenv.Traces.ClientKeyVar():         signalKey,
	})
	if m.TLS.Certificate != baseCA || m.TLS.ClientCertificate != baseCert {
		t.Errorf("metrics took %+v, want the base material", m.TLS)
	}

	cfg, err := d.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.RootCAs == nil || len(cfg.Certificates) != 1 {
		t.Fatalf("the configuration carries none of the material: %+v", cfg)
	}

	// NO MATERIAL, NO CONFIGURATION: an https endpoint with nothing named
	// is the client's own default — the system's roots.
	plain := resolve(t, otlpenv.Traces, map[string]string{otlpenv.EndpointVar: endpoint})
	if cfg, err := plain.Config(); cfg != nil || err != nil {
		t.Errorf("an endpoint naming no material built %+v (%v)", cfg, err)
	}
	if !plain.Secure() || resolve(t, otlpenv.Traces, map[string]string{
		otlpenv.EndpointVar: "http://collector.example.com"}).Secure() {
		t.Error("the scheme does not decide TLS")
	}
}

// MATERIAL THAT CAN NEVER BE USED IS REFUSED BY THE VARIABLE THAT NAMED IT.
// The SDK reports such a file through its error handler and exports without
// it — to a collector that refuses the handshake, or with no client
// certificate — and the SDK's two exporters disagree about material beside an
// http:// endpoint: the HTTP one refuses to start and the gRPC one encrypts the
// connection the scheme said was plaintext.
//
// Mutation: drop any one check and its case goes red.
func TestTLSMaterialThatCannotBeUsedIsRefused(t *testing.T) {
	t.Parallel()
	ca, _ := pemFiles(t, "ca")
	cert, key := pemFiles(t, "client")
	_, otherKey := pemFiles(t, "other")
	notPEM := filepath.Join(t.TempDir(), "not.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	const secure, plain = "https://collector.example.com", "http://collector.example.com"
	for _, tc := range []struct {
		name  string
		env   map[string]string
		names string
	}{
		{"a certificate that is not there", map[string]string{
			otlpenv.EndpointVar: secure, otlpenv.CertificateVar: "/nonexistent/ca.pem"},
			otlpenv.CertificateVar},
		{"a certificate that is not PEM", map[string]string{
			otlpenv.EndpointVar: secure, otlpenv.Traces.CertificateVar(): notPEM},
			otlpenv.Traces.CertificateVar()},
		{"a client certificate without its key", map[string]string{
			otlpenv.EndpointVar: secure, otlpenv.ClientCertificateVar: cert},
			otlpenv.ClientKeyVar},
		{"a client key without its certificate", map[string]string{
			otlpenv.EndpointVar: secure, otlpenv.Traces.ClientKeyVar(): key},
			otlpenv.Traces.ClientCertificateVar()},
		{"a client certificate and somebody else's key", map[string]string{
			otlpenv.EndpointVar: secure, otlpenv.ClientCertificateVar: cert,
			otlpenv.ClientKeyVar: otherKey},
			otlpenv.ClientCertificateVar},
		{"a certificate beside an http endpoint", map[string]string{
			otlpenv.EndpointVar: plain, otlpenv.CertificateVar: ca},
			otlpenv.CertificateVar},
		{"a client pair beside an http endpoint", map[string]string{
			otlpenv.EndpointVar: plain, otlpenv.ClientCertificateVar: cert,
			otlpenv.ClientKeyVar: key},
			otlpenv.ClientCertificateVar},
		{"insecure beside an https endpoint", map[string]string{
			otlpenv.EndpointVar: secure, otlpenv.InsecureVar: "true"},
			otlpenv.InsecureVar},
		{"secure beside an http endpoint", map[string]string{
			otlpenv.EndpointVar: plain, otlpenv.Traces.InsecureVar(): "false"},
			otlpenv.Traces.InsecureVar()},
		{"insecure that is not a boolean", map[string]string{
			otlpenv.EndpointVar: plain, otlpenv.InsecureVar: "yes"},
			otlpenv.InsecureVar},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := otlpenv.Resolve(envOf(tc.env), otlpenv.Traces)
			if err == nil || !strings.Contains(err.Error(), tc.names) {
				t.Errorf("%v, want a refusal naming %s", err, tc.names)
			}
		})
	}

	// AND AN INSECURE THAT AGREES WITH THE SCHEME IS NO ERROR: it says
	// nothing the scheme does not.
	for endpoint, insecure := range map[string]string{plain: "TRUE", secure: "false"} {
		if _, err := otlpenv.Resolve(envOf(map[string]string{
			otlpenv.EndpointVar: endpoint, otlpenv.InsecureVar: insecure,
		}), otlpenv.Traces); err != nil {
			t.Errorf("%s=%s beside %s was refused: %v", otlpenv.InsecureVar, insecure, endpoint, err)
		}
	}
}

// THE NEW VARIABLE NAMES ARE THE SPECIFICATION'S, spelled out for the reason
// TestTheVariableNamesAreTheSpecifications gives.
func TestTheTLSAndCompressionNamesAreTheSpecifications(t *testing.T) {
	t.Parallel()
	got := []string{
		otlpenv.CompressionVar, otlpenv.CertificateVar, otlpenv.ClientCertificateVar,
		otlpenv.ClientKeyVar, otlpenv.InsecureVar,
		otlpenv.Logs.CompressionVar(), otlpenv.Logs.CertificateVar(),
		otlpenv.Logs.ClientCertificateVar(), otlpenv.Logs.ClientKeyVar(),
		otlpenv.Logs.InsecureVar(),
	}
	want := []string{
		"OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_EXPORTER_OTLP_LOGS_COMPRESSION", "OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_LOGS_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_LOGS_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_LOGS_INSECURE",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s, want %s", got[i], want[i])
		}
	}
}
