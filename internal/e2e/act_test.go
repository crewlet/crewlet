package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The act transport, as the dashboard writes through it.
//
// EVERY CHANGE A PERSON MAKES FROM THE DASHBOARD is `POST /operator/act/{tool}`
// (ADR-0024): one tool of the operator catalogue per request, made as the
// person the browser's session resolves to, under an operation key the client
// minted when the gesture began and sends in `Idempotency-Key`. A case whose
// subject is what a person's screen sees therefore writes through this route
// as well, rather than through the engine's store beneath it — the route is
// where the session becomes a seat, the key becomes the write's operation and
// the tool's own rules are met, and a case that went round it certified a
// write no person can make.

// actCapture is one `/operator/act` exchange as a client sees it: the tool and
// arguments sent, the operation key they were sent under in the
// `Idempotency-Key` header, and the engine's answer, status and body byte for
// byte — what the client replay hands the dashboard's own `act`.
type actCapture struct {
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	OpID   string         `json:"op_id"`
	Status int            `json:"status"`
	Body   string         `json:"body"`

	// Reads names the questions that read the log this write landed on —
	// the dashboard's own table's (`SESSION_QUERIES`), written down by the
	// case — which the client replay holds the floor the write raised
	// against. Nothing the engine answers.
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

// act makes one change through the act transport as the person this browser
// is signed in as, under an operation key of its own, and returns the exchange
// whatever the engine answered.
//
// A UUIDv7, as the dashboard's `act` mints one: the key carries the instant
// its operation began, which is what a node judges a retry by.
func (b *browser) act(n *node, tool string, args map[string]any) actCapture {
	b.t.Helper()
	exchange := actCapture{Tool: tool, Args: args, OpID: uuid.Must(uuid.NewV7()).String()}
	var raw []byte
	exchange.Status, raw = b.sendRaw(n, http.MethodPost, "/operator/act/"+tool,
		exchange.OpID, map[string]any{"args": args})
	exchange.Body = string(raw)
	return exchange
}

// acted is [browser.act] for a change the case needs to have landed: answered
// 200, applied or pending, and waited for on this node at the position it
// names — so what the case reads from this node next includes it, as the
// dashboard's own read after a write does at the floor the answer raised.
func (b *browser) acted(n *node, tool string, args map[string]any) actAnswer {
	b.t.Helper()
	answer := answerOf(b.t, b.act(n, tool, args))
	if answer.Position == nil {
		// APPLIED AT NO POSITION: a write on no log — a read state — and
		// so nothing to wait for.
		return answer
	}
	if err := n.engine.WaitReached(b.t.Context(), positionOf(b.t, answer)); err != nil {
		b.t.Fatalf("waiting for a write at %s: %v", *answer.Position, err)
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
// takes — failing the case for an answer that names none, or one a read could
// not take back as `min_position`.
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
