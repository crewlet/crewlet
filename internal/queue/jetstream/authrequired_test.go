package jetstream

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

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
		// And by a name, which the dial resolves itself ([infoDialer]).
		_, port, err := net.SplitHostPort(srv.HostPort())
		if err != nil {
			t.Fatalf("the server's address: %v", err)
		}
		newQueueWith(t, Config{URL: "nats://localhost:" + port, Token: externaltest.Token})
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

// ONE ATTEMPT AT A MEMBER TAKES ONE HANDSHAKE BUDGET, HOWEVER MANY ADDRESSES
// ITS NAME HAS.
//
// The common Kubernetes shape is one stream.url name resolving to every pod
// of the NATS cluster. Without a custom dialer the client shares its budget
// among a name's addresses; handed one, it would hand each address the whole
// budget, and a partitioned cluster of three would hold every attempt for
// 6 s against the lease TTL the coordination store renews over this
// connection. So the client skips its lookup and the dialer resolves the name
// — that lookup bounded by the budget too — and dials each address with an
// equal share of it. A literal IP is one address, dialled with all of it.
//
// Mutations: drop nats.SkipHostLookup and the client goes back to resolving
// the name itself; give each address the whole budget and the attempt takes
// three; leave the lookup unbounded and it waits on DNS for as long as DNS
// takes.
func TestOneAttemptAtAMemberTakesOneHandshakeBudgetHoweverManyAddressesItsNameHas(t *testing.T) {
	t.Parallel()
	opts, recorded, err := dialOptions(Config{})
	if err != nil {
		t.Fatalf("dialOptions: %v", err)
	}
	applied := nats.GetDefaultOptions()
	for _, opt := range opts {
		if err := opt(&applied); err != nil {
			t.Fatalf("applying an option: %v", err)
		}
	}
	if !applied.SkipHostLookup || applied.CustomDialer != recorded {
		t.Fatalf("the client resolves a member's name itself (skip lookup %t, the "+
			"recording dialer installed %t): it would hand the dialer each address "+
			"in turn, each taking the whole budget",
			applied.SkipHostLookup, applied.CustomDialer == recorded)
	}

	// A silent address answers nothing until its dial's own deadline.
	type dialled struct {
		address string
		budget  time.Duration
	}
	silent := func(calls *[]dialled, mu *sync.Mutex) func(context.Context, string, string) (net.Conn, error) {
		return func(ctx context.Context, _, address string) (net.Conn, error) {
			deadline, ok := ctx.Deadline()
			mu.Lock()
			*calls = append(*calls, dialled{address, time.Until(deadline)})
			mu.Unlock()
			if !ok {
				return nil, errors.New("a dial with no deadline")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}

	t.Run("a name resolving to three silent addresses", func(t *testing.T) {
		t.Parallel()
		d := newInfoDialer(recorded.budget)
		var looked time.Duration
		d.lookup = func(ctx context.Context, host string) ([]string, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				return nil, errors.New("a lookup with no deadline")
			}
			looked = time.Until(deadline)
			return []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, nil
		}
		var (
			mu    sync.Mutex
			calls []dialled
		)
		d.connect = silent(&calls, &mu)
		began := time.Now()
		if _, err := d.Dial("tcp", "nats.example.com:4222"); err == nil {
			t.Fatal("a member whose every address is silent was dialled")
		}
		took := time.Since(began)
		if looked <= 0 || looked > externalHandshake {
			t.Errorf("the lookup was given %v, want at most %v", looked, externalHandshake)
		}
		share := externalHandshake / 3
		tried := map[string]bool{}
		for _, c := range calls {
			tried[c.address] = true
			if c.budget > share || c.budget < share/2 {
				t.Errorf("%s was dialled with %v, want an equal share of %v: %v",
					c.address, c.budget, externalHandshake, share)
			}
		}
		if len(calls) != 3 || len(tried) != 3 {
			t.Errorf("dialled %v, want each of the three addresses once", calls)
		}
		// The shares end on wall-clock deadlines, so the attempt ends on
		// the budget's; the second of slack is the runner's scheduling,
		// a third of what giving every address the budget would cost.
		if took > externalHandshake+time.Second {
			t.Errorf("one attempt at the member took %v, want about %v", took, externalHandshake)
		}
	})

	t.Run("a literal IP", func(t *testing.T) {
		t.Parallel()
		d := newInfoDialer(recorded.budget)
		d.lookup = func(context.Context, string) ([]string, error) {
			return nil, errors.New("a literal IP was looked up")
		}
		var (
			mu    sync.Mutex
			calls []dialled
		)
		d.connect = func(ctx context.Context, network, address string) (net.Conn, error) {
			deadline, _ := ctx.Deadline()
			mu.Lock()
			calls = append(calls, dialled{address, time.Until(deadline)})
			mu.Unlock()
			return nil, errors.New("refused")
		}
		if _, err := d.Dial("tcp", "192.0.2.9:4222"); err == nil || err.Error() != "refused" {
			t.Fatalf("Dial = %v, want the dial's own refusal", err)
		}
		if len(calls) != 1 || calls[0].address != "192.0.2.9:4222" ||
			calls[0].budget > externalHandshake || calls[0].budget < externalHandshake/2 {
			t.Errorf("dialled %v, want 192.0.2.9:4222 once with the whole %v", calls, externalHandshake)
		}
	})
}

// THE ADDRESSES BEHIND ONE NAME ARE DIALLED IN RANDOM ORDER.
//
// The client shuffled a name's addresses before it dialled them, which is
// what spreads a fleet's connections across the members behind one service
// name; the dialer that took the lookup over keeps it, where Go's own serial
// dial takes them in DNS order and would put every node on one member.
//
// Mutation: dial the addresses in the order the lookup gave them and the
// first is the one dialled every time. Sixty-four dials see both first with
// a probability of one less 2⁻⁶³.
func TestTheAddressesBehindOneNameAreDialledInRandomOrder(t *testing.T) {
	t.Parallel()
	d := newInfoDialer(externalHandshake)
	d.lookup = func(context.Context, string) ([]string, error) {
		return []string{"192.0.2.1", "192.0.2.2"}, nil
	}
	firsts := map[string]int{}
	d.connect = func(_ context.Context, _, address string) (net.Conn, error) {
		firsts[address]++
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}
	for range 64 {
		c, err := d.Dial("tcp", "nats.example.com:4222")
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		_ = c.Close()
	}
	if len(firsts) != 2 {
		t.Errorf("the first address dialled was always the same: %v", firsts)
	}
}

// THE INFO JUDGED IS THE ONE OF THE SERVER THE CLIENT IS ON.
//
// Not the newest connection the dialer opened: nats.go dials the next member
// at once when a server drops a connection, so a member restarting between
// the connect and the check leaves the newest connection on a member whose
// INFO is not read yet, while the client still names the server it completed
// on — or, its reconnect not done, none. The line judged is the one carrying
// the server_id the client names; a client on no server is refused as having
// lost its connection, not as a server that sent no INFO; and once the dial
// has asked, the dialer records nothing more, so a connection that
// reconnects for its whole life keeps no line of each.
//
// Mutations: answer for the newest connection and the restarted member's
// unread one is judged, refusing a server that authenticates; leave the
// dialer unsealed and a later connection is still recorded; drop the branch
// for a client on no server and it is refused as one that read no INFO.
func TestTheINFOJudgedIsTheOneOfTheServerTheClientIsOn(t *testing.T) {
	t.Parallel()
	// opened is a dialer that has opened three connections: to server A,
	// which requires authentication, to server B, which does not, and a
	// newest that has read nothing yet.
	opened := func(t *testing.T) *infoDialer {
		t.Helper()
		d := newInfoDialer(externalHandshake)
		var servers []net.Conn
		d.connect = func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			servers = append(servers, server)
			return client, nil
		}
		t.Cleanup(func() {
			for _, s := range servers {
				_ = s.Close()
			}
		})
		for _, info := range []string{
			`INFO {"server_id":"A","auth_required":true}`,
			`INFO {"server_id":"B"}`,
			"",
		} {
			c, err := d.Dial("tcp", "192.0.2.1:4222")
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			if info == "" {
				continue
			}
			server := servers[len(servers)-1]
			go func() { _, _ = server.Write([]byte(info + "\r\n")) }()
			if _, err := c.Read(make([]byte, 256)); err != nil {
				t.Fatalf("read the INFO: %v", err)
			}
		}
		return d
	}

	t.Run("a client on an older connection's server", func(t *testing.T) {
		t.Parallel()
		info := opened(t).sealedInfo("A")
		if err := requiresAuthentication("server A", info); err != nil {
			t.Errorf("the INFO of the server the client is on, which requires "+
				"authentication, was not the one judged: %v", err)
		}
	})
	t.Run("each connection's server by its own id", func(t *testing.T) {
		t.Parallel()
		if got, want := string(opened(t).sealedInfo("B")), `INFO {"server_id":"B"}`; got != want {
			t.Errorf("the INFO for server B = %q, want %q", got, want)
		}
	})
	t.Run("a server no connection read an INFO from", func(t *testing.T) {
		t.Parallel()
		for _, id := range []string{"C", ""} {
			if info := opened(t).sealedInfo(id); info != nil {
				t.Errorf("the INFO for server %q = %q, want none", id, info)
			}
		}
	})
	t.Run("a connection opened once the dial has asked", func(t *testing.T) {
		t.Parallel()
		d := opened(t)
		d.sealedInfo("A")
		c, err := d.Dial("tcp", "192.0.2.1:4222")
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = c.Close() }()
		if _, recorded := c.(*infoConn); recorded {
			t.Error("a connection opened after the dial asked is still recorded")
		}
		if len(d.opened) != 0 {
			t.Errorf("the sealed dialer still holds %d connections", len(d.opened))
		}
	})
	t.Run("a live client on no server", func(t *testing.T) {
		t.Parallel()
		srv := externaltest.Start(t, queue.MaxPayloadBytes)
		opts, recorded, err := dialOptions(Config{Token: externaltest.Token})
		if err != nil {
			t.Fatalf("dialOptions: %v", err)
		}
		nc, err := nats.Connect(srv.URL(), opts...)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		nc.Close()
		_, err = heldToAuthentication(nc, recorded, srv.URL())
		if err == nil || errors.Is(err, ErrUnauthenticatedBroker) {
			t.Fatalf("err = %v, want a refusal for a lost connection, not a "+
				"finding about a server that authenticates", err)
		}
		for _, says := range []string{"was lost before", "nats://" + srv.HostPort()} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("the refusal does not say %q: %v", says, err)
			}
		}
	})
}

