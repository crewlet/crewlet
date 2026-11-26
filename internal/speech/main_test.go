package speech

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// The engine starts its worker as this binary; under `go test` that is
	// the test binary, which must then be the worker rather than the suite.
	DispatchIfWorker()
	dispatchScriptedWorker()
	os.Exit(m.Run())
}
