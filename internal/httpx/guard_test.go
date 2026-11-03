package httpx_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// AN ADDRESS IS PUBLIC ONLY WHEN THE PUBLIC INTERNET CAN ROUTE TO IT: never a
// loopback, private, link-local, carrier-grade NAT, documentation or other
// reserved address, never the metadata address a cloud serves its credentials
// on, and never one of those spelled another way (an IPv4 address in IPv6
// form, behind the NAT64 prefix, inside 6to4, or with a zone).
func TestOnlyAnAddressOnThePublicInternetIsPublic(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"127.0.0.1", "127.8.9.10", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", // the metadata address of AWS, GCP and Azure
		"100.100.100.200", // Alibaba Cloud's, inside carrier-grade NAT
		"100.64.0.1",      // carrier-grade NAT
		"0.0.0.0", "0.1.2.3", "192.0.0.8", "192.0.2.1", "198.18.0.1", "198.51.100.7", "203.0.113.9",
		"224.0.0.1", "239.1.2.3", "240.0.0.1", "255.255.255.255",
		"::", "::1", "fd00::1", "fd00:ec2::254", "fe80::1", "fe80::1%en0", "fec0::1", "ff02::1", "100::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", // an IPv4 address in IPv6 form
		"::7f00:1",             // the deprecated IPv4-compatible form of 127.0.0.1
		"64:ff9b::a9fe:a9fe",   // 169.254.169.254 behind the NAT64 prefix
		"64:ff9b::a00:5",       // 10.0.0.5 behind it
		"64:ff9b:1::a9fe:a9fe", // the local-use translation prefix
		"2002:a9fe:a9fe::1",    // 169.254.169.254 inside 6to4
		"2001::1", "2001:2::1", // Teredo, benchmarking
		"2001:db8::1", "3fff::1", // documentation
		"5f00::1",          // segment routing
		"2606:4700::1%en0", // a zone on a global address
	} {
		if httpx.Public(netip.MustParseAddr(raw)) {
			t.Errorf("%s is judged public", raw)
		}
	}
	if httpx.Public(netip.Addr{}) {
		t.Error("the zero address is judged public")
	}
	for _, raw := range []string{
		"104.16.0.1", "151.101.1.1", "93.184.215.14", "8.8.8.8",
		"2606:4700::1", "2a04:4e42::1", "2001:4860:4860::8888",
		"::ffff:104.16.0.1", // a public IPv4 address in IPv6 form
		"64:ff9b::6810:1",   // 104.16.0.1 behind the NAT64 prefix, how an IPv6-only host reaches it
	} {
		if !httpx.Public(netip.MustParseAddr(raw)) {
			t.Errorf("%s is judged not public", raw)
		}
	}
}

// A GUARDED CLIENT REACHES NO ADDRESS THAT IS NOT PUBLIC, whether the URL
// writes the address or a name resolves to it, and the server behind it never
// hears the request. The refusal is [httpx.ErrNotPublic], told apart from an
// address that did not answer.
func TestAGuardedClientReachesNoAddressThatIsNotPublic(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	port := mustURL(t, srv.URL).Port()

	client := httpx.Guarded(5 * time.Second)
	for _, target := range []string{
		srv.URL,                    // the literal loopback address
		"http://localhost:" + port, // a name that resolves to loopback
		"http://[::1]:" + port,     // loopback in IPv6
		"http://169.254.169.254/latest/meta-data/", // refused before it connects, so no network is needed
		"http://10.255.255.1/",
		"http://[64:ff9b::a9fe:a9fe]/",
	} {
		resp, err := client.Get(target)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("GET %s answered %s, want a refusal", target, resp.Status)
			continue
		}
		if !errors.Is(err, httpx.ErrNotPublic) {
			t.Errorf("GET %s failed with %v, want ErrNotPublic", target, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the loopback server answered %d guarded request(s)", n)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