// AN INFO LINE IS KEPT WHOLE, AND NOTHING AFTER IT OR PAST ITS BOUND.
//
// A large cluster's INFO behind a small first TCP segment arrives in several
// reads, its CRLF among them split; the line is assembled across them, and
// what follows it — a TLS upgrade's ciphertext — is never kept. A line that
// runs past infoLineMax, its CRLF arrived in the same read or not at all, is
// kept as none, so the dial refuses it as unread rather than judging a
// prefix; one at the bound exactly is kept.
//
// Mutations: keep only the latest read and the line split across reads is
// never assembled; keep what follows the CRLF and the tail is held; drop the
// bound and a line past it is kept; keep the bound's prefix and that prefix
// is judged.
func TestAnINFOLineIsKeptWholeAndNothingAfterItOrPastItsBound(t *testing.T) {
	t.Parallel()
	// fed reads every chunk through an infoConn, one write a read, and
	// answers the connection.
	fed := func(t *testing.T, chunks ...[]byte) *infoConn {
		t.Helper()
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
		c := &infoConn{Conn: client}
		buf := make([]byte, 2*infoLineMax)
		for _, chunk := range chunks {
			go func() { _, _ = server.Write(chunk) }()
			if n, err := io.ReadFull(c, buf[:len(chunk)]); err != nil || n != len(chunk) {
				t.Fatalf("read %d of a %d-byte chunk: %v", n, len(chunk), err)
			}
		}
		return c
	}
	held := func(c *infoConn) int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.read)
	}
	const info = `INFO {"server_id":"A","auth_required":true,"max_payload":8388608}`
	ciphertext := []byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01, 0x00, 0x01, 0xfc, 0x03, 0x03}

	t.Run("split across reads, its CRLF among them", func(t *testing.T) {
		t.Parallel()
		c := fed(t, []byte(info[:7]))
		if c.line() != nil {
			t.Fatalf("a line was kept before its CRLF: %q", c.line())
		}
		tail := append([]byte("\n"), ciphertext...)
		c = fed(t, []byte(info[:7]), []byte(info[7:30]), []byte(info[30:]+"\r"), tail, ciphertext)
		if got := string(c.line()); got != info {
			t.Errorf("the line = %q, want %q", got, info)
		}
		if n := held(c); n != 0 {
			t.Errorf("%d bytes after the CRLF are still held", n)
		}
	})
	past := bytes.Repeat([]byte("x"), infoLineMax-len("INFO ")+1)
	at := past[:len(past)-1]
	for _, tc := range []struct {
		name   string
		chunks [][]byte
		kept   bool
	}{
		{"past the bound in one read", [][]byte{append(append([]byte("INFO "), past...), "\r\n"...)}, false},
		{"past the bound across reads", [][]byte{[]byte("INFO "), past[:infoLineMax/2], past[infoLineMax/2:], []byte("\r\n")}, false},
		{"at the bound exactly", [][]byte{append(append([]byte("INFO "), at...), "\r\n"...)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := fed(t, tc.chunks...)
			if got := c.line(); tc.kept != (got != nil) {
				t.Fatalf("kept a %d-byte line: %t, want %t (the bound is %d)",
					len(got), got != nil, tc.kept, infoLineMax)
			}
			if tc.kept && len(c.line()) != infoLineMax {
				t.Errorf("kept %d bytes of a %d-byte line", len(c.line()), infoLineMax)
			}
			if n := held(c); n != 0 {
				t.Errorf("%d bytes are still held once the line was judged", n)
			}
		})
	}
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
