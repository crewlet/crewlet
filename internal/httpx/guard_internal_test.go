package httpx

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// AN ANSWER THAT CHANGES BETWEEN LOOKUPS IS JUDGED AT EACH CONNECTION, AND THE
// CONNECTION GOES TO THE ADDRESS THAT WAS JUDGED.
//
// The resolver here answers a name with the listener's address the first time
// and another loopback address every time after, which is what a rebinding
// resolver does to a guard that checks a name and then dials it: the check
// sees the first answer and the dial resolves again and gets the second. The
// guard under test admits the listener's address alone, so the first request
// must reach the listener (a dial of the name would have connected to the
// second answer, where nothing listens) and the second, on a connection of its
// own, must be refused without reaching anything.
func TestTheGuardJudgesTheAddressItConnectsToNotTheName(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "listener")
	}))
	t.Cleanup(srv.Close)
	listener := netip.MustParseAddrPort(srv.Listener.Addr().String())

	var lookups atomic.Int64
	dns := fakeDNS(t, func(q dnsmessage.Question) []netip.Addr {
		if q.Type != dnsmessage.TypeA {
			return nil
		}
		if lookups.Add(1) == 1 {
			return []netip.Addr{listener.Addr()}
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.2")}
	})
	d := &net.Dialer{Timeout: 5 * time.Second, Resolver: &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", dns)
		},
	}}
	g := newGuard(d, func(ip netip.Addr) bool { return ip == listener.Addr() })
	client := guardedClient(t, g, nil)

	target := "http://rebind.test:" + strconv.Itoa(int(listener.Port())) + "/"
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("the first request, whose answer the guard admits, failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "listener" {
		t.Fatalf("the first request reached %q, want the listener the guard admitted", body)
	}

	resp, err = client.Get(target)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the second request, whose answer the guard refuses, answered %s", resp.Status)
	}
	if !errors.Is(err, ErrNotPublic) {
		t.Fatalf("the second request failed with %v, want ErrNotPublic", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the listener answered %d requests, want the first alone", n)
	}
}

// A PROXY THE OPERATOR CONFIGURED IS DIALLED AS ITSELF, AND AN ADDRESS IT IS
// ASKED FOR IS STILL JUDGED.
//
// The proxy stands on loopback, which the guard refuses for anything else, and
// a request for a name goes through it. A request whose URL writes an address
// that is not public is refused before the proxy hears of it, since the proxy
// would connect there on the engine's behalf.
func TestTheOperatorsProxyIsDialledAsItselfAndAnAddressItIsAskedForIsJudged(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var asked []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.String())
		mu.Unlock()
		_, _ = io.WriteString(w, "proxied")
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	g := newGuard(&net.Dialer{Timeout: 5 * time.Second}, Public)
	client := guardedClient(t, g, http.ProxyURL(proxyURL))

	resp, err := client.Get("http://cdn.example.com/a.js")
	if err != nil {
		t.Fatalf("a request through the operator's proxy on loopback was refused: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "proxied" {
		t.Fatalf("the request answered %q, want the proxy's answer", body)
	}

	for _, target := range []string{"http://169.254.169.254/latest/meta-data/", "http://[::1]:8200/", "http://10.0.0.5/"} {
		resp, err := client.Get(target)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("GET %s through the proxy answered %s, want a refusal", target, resp.Status)
			continue
		}
		if !errors.Is(err, ErrNotPublic) {
			t.Errorf("GET %s through the proxy failed with %v, want ErrNotPublic", target, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "http://cdn.example.com/a.js" {
		t.Fatalf("the proxy was asked for %q, want the name alone", asked)
	}
}

// guardedClient is a client on a clone of the shared transport with g
// installed over proxy, opening a connection per request so each one dials.
func guardedClient(t *testing.T, g *guard, proxy func(*http.Request) (*url.URL, error)) *http.Client {
	t.Helper()
	base, ok := Transport().(*http.Transport)
	if !ok {
		t.Fatal("the shared transport is not an *http.Transport")
	}
	tr := base.Clone()
	tr.Proxy = proxy
	tr.DisableKeepAlives = true
	g.install(tr)
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// fakeDNS serves answer over UDP on loopback until the test ends, and answers
// its address.
func fakeDNS(t *testing.T, answer func(dnsmessage.Question) []netip.Addr) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			msg, err := dnsAnswer(h, q, answer(q))
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(msg, from)
		}
	}()
	return pc.LocalAddr().String()
}

// dnsAnswer is the response to q carrying addrs.
func dnsAnswer(h dnsmessage.Header, q dnsmessage.Question, addrs []netip.Addr) ([]byte, error) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: h.ID, Response: true, Authoritative: true,
		RecursionDesired: h.RecursionDesired, RecursionAvailable: true,
	})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET}
	for _, a := range addrs {
		var err error
		switch {
		case a.Is4() && q.Type == dnsmessage.TypeA:
			err = b.AResource(rh, dnsmessage.AResource{A: a.As4()})
		case a.Is6() && q.Type == dnsmessage.TypeAAAA:
			err = b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: a.As16()})
		}
		if err != nil {
			return nil, err
		}
	}
	return b.Finish()
}
