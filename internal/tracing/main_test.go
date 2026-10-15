package tracing

import (
	"fmt"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
)

// TestMain holds the package to ENDING ON THE PROVIDERS IT BEGAN WITH — the
// working ones this package's init installs.
//
// Every test here shares one process and its global providers, and a test
// that installs one through [Configure] and does not put the old one back
// leaves a TERMINATED provider behind it once its flush runs: every later
// test that opens a span then opens none, and passes having tested nothing.
// [install] is the one way a test here installs a provider, and its cleanup
// restores the globals; this is what catches a test that went around it,
// whatever order the tests run in, since anything left installed is still
// installed when the last test returns.
func TestMain(m *testing.M) {
	tracers, meters, rule := otel.GetTracerProvider(), otel.GetMeterProvider(), root.Load()
	code := m.Run()
	if code == 0 && (otel.GetTracerProvider() != tracers ||
		otel.GetMeterProvider() != meters || root.Load() != rule) {
		fmt.Fprintln(os.Stderr, "FAIL: a test left a provider or root rule of its own installed "+
			"as the process global; install one through install(), whose cleanup puts "+
			"the package's own back")
		code = 1
	}
	os.Exit(code)
}
