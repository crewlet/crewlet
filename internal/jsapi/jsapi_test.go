package jsapi_test

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/jsapi"
)

// AN API NOBODY CHOSE IS REFUSED, not read as either one.
//
// Either reading is a node that boots and fails on first use: the account's
// own API times out on every leaf, the domain on every external cluster.
func TestAnUnnamedAPIIsRefused(t *testing.T) {
	t.Parallel()
	var zero jsapi.API
	if zero.Named() {
		t.Fatal("the zero API reports itself named")
	}
	if _, err := zero.Client(&nats.Conn{}); !errors.Is(err, jsapi.ErrUnnamed) {
		t.Errorf("Client on the zero API = %v, want ErrUnnamed", err)
	}
	if _, err := zero.Subject("$JS.API.INFO"); !errors.Is(err, jsapi.ErrUnnamed) {
		t.Errorf("Subject on the zero API = %v, want ErrUnnamed", err)
	}
}

// A SUBJECT REACHED PAST THE CLIENT LANDS ON THE API THE CLIENT SPEAKS.
//
// The backup asks for a stream snapshot with a raw request, because the
// client library has no call for it — and a raw `$JS.API.` subject is exactly
// what a leaf's broker never answers.
func TestASubjectReachedPastTheClientLandsOnTheSameAPI(t *testing.T) {
	t.Parallel()
	const snapshot = "$JS.API.STREAM.SNAPSHOT.CREWLET_AGENT"
	cases := []struct {
		api  jsapi.API
		want string
	}{
		{jsapi.Account(), snapshot},
		{jsapi.Embedded(), "$JS.crewlet.API.STREAM.SNAPSHOT.CREWLET_AGENT"},
	}
	for _, tc := range cases {
		got, err := tc.api.Subject(snapshot)
		if err != nil || got != tc.want {
			t.Errorf("%s: Subject = (%q, %v), want %q", tc.api, got, err, tc.want)
		}
	}
	if _, err := jsapi.Embedded().Subject("crewlet.agent.x"); err == nil {
		t.Error("a subject outside the JetStream API was rewritten rather than refused")
	}
}
