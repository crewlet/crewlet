package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The act transport, as the dashboard writes through it.
//
// EVERY CHANGE A PERSON MAKES FROM THE DASHBOARD is `POST /operator/act/{tool}`
// (ADR-0024): one tool of the operator catalogue per request, made as the
// person the bearer token is bound to, under a request id the client minted
// when the gesture began. A case whose subject is what a person's screen sees
// therefore writes through this route as well, rather than through the
// engine's store beneath it — the route is where the token becomes a seat, the
// request id becomes the write's operation and the tool's own rules are met,
// and a case that went round it certified a write no person can make.

// actCapture is one `/operator/act` exchange as a client sees it: the tool and
// arguments sent, the request id they were sent under, and the engine's
// answer, status and body byte for byte.
type actCapture struct {
	Tool      string         `json:"tool"`
	Args      map[string]any `json:"args"`
	RequestID string         `json:"request_id"`
	Status    int            `json:"status"`
	Body      string         `json:"body"`

	// Reads names the questions that read the log this write landed on —
	// the dashboard's own table's, written down by the case — which the
	// client replay holds the floor the write raised against. Nothing the
	// engine answers.
	Reads []string `json:"reads,omitempty"`
}

// actAnswer is what the act transport answers a change that went through
// ([operator.ActAnswer]): the outcome, where its record landed, and the tool's
// own receipt verbatim.
type actAnswer struct {
	Outcome  string          `json:"outcome"`
	Position *string         `json:"position"`
	Receipt  json.RawMessage `json:"receipt"`
}

// request sends one request to this node's API with token as its bearer
// credential, and returns the status and the body.
func (n *node) request(t *testing.T, token, method, path string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, n.server.URL+path,
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := n.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the answer: %v", method, path, err)
	}
	return res.StatusCode, raw
}

// act makes one change through the act transport as the person token is bound
// to, under a request id of its own, and returns the exchange whatever the
// engine answered.
//
// A UUIDv7, as the dashboard mints one: the transport derives the write's
// operation from it and judges that operation by the instant the id carries,
// so an id without one is refused before anything is written.
func (n *node) act(t *testing.T, token, tool string, args map[string]any) actCapture {
	t.Helper()
	exchange := actCapture{Tool: tool, Args: args, RequestID: uuid.Must(uuid.NewV7()).String()}
	body, err := json.Marshal(map[string]any{
		"request_id": exchange.RequestID, "args": exchange.Args,
	})
	if err != nil {
		t.Fatalf("encoding the %s request: %v", tool, err)
	}
	status, raw := n.request(t, token, http.MethodPost, "/operator/act/"+tool, body)
	exchange.Status, exchange.Body = status, string(raw)
	return exchange
}

// acted is [node.act] for a change the case needs to have landed: answered
// 200, applied or pending, and waited for on this node at the position it
// names — so what the case reads from this node next includes it, as the
// dashboard's own read after a write does at the floor the answer raised.
func (n *node) acted(t *testing.T, token, tool string, args map[string]any) actAnswer {
	t.Helper()
	answer := answerOf(t, n.act(t, token, tool, args))
	if answer.Position == nil {
		// APPLIED AT NO POSITION: a write on no log — a read state — or one
		// that appended nothing, and either way nothing to wait for.
		return answer
	}
	at := positionOf(t, answer)
	if err := n.engine.WaitCommitted(t.Context(), at); err != nil {
		t.Fatalf("waiting for a write at %s: %v", at, err)
	}
	return answer
}

// answerOf reads an exchange that has to have gone through, failing the case
// otherwise: answered 200, and applied or pending.
func answerOf(t *testing.T, exchange actCapture) actAnswer {
	t.Helper()
	var answer actAnswer
	if exchange.Status != http.StatusOK || json.Unmarshal([]byte(exchange.Body), &answer) != nil {
		t.Fatalf("%s %v was not answered: %d %s", exchange.Tool, exchange.Args,
			exchange.Status, exchange.Body)
	}
	if answer.Outcome != string(statelog.OutcomeApplied) &&
		answer.Outcome != string(statelog.OutcomePending) {
		t.Fatalf("%s came to %q, want applied or pending: %s", exchange.Tool,
			answer.Outcome, exchange.Body)
	}
	return answer
}

// positionOf is where an answer says its record landed, in the form a wait
// takes — failing the case for an answer that names none, or one a read
// could not take back as `min_position`.
func positionOf(t *testing.T, answer actAnswer) statelog.Position {
	t.Helper()
	if answer.Position == nil {
		t.Fatalf("an act answered %s at no position", answer.Outcome)
	}
	at, err := statelog.ParsePosition(*answer.Position)
	if err != nil {
		t.Fatalf("an act answered a position a read cannot take back: %v", err)
	}
	return at
}

// receiptOf decodes the tool's own answer an act carried.
func receiptOf[T any](t *testing.T, answer actAnswer) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(answer.Receipt, &out); err != nil {
		t.Fatalf("decoding a receipt %s: %v", answer.Receipt, err)
	}
	return out
}
