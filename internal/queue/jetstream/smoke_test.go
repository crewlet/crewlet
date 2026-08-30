package jetstream

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// probe is a minimal registered payload so the smoke tests exercise the real
// typed-event path rather than a bare envelope.
type probe struct {
	N int `json:"n"`
}

func (probe) EventType() string { return "test.probe" }

func init() { events.Register[probe]() }

func newQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := Open(t.Context(), Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return q
}

func ev(n int) *events.Event {
	e := events.New(probe{N: n}, events.TraceContext{})
	// Distinct, ordered timestamps: within-partition ordering is by event
	// timestamp, so tests must not rely on two events sharing one.
	e.Timestamp = time.Now().UTC().Add(time.Duration(n) * time.Millisecond)
	return e
}

// TestMailIsRetainedWithNothingAttached is the property a seat's mailbox is
// built on: the subscription exists without a consumer, retains what is
// published while nothing is attached, and replays it in order on attach.
func TestMailIsRetainedWithNothingAttached(t *testing.T) {
	q := newQueue(t)
	ctx := t.Context()
	topic, group := topics.AgentInbox("alice"), topics.AgentInboxGroup("alice")

	created, err := q.EnsureSubscription(ctx, topic, group)
	if err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	if !created {
		t.Error("EnsureSubscription reported the subscription already existed")
	}
	// Creating an existing subscription is success, not an error, and must
	// report that it did not create it.
	if created, err = q.EnsureSubscription(ctx, topic, group); err != nil || created {
		t.Errorf("second EnsureSubscription = (%v, %v), want (false, nil)", created, err)
	}

	for i := range 3 {
		if err := q.Publish(ctx, topic, ev(i)); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	got := make(chan int, 3)
	if err := q.Subscribe(ctx, topic, group, func(_ context.Context, e *events.Event) queue.Result {
		p, _ := events.DataAs[*probe](e)
		got <- p.N
		return queue.Ack()
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for want := range 3 {
		select {
		case n := <-got:
			if n != want {
				t.Errorf("received %d, want %d — mail replayed out of order", n, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for event %d", want)
		}
	}
}

// TestPublishWithNoSubscriptionIsDropped pins the contract's stated
// behaviour rather than treating it as a surprise: interest retention means
// a message published where no subscription covers it is gone, which is
// exactly why EnsureSubscription must run before anything publishes.
func TestPublishWithNoSubscriptionIsDropped(t *testing.T) {
	q := newQueue(t)
	ctx := t.Context()
	topic, group := topics.AgentInbox("ghost"), topics.AgentInboxGroup("ghost")

	if err := q.Publish(ctx, topic, ev(1)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}

	got := make(chan int, 1)
	if err := q.Subscribe(ctx, topic, group, func(_ context.Context, e *events.Event) queue.Result {
		p, _ := events.DataAs[*probe](e)
		got <- p.N
		return queue.Ack()
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case n := <-got:
		t.Errorf("received %d; a publish with no subscription must be dropped", n)
	case <-time.After(2 * time.Second):
	}
}

// TestDeferReturnsWorkAndQuiesces is the seat-handoff path: a node that has
// lost the right to do the work must neither claim it (ack) nor condemn it
// (an ordinary failure), and must stop taking more.
func TestDeferReturnsWorkAndQuiesces(t *testing.T) {
	q := newQueue(t)
	ctx := t.Context()
	topic, group := topics.AgentInbox("bob"), topics.AgentInboxGroup("bob")
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	if err := q.Publish(ctx, topic, ev(1)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	deferred := make(chan struct{}, 4)
	var once sync.Once
	if err := q.Subscribe(ctx, topic, group, func(context.Context, *events.Event) queue.Result {
		once.Do(func() { close(deferred) })
		return queue.Defer("seat lease moved")
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case <-deferred:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never ran")
	}

	// The attachment quiesces itself, so a second call must not arrive.
	atts := q.lookup(topic, group)
	if len(atts) != 1 {
		t.Fatalf("expected exactly one attachment, got %d", len(atts))
	}
	a := atts[0]
	deadline := time.Now().Add(5 * time.Second)
	for !a.quiesced.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !a.quiesced.Load() {
		t.Error("a deferred delivery did not quiesce its attachment")
	}

	// Detach is non-destructive: a successor attaching to the same durable
	// subscription must find the deferred work waiting.
	if detached, err := q.Detach(ctx, topic, group); err != nil || !detached {
		t.Fatalf("Detach = (%v, %v), want (true, nil)", detached, err)
	}
	got := make(chan int, 1)
	if err := q.Subscribe(ctx, topic, group, func(_ context.Context, e *events.Event) queue.Result {
		p, _ := events.DataAs[*probe](e)
		got <- p.N
		return queue.Ack()
	}); err != nil {
		t.Fatalf("re-Subscribe: %v", err)
	}
	select {
	case n := <-got:
		if n != 1 {
			t.Errorf("successor received %d, want 1", n)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("deferred work never reached the successor")
	}
}

// TestBatchCoalescesByConversation is why ten comments on one issue cost one
// agent turn instead of ten.
func TestBatchCoalescesByConversation(t *testing.T) {
	q := newQueue(t)
	ctx := t.Context()
	topic, group := topics.AgentInbox("carol"), topics.AgentInboxGroup("carol")
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	for i := range 5 {
		if err := q.Publish(ctx, topic, ev(i)); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	batches := make(chan []int, 8)
	opts := queue.NewBatchOptions(0.25, 20)
	if err := q.SubscribeBatch(ctx, topic, group,
		func(_ context.Context, evs []*events.Event) queue.Result {
			ns := make([]int, len(evs))
			for i, e := range evs {
				p, _ := events.DataAs[*probe](e)
				ns[i] = p.N
			}
			batches <- ns
			return queue.Ack()
		},
		func(*events.Event) string { return "one-conversation" },
		opts,
	); err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}

	var seen []int
	deadline := time.After(20 * time.Second)
	for len(seen) < 5 {
		select {
		case b := <-batches:
			seen = append(seen, b...)
		case <-deadline:
			t.Fatalf("only saw %v", seen)
		}
	}
	// Within a conversation, order is by event timestamp — the property
	// that makes a conversation read correctly on any broker.
	for i, n := range seen {
		if n != i {
			t.Errorf("batch order %v is not timestamp-ascending", seen)
			break
		}
	}
}

// TestMalformedSubjectsAreRefused guards the failure that never raises on
// its own: a publish to a subject nobody can consume.
//
// Note what is NOT refused: a well-formed subject in a namespace the engine
// does not itself define. The subject space belongs to the engine and its
// extensions, not to this backend, so an unfamiliar namespace is provisioned
// rather than rejected. Only subjects that cannot be consumed at all are
// errors — and an EMPTY SEGMENT is the important one, because that is what
// an unroutable handle produces (crewlet.agent..inbox), a real subject
// nobody subscribes to that would swallow events in silence.
func TestMalformedSubjectsAreRefused(t *testing.T) {
	q := newQueue(t)
	for _, subject := range []string{"", "crewlet.agent..inbox", ".leading", "trailing.", "has space"} {
		if err := q.Publish(t.Context(), subject, ev(1)); err == nil {
			t.Errorf("Publish(%q) succeeded; a malformed subject must be refused", subject)
		}
	}
	// A foreign but well-formed namespace is provisioned, not refused.
	if _, err := q.EnsureSubscription(t.Context(), "extension.thing", "grp"); err != nil {
		t.Fatalf("EnsureSubscription on a foreign namespace: %v", err)
	}
	if err := q.Publish(t.Context(), "extension.thing", ev(1)); err != nil {
		t.Errorf("Publish to a foreign namespace failed: %v", err)
	}
}

func TestAClusteredServerMustBeNamed(t *testing.T) {
	t.Parallel()
	// Refused rather than defaulted, because both halves of the
	// requirement matter and only one is obvious. Unique: NATS rejects a
	// route from a server whose name it already knows, so a fleet of
	// identically-named members never forms — which is what every embedded
	// server in this package used to be. Stable: JetStream places replicas
	// by server name, so a member returning under a new name is a new
	// peer, its old replicas are stranded on a server that no longer
	// exists, and the stream sits below quorum waiting for it.
	//
	// A generated name satisfies the first and quietly breaks the second on
	// every restart, which is why this is an error and not a default.
	_, err := StartServer(t.Context(), Config{ClusterName: "unnamed", ClusterPort: 0})
	if err == nil {
		t.Fatal("a clustered server started with no ServerName")
	}
	if !strings.Contains(err.Error(), "ServerName") {
		t.Errorf("error %q does not say which field is missing", err)
	}

	// Solo is unaffected: there is no cluster to be unique within.
	srv, err := StartServer(t.Context(), Config{})
	if err != nil {
		t.Fatalf("a solo server needs no name: %v", err)
	}
	srv.Shutdown()
}

// A TLS PATH THAT IS NOT THERE IS NAMED AS A CONFIG PROBLEM.
//
// nats.Connect's failure for an unreadable certificate arrives as a dial
// error, which reads as "the broker is unreachable" — so an operator goes
// looking at the network for a path that is simply not there.
func TestAnUnreadableTLSPathIsNamedBeforeDialling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tls  TLS
		want []string
	}{
		{"missing ca", TLS{CA: "/nope/ca.pem"}, []string{"tls.ca", "/nope/ca.pem"}},
		{
			"missing client key",
			TLS{Cert: "/nope/c.pem", Key: "/nope/k.pem"},
			[]string{"/nope/"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A URL nothing listens on: the point is that the TLS
			// material is refused BEFORE anything is dialled, so the
			// error must not be a connection failure.
			_, err := Dial(Config{URL: "nats://127.0.0.1:1", TLS: tc.tls})
			if err == nil {
				t.Fatal("an unreadable TLS path dialled anyway")
			}
			if strings.Contains(err.Error(), "dial nats://") {
				t.Errorf("reported as a connection failure: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to name %s", err, want)
				}
			}
		})
	}
}

// THE MATERIAL ACTUALLY REACHES THE CONNECTION.
//
// Asserted on the option list rather than on a live handshake: a real proof
// needs a TLS broker with `verify: true`, and would then be testing that NATS
// honours its own documented option rather than that this package passes it.
// What CAN silently go wrong here is a field that is read, validated, and
// then never appended — a config an operator sets, a `crewlet validate` that
// accepts it, and a broker that rejects every connection for a reason no log
// line connects to the omission.
func TestTheConfiguredTLSMaterialReachesTheDialOptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPEM, keyPEM := selfSignedPEM(t)
	write := func(name string, body []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ca := write("ca.pem", certPEM)
	cert := write("c.pem", certPEM)
	key := write("k.pem", keyPEM)

	opts, err := dialOptions(Config{TLS: TLS{CA: ca, Cert: cert, Key: key}})
	if err != nil {
		t.Fatalf("dialOptions: %v", err)
	}
	// Applied the way nats.Connect applies them, then inspected.
	applied := nats.GetDefaultOptions()
	for _, opt := range opts {
		if oerr := opt(&applied); oerr != nil {
			t.Fatalf("applying an option: %v", oerr)
		}
	}
	// Both options install a CALLBACK and flip Secure, rather than
	// writing into TLSConfig directly — so the assertion invokes them.
	if applied.RootCAsCB == nil {
		t.Fatal("the configured tls.ca never became a dial option, so a " +
			"private CA is read from the config and never trusted")
	}
	pool, err := applied.RootCAsCB()
	if err != nil || pool == nil {
		t.Fatalf("the root CA callback answered %v (err %v)", pool, err)
	}
	if applied.TLSCertCB == nil {
		t.Fatal("the configured tls.cert/tls.key never became a dial " +
			"option, so a broker requiring mutual TLS refuses every connection")
	}
	if _, err = applied.TLSCertCB(); err != nil {
		t.Fatalf("the client certificate callback failed: %v", err)
	}
	if !applied.Secure {
		t.Error("TLS material was configured and the connection is not " +
			"marked secure, so it would be attempted in the clear")
	}
}

// selfSignedPEM mints one throwaway certificate and its key.
//
// Real material rather than a stub, because both options PARSE what they are
// given — a placeholder makes them fail, and a test that asserted on those
// failures would pass just as happily if the options were never applied.
func selfSignedPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "crewlet-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return certPEM, keyPEM
}
