package engine

import (
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/httprelay"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY CHANNEL A NODE CARRIES OUT A REQUEST ON SIGNS IN DOMAINS OF ITS OWN —
// the estate service, the fleet broker's membership, a note to a running turn,
// a read of a box asked of its host, a box's tool call relayed to the process
// holding its session, and the Dashboard Assistant's replays and stops — and
// none of them in a state log's. So does a person's answer to a parked run,
// sealed onto the run's row and handed to the resumed run as their words, and
// a note to a turn whose coding run is out, sealed onto the run's row with the
// seat anchor a person's authority to send one is decided on. And so do the
// sealed channels that are no instruction: presence, which a node displays as
// a person in a room; the live relay, whose answers a node pushes to its
// sockets as a peer's appliers' work; and the assistant's frames, which a node
// streams to a person as their own turn. A domain shared with any of the
// others would let a captured frame of one verify as the other. The domain is
// in the derived key and in the MAC, so two channels sharing one would each
// verify the other's frames: a captured estate reply sent to a member as a
// broker request, or a state-log record replayed as a note. The names live in
// the packages that build them and nothing else compares them; this does,
// here, where all of them are built.
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
	desk, err := newSteerDesk("node-channels", ring)
	if err != nil {
		t.Fatal(err)
	}
	boxReads, err := newBoxReads(ring, "node-a", nil, func() *sandbox.Coordinator { return nil })
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantWires(ring)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := httprelay.New(httprelay.Options{
		Fabric: queuemem.NewBroker().Client(), Name: mcpbridge.RelayName, Node: "node-a",
		Owner: "node-a:1", Keyring: ring, Handler: http.NotFoundHandler(),
		Admit: func(*http.Request) httprelay.Verdict { return httprelay.Refuse },
	})
	if err != nil {
		t.Fatal(err)
	}
	presenceWire, err := newPresenceWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	liveWire, err := newRelayWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	answers, err := sandbox.NewAnswerSeal(ring)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := newSteerSeals(ring)
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
		"the estate service":           pair(envelopes.Domains()),
		"the fleet broker":             pair(broker.Domains()),
		"a note to a running turn":     pair(desk.wire.Domains()),
		"a box read asked of its host": pair(boxReads.wire.Domains()),
		"the assistant's replay":       pair(assistant.replays.Domains()),
		"the assistant's stop":         pair(assistant.stops.Domains()),
		"the live relay":               pair(liveWire.Domains()),
	} {
		claim(domains[0], name+"'s requests")
		claim(domains[1], name+"'s replies")
	}
	for _, domain := range relay.Domains() {
		claim(domain, "the tool bridge's relay")
	}
	claim(assistant.frames.Domain(), "an assistant turn's frame")
	claim(presenceWire.Domain(), "a presence frame")
	claim(answers.Domain(), "an answer to a parked run")
	note, seat := notes.Domains()
	claim(note, "a note on a run record")
	claim(seat, "a run record's seat anchor")
	if len(owner) != len(registeredDomains())+23 {
		t.Fatalf("%d domains claimed: %v", len(owner), owner)
	}
}

func pair(request, reply string) [2]string { return [2]string{request, reply} }
