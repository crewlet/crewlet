package jsapi_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

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

// A CLIENT IS READ BACK AS THE API IT SPEAKS, and one built any way but this
// package is refused by name.
//
// A caller handed a client — coordination's leader reads and purges, the
// object store's metadata reads — reaches past it with raw requests, which
// have to be addressed where the client's own go or nothing answers them; a
// client speaking neither API is a wiring mistake named before the broker is
// asked anything.
func TestAClientIsReadBackAsTheAPIItSpeaks(t *testing.T) {
	t.Parallel()
	nc := &nats.Conn{}
	must := func(js jetstream.JetStream, err error) jetstream.JetStream {
		t.Helper()
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		return js
	}
	for _, c := range []struct {
		name   string
		client jetstream.JetStream
		want   jsapi.API
		refuse string
	}{
		{"the_account", must(jsapi.Account().Client(nc)), jsapi.Account(), ""},
		{"the_embedded_fleet", must(jsapi.Embedded().Client(nc)), jsapi.Embedded(), ""},
		{"a_custom_prefix", must(jetstream.NewWithAPIPrefix(nc, "$ELSEWHERE.API")), jsapi.API{}, "$ELSEWHERE.API"},
		{"another_domain", must(jetstream.NewWithDomain(nc, "elsewhere")), jsapi.API{}, "elsewhere"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := jsapi.Of(c.client)
			if c.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), c.refuse) || !strings.Contains(err.Error(), "internal/jsapi") {
					t.Fatalf("Of = (%v, %v), want a refusal naming %q and internal/jsapi", got, err, c.refuse)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("Of = (%v, %v), want %v", got, err, c.want)
			}
		})
	}
}
