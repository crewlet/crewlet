package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY CHANNEL A NODE CARRIES OUT A REQUEST ON SIGNS IN DOMAINS OF ITS OWN (the
// estate service, the fleet broker's membership, a note to a running turn and a
// code search),
// and none of them in a state log's. And so does the one sealed frame
// that is no instruction, presence, which a node displays as a person in a
// room: a domain shared with any of the others would let a captured frame of
// one verify as the other. The domain is in the derived key
// and in the MAC, so two channels sharing one would each verify the other's
// frames: a captured estate reply sent to a member as a broker request, or a
// state-log record replayed as a note. The names live in three packages and
// nothing else compares them; this does, here, where all of them are built.
func TestEveryInstructionChannelSignsInDomainsOfItsOwn(t *testing.T) {
	t.Parallel()
	ring := statelog.OneKey("k1", "the channel gate's fleet key material")
	envelopes, err := estate.NewEnvelopes(ring)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := newBrokerWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	desk, err := newSteerDesk(ring)
	if err != nil {
		t.Fatal(err)
	}
	searchWire, err := newCodeSearchWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	presenceWire, err := newPresenceWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	owner := map[string]string{}
	claim := func(domain, by string) {
		if prior, taken := owner[domain]; taken {
			t.Errorf("%s signs in %q, which %s signs in too", by, domain, prior)
		}
		owner[domain] = by
	}
	for _, d := range registeredDomains() {
		claim(d.Name(), "the "+d.Name()+" state log")
	}
	for name, domains := range map[string][2]string{
		"the estate service":       pair(envelopes.Domains()),
		"the fleet broker":         pair(broker.Domains()),
		"a note to a running turn": pair(desk.wire.Domains()),
		"a code search":            pair(searchWire.Domains()),
	} {
		claim(domains[0], name+"'s requests")
		claim(domains[1], name+"'s replies")
	}
	claim(presenceWire.Domain(), "a presence frame")
	if len(owner) != len(registeredDomains())+9 {
		t.Fatalf("%d domains claimed: %v", len(owner), owner)
	}
}

func pair(request, reply string) [2]string { return [2]string{request, reply} }
