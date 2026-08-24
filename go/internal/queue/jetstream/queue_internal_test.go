package jetstream

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// A CLUSTER STILL FORMING is the one error worth waiting out, and telling it
// from the rest is what keeps a config mistake from becoming a
// thirty-second hang with the same message at the end.
func TestOnlyAPlacementFailureIsWaitedOut(t *testing.T) {
	t.Parallel()
	placement := &jetstream.APIError{
		ErrorCode: jsErrCodeNoPeers, Code: 400,
		Description: "no suitable peers for placement",
	}
	if !unplaceable(placement) {
		t.Fatal("a placement failure is not recognised")
	}
	// WRAPPED, because that is how it arrives: the caller adds the stream
	// name before anything sees it.
	if !unplaceable(errors.Join(errors.New("ensure stream CREWLET_AGENT"), placement)) {
		t.Fatal("a wrapped placement failure is not recognised")
	}

	for _, err := range []error{
		nil,
		errors.New("connection refused"),
		&jetstream.APIError{ErrorCode: jetstream.JSErrCodeBadRequest, Code: 400,
			Description: "subject overlaps with an existing stream"},
		&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamNotFound, Code: 404},
	} {
		if unplaceable(err) {
			t.Fatalf("%v was treated as a forming cluster", err)
		}
	}
}
