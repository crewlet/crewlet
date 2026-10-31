package jetstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// AN EXTERNAL SERVER THAT AUTHENTICATES NOBODY IS REFUSED.
//
// Whatever this node presents: a server that does not ask for a token admits
// it and everybody else, so the dial judges what the SERVER announces in its
// INFO — admitted on auth_required, or on tls_required with tls_verify (every
// client must present a certificate the server verifies), and on nothing
// else. Not on tls_verify alone, which is the mixed mode allow_non_tls where
// a plaintext client presents nothing; and not on a certificate this node
// presents, which a `tls { }` server without verify never asks for. An INFO
// the dial did not read is a refusal, never a pass, and says it read none.
// The refusal names the server by host and port only, so a token in the URL
// never reaches it.
//
// Mutations: skip the check in dial and the anonymous rows dial; exempt a
// presented client certificate and the tls-without-verify row dials; read
// tls_verify without tls_required and the allow_non_tls row is admitted; pass
// an INFO never read and the no-INFO row is admitted; judge one as an
// unreadable line and that row no longer says none was read; name the server
// by cfg.URL and the userinfo row prints its token.
func TestAnExternalServerThatAuthenticatesNobodyIsRefused(t *testing.T) {
	t.Parallel()

	t.Run("the INFO a server announces", func(t *testing.T) {
		t.Parallel()
		const named = "the NATS server at nats://nats-1.example.com:4222"
		// says is what a refusal tells its reader happened: a server that
		// admits anybody, an INFO never read, a first line that is none.
		const anonymous, unread, unreadable = "requires no authentication",
			"read no INFO", "is not an INFO"
		for _, tc := range []struct {
			name     string
			info     []byte
			admitted bool
			says     string
		}{
			{"authentication required", []byte(`INFO {"server_id":"x","auth_required":true,"max_payload":8388608}`), true, ""},
			{"TLS required and verified", []byte(`INFO {"tls_required":true,"tls_verify":true}`), true, ""},
			{"verified but not required (allow_non_tls)", []byte(`INFO {"tls_required":false,"tls_verify":true}`), false, anonymous},
			{"TLS required and not verified", []byte(`INFO {"tls_required":true}`), false, anonymous},
			{"neither", []byte(`INFO {"server_id":"x","max_payload":8388608}`), false, anonymous},
			{"no INFO read", nil, false, unread},
			{"a first line that is no INFO", []byte(`HTTP/1.1 400 Bad Request`), false, unreadable},
			{"an INFO that does not parse", []byte(`INFO {"auth_required":tru`), false, unreadable},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				err := requiresAuthentication(named, tc.info)
				if tc.admitted {
					if err != nil {
						t.Fatalf("refused a server that authenticates: %v", err)
					}
					return
				}
				if !errors.Is(err, ErrUnauthenticatedBroker) {
					t.Fatalf("err = %v, want ErrUnauthenticatedBroker", err)
				}
				if !strings.Contains(err.Error(), named) {
					t.Errorf("the refusal does not name the server: %v", err)
				}
				if !strings.Contains(err.Error(), tc.says) {
					t.Errorf("the refusal does not say %q: %v", tc.says, err)
				}
			})
		}
	})

	t.Run("a live server started anonymous, whatever this node presents", func(t *testing.T) {
		t.Parallel()
		srv := externaltest.Start(t, queue.MaxPayloadBytes, externaltest.Anonymous)
		const secret = "fixture-token-the-server-ignores"
		for name, cfg := range map[string]Config{
			"a token":                    {URL: srv.URL(), Token: secret},
			"a token in the URL":         {URL: "nats://" + secret + "@" + srv.HostPort()},
			"a user and password in URL": {URL: "nats://engine:" + secret + "@" + srv.HostPort()},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				err := refusedOpening(t, cfg)
				if !errors.Is(err, ErrUnauthenticatedBroker) {
					t.Fatalf("err = %v, want ErrUnauthenticatedBroker", err)
				}
				for _, says := range []string{"nats://" + srv.HostPort(), "requires no authentication",
					"configure authorization"} {
					if !strings.Contains(err.Error(), says) {
						t.Errorf("the refusal does not say %q: %v", says, err)
					}
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the refusal carries the credential this node presented: %v", err)
				}
			})
		}
	})

	t.Run("a live server requiring a token", func(t *testing.T) {
		t.Parallel()
		srv := externaltest.Start(t, queue.MaxPayloadBytes)
		newQueueWith(t, Config{URL: srv.URL(), Token: externaltest.Token})
	})

	t.Run("a live TLS server", func(t *testing.T) {
		t.Parallel()
		ca, cert, key := loopbackTLSFiles(t)
		for _, tc := range []struct {
			name     string
			verify   bool
			admitted bool
		}{
			// A client certificate presented to a server that never asks
			// for one: admitted there alongside a client presenting none.
			{"tls { } without verify, presented a client certificate", false, false},
			{"tls { verify: true }, presented a client certificate", true, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				tlsConfig, err := server.GenTLSConfig(&server.TLSConfigOpts{
					CertFile: cert, KeyFile: key, CaFile: ca, Verify: tc.verify,
				})
				if err != nil {
					t.Fatalf("configure the server's TLS: %v", err)
				}
				hostPort := startRawServer(t, &server.Options{
					TLS: true, TLSConfig: tlsConfig, TLSVerify: tc.verify, TLSTimeout: 5,
				})
				cfg := Config{URL: "nats://" + hostPort, TLS: TLS{CA: ca, Cert: cert, Key: key}}
				if tc.admitted {
					newQueueWith(t, cfg)
					return
				}
				err = refusedOpening(t, cfg)
				if !errors.Is(err, ErrUnauthenticatedBroker) {
					t.Fatalf("err = %v, want ErrUnauthenticatedBroker", err)
				}
			})
		}
	})
}

// refusedOpening opens a queue on cfg and answers the refusal, failing the
// case where it opens.
func refusedOpening(t *testing.T, cfg Config) error {
	t.Helper()
	q, err := Open(t.Context(), cfg)
	if err == nil {
		_ = q.Stop(context.WithoutCancel(t.Context()))
		t.Fatalf("a queue opened on a server every row here refuses")
	}
	return err
}

// startRawServer starts a JetStream server on loopback at the contract's
// max_payload, configured by opts beyond that, and answers its host and port.
func startRawServer(t *testing.T, opts *server.Options) string {
	t.Helper()
	opts.Host, opts.Port, opts.NoLog, opts.NoSigs = "127.0.0.1", -1, true, true
	opts.JetStream, opts.StoreDir = true, t.TempDir()
	opts.MaxPayload = int32(queue.MaxPayloadBytes)
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("configure a server: %v", err)
	}
	go ns.Start()
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	if !ns.ReadyForConnections(30 * time.Second) {
		t.Fatal("the server never became ready")
	}
	return ns.Addr().(*net.TCPAddr).String()
}

// loopbackTLSFiles writes one self-signed certificate for 127.0.0.1 — its own
// CA, and both the server's certificate and the client's — and answers the
// paths of the CA, the certificate and the key.
func loopbackTLSFiles(t *testing.T) (ca, cert, key string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "crewlet-test"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal the key: %v", err)
	}
	dir := t.TempDir()
	cert = filepath.Join(dir, "cert.pem")
	key = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, cert, key
}
