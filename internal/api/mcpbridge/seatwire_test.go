package mcpbridge_test

import (
	"encoding/json"
	"testing"
)

// A SEAT'S SESSION IS BYTE-IDENTICAL ON THE WIRE whatever else the bridge
// serves: its handshake, its advertised tools with their hints, a call's
// answer, a failure's answer and who each call is decided as are pinned here
// as the box reads them. The bridge serving a second kind of caller (the
// assistant's deep work, ADR-0054) must leave every byte of this alone.
func TestASeatSessionIsByteIdenticalOnTheWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "read_page", "post_message", "whoami")
	f.byName["post_message"].fail = true
	sess := dial(t, f.open(t))

	wire := map[string]any{"server": sess.InitializeResult().ServerInfo}
	list, err := sess.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wire["tools"] = list.Tools
	wire["call"] = callTool(t, sess, "read_page", map[string]any{"id": 7, "text": "x"})
	wire["whoami"] = callTool(t, sess, "whoami", nil)
	wire["failed"] = callTool(t, sess, "post_message", map[string]any{"text": "hi"})
	got, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != seatWire {
		t.Fatalf("a seat's session changed on the wire:\n got %s\nwant %s", got, seatWire)
	}
}

// seatWire is the seat session's wire as it stood before the bridge served
// any other caller.
const seatWire = `{"call":{"content":[{"type":"text","text":"read_page ok"}]},"failed":{"content":[{"type":"text","text":"post_message ok"}],"isError":true},"server":{"name":"crewlet","title":"Engineer (dev)","version":"1"},"tools":[{"annotations":{"destructiveHint":false,"idempotentHint":false,"openWorldHint":true,"readOnlyHint":false},"description":"post_message does a thing","inputSchema":{"properties":{"id":{"type":"integer"},"text":{"type":"string"}},"type":"object"},"name":"post_message"},{"annotations":{"idempotentHint":true,"readOnlyHint":true},"description":"read_page does a thing","inputSchema":{"properties":{"id":{"type":"integer"},"text":{"type":"string"}},"type":"object"},"name":"read_page"},{"description":"says who this call acts as","inputSchema":{"properties":{},"type":"object"},"name":"whoami"}],"whoami":{"content":[{"type":"text","text":"resolved seat seat=dev login="}]}}`
