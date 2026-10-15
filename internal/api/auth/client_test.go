package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
)

// behind builds a guard for a deployment behind the named proxy blocks.
func behind(t *testing.T, blocks ...string) *auth.Guard {
	t.Helper()
	b := config.DefaultBootstrap()
	b.API.TrustedProxies = blocks
	return auth.New(&b)
}

// asking builds a request from a peer, carrying whatever forwarded headers a
// case names.
func asking(peer string, forwarded ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	r.RemoteAddr = peer
	for _, value := range forwarded {
		r.Header.Add("X-Forwarded-For", value)
	}
	return r
}

// A DEPLOYMENT THAT TRUSTS NOBODY BELIEVES NO FORWARDED HEADER.
//
// This is the default, and it has to be: a header anybody can send would
// otherwise let an attacker pick their own rate-limit bucket and their own row
// in the audit trail — so the throttle never fires and the log names whoever
// they decided to blame.
func TestAnUntrustedPeersForwardedHeaderIsIgnored(t *testing.T) {
	t.Parallel()
	g := behind(t)
	got := g.Client(asking("203.0.113.9:44321", "10.9.9.9"))
	if got != "203.0.113.9" {
		t.Errorf("client = %q, want the peer: a deployment trusting no proxy "+
			"believed a header anybody can send", got)
	}
	// AND NEITHER DOES A DEPLOYMENT THAT TRUSTS A DIFFERENT PEER. The
	// question is whether THIS peer is the proxy, which is what a CIDR
	// list answers and a bool cannot.
	g = behind(t, "192.0.2.0/24")
	got = g.Client(asking("203.0.113.9:44321", "10.9.9.9"))
	if got != "203.0.113.9" {
		t.Errorf("client = %q, want the peer: a header from a peer this "+
			"deployment does not trust was believed", got)
	}
}

// AND A TRUSTED PROXY'S HEADER IS BELIEVED, which is the control — without it
// the case above would pass on a resolver that ignored every header, and a
// deployment behind a real proxy would bucket the entire internet under one
// address and lock every honest person out the moment one attacker arrived.
func TestATrustedProxysForwardedHeaderIsBelieved(t *testing.T) {
	t.Parallel()
	g := behind(t, "192.0.2.0/24")
	got := g.Client(asking("192.0.2.7:44321", "203.0.113.9"))
	if got != "203.0.113.9" {
		t.Errorf("client = %q, want the forwarded client", got)
	}
}

// THE WALK GOES RIGHT TO LEFT, AND STOPS AT THE FIRST HOP THIS DEPLOYMENT
// CANNOT VOUCH FOR.
//
// This is the whole of the rule and the classic place it is got wrong. The
// header is APPENDED to by each hop, so it reads client, proxy1, proxy2 with
// the nearest proxy last — and everything to the left of the first untrusted
// entry is whatever that entry's author chose to claim. Taking the LEFTMOST
// entry reads naturally as "the original client" and is the one value entirely
// under the caller's control.
func TestTheWalkStopsAtTheFirstHopItCannotVouchFor(t *testing.T) {
	t.Parallel()
	g := behind(t, "192.0.2.0/24", "198.51.100.0/24")
	// A caller claiming to be somebody else, through two of this
	// deployment's own proxies. The claim is the leftmost entry.
	got := g.Client(asking("192.0.2.7:1",
		"10.0.0.1, 203.0.113.9, 198.51.100.4"))
	if got != "203.0.113.9" {
		t.Errorf("client = %q, want 203.0.113.9: the walk took a hop the "+
			"caller wrote rather than the last one this deployment can "+
			"vouch for", got)
	}
}

// SEVERAL HEADER LINES ARE ONE CHAIN. Each proxy is entitled to APPEND a line
// rather than extend the last, so a resolver reading one of them drops hops
// the walk above depends on seeing — and lands on a value the caller wrote.
func TestSeveralForwardedLinesAreOneChain(t *testing.T) {
	t.Parallel()
	g := behind(t, "192.0.2.0/24", "198.51.100.0/24")
	// THE DECIDING HOP IS IN THE SECOND LINE, which is what makes this
	// case distinguish anything: with both lines the walk finds
	// 203.0.113.9, and reading only the first it never gets past
	// 10.0.0.1 — the value the caller wrote, which is the whole failure.
	got := g.Client(asking("192.0.2.7:1",
		"10.0.0.1", "203.0.113.9, 198.51.100.4"))
	if got != "203.0.113.9" {
		t.Errorf("client = %q, want 203.0.113.9: a second header line was "+
			"dropped, so the walk stopped inside what the caller wrote", got)
	}
}

// A CHAIN OF NOTHING BUT THIS DEPLOYMENT'S OWN PROXIES has no client address
// in it, and the leftmost is then the closest thing there is — written by a
// peer this deployment trusts, which is the whole reason it may be used.
func TestAChainOfOnlyTrustedHopsFallsBackToTheLeftmost(t *testing.T) {
	t.Parallel()
	g := behind(t, "192.0.2.0/24")
	got := g.Client(asking("192.0.2.7:1", "192.0.2.4, 192.0.2.5"))
	if got != "192.0.2.4" {
		t.Errorf("client = %q, want the leftmost trusted hop", got)
	}
}

// AN IPv6 PEER AND AN IPv6 HOP BOTH RESOLVE. A resolver that stripped a port
// by looking for the last colon would turn every IPv6 address into a prefix of
// itself, and two callers from one /64 into two buckets — or one.
func TestAnIPv6AddressSurvivesThePortStrip(t *testing.T) {
	t.Parallel()
	g := behind(t, "2001:db8::/32")
	if got := g.Client(asking("[2001:db8::7]:44321", "2001:db8:1::9")); got != "2001:db8:1::9" {
		t.Errorf("client = %q, want the forwarded IPv6 client", got)
	}
	// And a peer this deployment does not trust keeps its own address
	// whole.
	g = behind(t)
	if got := g.Client(asking("[2001:db8::7]:44321")); got != "2001:db8::7" {
		t.Errorf("client = %q, want the whole IPv6 peer", got)
	}
}

// A BLOCK THAT WILL NOT PARSE IS DROPPED, not fatal — `crewlet validate`
// refuses one by name at the moment somebody writes it, and a panic at bind
// time would take a node down over a value the operator was already told
// about. Dropping is also the safe direction: one fewer trusted proxy means
// one more client keyed to its own peer.
func TestAnUnparsableTrustedBlockIsDroppedRatherThanFatal(t *testing.T) {
	t.Parallel()
	g := behind(t, "not-a-cidr", "192.0.2.0/24")
	if got := g.Client(asking("192.0.2.7:1", "203.0.113.9")); got != "203.0.113.9" {
		t.Errorf("client = %q: one unparsable block took the whole list with "+
			"it", got)
	}
}
