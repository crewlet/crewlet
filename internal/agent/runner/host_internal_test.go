package runner

import (
	"testing"

	"github.com/crewlet/crewlet/internal/toolsession"
)

// testHost is a session host for one case of this package's own tests, which
// cannot reach the external suite's (main_test.go): a runner refuses to be
// built without one.
func testHost(t *testing.T) *toolsession.Host {
	t.Helper()
	h, err := toolsession.NewHost()
	if err != nil {
		t.Fatalf("toolsession.NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}
