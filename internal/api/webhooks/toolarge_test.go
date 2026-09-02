package webhooks_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
)

// encodedRawWebhookSize is what one body costs on the wire: the published
// event carries the parsed map AND the exact signed bytes, exactly as
// Receiver.accept builds it.
func encodedRawWebhookSize(t *testing.T, filler string) int {
	t.Helper()
	raw := []byte(`{"payload":"` + filler + `"}`)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("fixture is not a JSON object: %v", err)
	}
	ev := events.New(types.RawWebhook{Body: body, BodyRaw: raw}, events.NewTrace())
	ev.Source = "github"
	encoded, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return len(encoded)
}

// TestAnOversizedDeliveryIsRefusedPermanently pins the difference between the
// two ways a publish can fail, which is the whole of this fix.
//
// A broker that is down comes back, so a delivery it could not take is worth
// retrying and answers 503. A delivery that does not fit on the wire will not
// fit on the next attempt either: answering THAT as an outage asks the
// provider to repeat a request that can never succeed — claiming and releasing
// on every pass, forever.
//
// It is asserted through the real HTTP edge rather than against the classifier,
// because the status code is the entire observable: everything upstream of it
// (verify, claim, publish, release) already worked, and the bug was in what the
// last line said about it.
func TestAnOversizedDeliveryIsRefusedPermanently(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.published.fail(fmt.Errorf("publish: %w", queue.ErrTooLarge))

	res := e.post(t, "/webhooks/github", issueBody, githubDelivery(issueBody, "gh-secret"))
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413: a delivery too large to publish must not be "+
			"reported as a transient outage, or the provider retries it forever",
			res.Code)
	}
	// No Retry-After: there is nothing to come back for.
	if got := res.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q on a permanent refusal", got)
	}
}

// TestATransientPublishFailureStillAsksForARetry is the other half, and the
// reason the case above cannot simply refuse every publish failure: a broker
// blip must still be retried.
func TestATransientPublishFailureStillAsksForARetry(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.published.fail(errors.New("nats: connection closed"))

	res := e.post(t, "/webhooks/github", issueBody, githubDelivery(issueBody, "gh-secret"))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 — a broker that is down comes back", res.Code)
	}
}

// TestTheEncodedSizeIsWhatDecides is the finding that withdrew the derived
// body cap: a webhook body's cost on the wire is a property of its BYTES, not
// its length, so no divisor at the edge can decide this.
//
// encoding/json escapes '<', '>' and '&' to six bytes each, and the published
// event carries the parsed body re-marshaled ALONGSIDE the exact signed bytes
// as base64. So two bodies of identical length reach the transport at very
// different sizes, and a cap derived from length alone is either wrong for one
// or wasteful for the other. Measured here rather than asserted, because the
// ratio is the thing that was got wrong.
func TestTheEncodedSizeIsWhatDecides(t *testing.T) {
	t.Parallel()
	const n = 100_000

	plain := encodedRawWebhookSize(t, strings.Repeat("a", n))
	escaped := encodedRawWebhookSize(t, strings.Repeat("&", n))

	if escaped <= plain {
		t.Fatalf("escape-heavy body encoded to %d bytes and plain to %d: if these "+
			"were equal a length-derived cap would be sound", escaped, plain)
	}
	// The plain case is the ~2.3x the withdrawn cap assumed; the escaped
	// case is ~7.3x. Pinned loosely — the point is the GAP, not the digits.
	if ratio := float64(escaped) / float64(plain); ratio < 2.0 {
		t.Errorf("escaped/plain ratio is %.2f; the withdrawn derivation assumed "+
			"one factor covered both, and this is the measurement that says it "+
			"cannot", ratio)
	}
}

// EVERY 413 ON THIS SURFACE ANSWERS ONE CODE.
//
// `httpjson.CodeBodyTooLarge`'s own doc calls itself "the single spelling of a
// 413, whether what overflowed was a config document, a secret value or a
// webhook delivery" — and a webhook delivery was the case that did not. Both
// sites here answered prose (`delivery too large to queue`, `body too large`)
// where every other surface answers `body_too_large`, so a client branching on
// the code saw a 413 it could not classify from exactly the route the doc
// names.
func TestBothOversizeRefusalsAnswerTheOneCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body func(*edge) ([]byte, map[string]string)
	}{
		{"too large to publish", func(e *edge) ([]byte, map[string]string) {
			e.published.fail(fmt.Errorf("publish: %w", queue.ErrTooLarge))
			return issueBody, githubDelivery(issueBody, "gh-secret")
		}},
		{"too large to read", func(*edge) ([]byte, map[string]string) {
			big := []byte(strings.Repeat("x", webhooks.MaxBodyBytes+1))
			return big, githubDelivery(big, "gh-secret")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			body, headers := tc.body(e)
			res := e.post(t, "/webhooks/github", body, headers)
			if res.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("got %d, want 413: %s", res.Code, res.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
				t.Fatalf("the refusal is not JSON: %q", res.Body.String())
			}
			if got["error"] != string(httpjson.CodeBodyTooLarge) {
				t.Errorf("error = %v, want %q — a client cannot classify a 413 "+
					"whose code is prose", got["error"], httpjson.CodeBodyTooLarge)
			}
		})
	}
}
