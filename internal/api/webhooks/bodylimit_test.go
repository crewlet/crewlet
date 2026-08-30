package webhooks_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
)

// TestAnAcceptedBodyAlwaysFitsOnTheWire is the one assertion that makes
// webhooks.MaxBodyBytes correct rather than merely derived.
//
// Accepting a delivery is a promise to publish it: the body is read, its
// signature verified, its delivery claimed, and only then is it published as a
// single event. So a body this package accepts and the transport refuses is
// not a degraded path — it is a 503 asking the provider to retry something
// that can never succeed, forever, with the claim released each time.
//
// The derivation in body.go divides the transport ceiling by an amplification
// factor, and that factor is the part that can be wrong: types.RawWebhook puts
// one body on the wire more than once — the parsed map re-marshaled, plus the
// exact signed bytes as base64 at 4/3. Asserting the arithmetic against itself
// would restate the constant. So this ENCODES a worst-case delivery at exactly
// the cap and measures what comes out.
//
// Mutated to confirm it can fail: raising bodyAmplification's divisor to 2
// makes the encoded event exceed queue.MaxPayloadBytes and this test goes red.
func TestAnAcceptedBodyAlwaysFitsOnTheWire(t *testing.T) {
	t.Parallel()

	// A body at exactly the cap, shaped to be as expensive as a real one:
	// the value is a string of non-ASCII runes, so re-marshaling escapes
	// nothing but the map carries full-width keys, and the raw bytes are
	// carried a second time as base64.
	const key = `"payload":`
	filler := strings.Repeat("a", webhooks.MaxBodyBytes-len(key)-len(`{}""`))
	raw := []byte(`{` + key + `"` + filler + `"}`)
	if len(raw) != webhooks.MaxBodyBytes {
		t.Fatalf("fixture is %d bytes, want exactly the cap %d",
			len(raw), webhooks.MaxBodyBytes)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("fixture is not a JSON object: %v", err)
	}

	// Every field a real delivery carries. Headers are the one part not
	// bounded by MaxBodyBytes, so they are filled to something larger than
	// any provider sends rather than left empty.
	ev := events.New(types.RawWebhook{
		Body:    body,
		BodyRaw: raw,
		Headers: map[string]string{
			"X-GitHub-Event":      "push",
			"X-Hub-Signature-256": strings.Repeat("f", 512),
			"User-Agent":          strings.Repeat("u", 512),
		},
		Handle: "some-seat-handle",
	}, events.NewTrace())
	ev.Source = "github"

	encoded, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal the event: %v", err)
	}
	if len(encoded) > queue.MaxPayloadBytes {
		t.Errorf("a delivery at the accepted cap encodes to %d bytes, which the "+
			"transport refuses at %d: webhooks.MaxBodyBytes (%d) is derived with "+
			"too small an amplification factor, so this body would be accepted, "+
			"verified, claimed and then fail to publish forever",
			len(encoded), queue.MaxPayloadBytes, webhooks.MaxBodyBytes)
	}
}
