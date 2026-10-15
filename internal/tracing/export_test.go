package tracing

import "github.com/crewlet/crewlet/internal/otlpenv"

// DestinationFor is the destination [Configure] builds the exporter for signal
// s from, given env — exported for the agreement test, which lives in the
// external test package so that it can import the sandbox receiver without
// this package ever having to.
func DestinationFor(env func(string) string, s otlpenv.Signal) (otlpenv.Destination, error) {
	return Options{Env: env}.destination(s)
}
