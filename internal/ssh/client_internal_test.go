package ssh

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// THE CLIENT NEVER DIALS THE ENGINE HOST OR A LINK-LOCAL ADDRESS: the address
// the socket would connect to is judged, so a literal and a name that resolves
// to one are refused alike, and the scan holds a typed address to the same
// rule before it dials at all.
func TestTheClientNeverDialsTheEngineHost(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"127.0.0.1:22", "[::1]:22", "localhost:22", "169.254.169.254:22", "0.0.0.0:22"} {
		conn, err := guardedDial(t.Context(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
		}
		if !errors.Is(err, ErrUnreachableAddress) {
			t.Errorf("%s: %v, want refused as unreachable", addr, err)
		}
	}
	if _, err := Scan(t.Context(), "127.0.0.1", 22); !errors.Is(err, ErrUnreachableAddress) {
		t.Errorf("a scan of the engine host: %v", err)
	}
	h := config.SSHHost{ID: "api-1", Address: "localhost", HostKeys: []string{"x"}}
	if _, err := NewClient(nil).connect(t.Context(), h, "crewlet-read", nil); err == nil {
		t.Error("a host whose keys do not parse was connected to")
	}
}
