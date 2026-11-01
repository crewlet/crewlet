package mirror_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/code/codetest"
)

// TestMain runs the code host's hook relay when a git node's receive-pack
// asks for it, as every package standing up a codetest fleet does.
func TestMain(m *testing.M) {
	if status, ok := codetest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	os.Exit(m.Run())
}
