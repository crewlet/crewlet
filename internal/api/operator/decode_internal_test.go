package operator

import (
	"encoding/json"
	"testing"
)

// AN OPERATOR CALL'S NUMBERS ARRIVE EXACTLY, on both transports.
//
// Every other path into these tools reads a call's arguments through
// json.Number — a seat's from its provider, a coding agent's over the bridge
// — and the tools are written for that. The act transport and the MCP
// transport decoded with the default decoder, which turns every JSON number
// into a float64 first, so an integer past 2^53 was rounded before any tool
// saw it: a packed log position, whose generation sits above bit 40, became a
// place its sender never named. Each decoder still refuses a second value
// after the object, as the decoder it replaced did.
func TestAnOperatorCallsNumbersArriveExactly(t *testing.T) {
	t.Parallel()
	const past = "9007199254740993" // 2^53 + 1, the first integer a float64 rounds

	req, err := decodeAct([]byte(`{"request_id": "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b", ` +
		`"args": {"position": ` + past + `}}`))
	if err != nil {
		t.Fatalf("decodeAct: %v", err)
	}
	if got, ok := req.Args["position"].(json.Number); !ok || got.String() != past {
		t.Errorf("the act transport read position as %#v, want json.Number(%s)",
			req.Args["position"], past)
	}

	args, err := decodeArgs(json.RawMessage(`{"position": ` + past + `}`))
	if err != nil {
		t.Fatalf("decodeArgs: %v", err)
	}
	if got, ok := args["position"].(json.Number); !ok || got.String() != past {
		t.Errorf("the MCP transport read position as %#v, want json.Number(%s)",
			args["position"], past)
	}

	if _, err := decodeAct([]byte(`{"request_id": "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b", ` +
		`"args": {}} {}`)); err == nil {
		t.Error("the act transport accepted a second JSON value after the object")
	}
	if _, err := decodeArgs(json.RawMessage(`{"position": 1} {}`)); err == nil {
		t.Error("the MCP transport accepted a second JSON value after the object")
	}
}
