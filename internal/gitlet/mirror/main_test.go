package mirror_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
)

// TestMain runs Gitlet's hook relay when a Gitlet node's receive-pack
// asks for it, as every package standing up a gitlettest fleet does.
func TestMain(m *testing.M) {
	if status, ok := gitlettest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	os.Exit(m.Run())
}
