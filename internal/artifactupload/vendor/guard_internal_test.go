package vendor

import (
	"errors"
	"net"
	"net/url"
	"testing"
)

// THE GUARD DIALS NO ADDRESS THAT IS NOT PUBLIC, by name or by literal, and
// dials a proxy the operator configured as itself.
func TestTheGuardRefusesAnAddressThatIsNotPublic(t *testing.T) {
	t.Parallel()
	g := &guard{dialer: &net.Dialer{}, proxies: map[string]bool{}}
	for _, addr := range []string{"127.0.0.1:443", "[::1]:443", "localhost:443", "169.254.169.254:80"} {
		if _, err := g.dial(t.Context(), "tcp", addr); !errors.Is(err, errPrivateAddress) {
			t.Errorf("a dial of %s answered %v, want errPrivateAddress", addr, err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	g.trust(&url.URL{Scheme: "http", Host: ln.Addr().String()})
	conn, err := g.dial(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("the operator's proxy was refused: %v", err)
	}
	_ = conn.Close()
}
