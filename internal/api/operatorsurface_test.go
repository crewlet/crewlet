package api_test

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
)

// switchedOperator is an operator surface whose catalogue a case replaces
// between requests: whatever server it holds at the request, or none.
type switchedOperator struct {
	current *atomic.Pointer[operator.Server]
}

func (s switchedOperator) Operator() (*operator.Server, bool) {
	server := s.current.Load()
	return server, server != nil
}

// THE OPERATOR SURFACE IS ASKED ON EVERY REQUEST: both of its transports serve
// the catalogue the surface answers at that request — absent while it answers
// none, a catalogue's own answer once it answers one, the NEXT catalogue's once
// it answers another, and absent again where it answers none again.
//
// What a captured catalogue was: the engine builds it from the native
// tracker, the knowledge base and the knowledge backend this node runs, and on
// a node that booted with no company those arrive with its first apply, after
// the app was assembled. A catalogue read once there offered such a node's
// people none of the tracker's or the knowledge base's tools for the life of
// the process. One app, four phases, so an implementation that resolves the
// surface once — at assembly or at the first request — answers a phase wrong.
func TestTheOperatorSurfaceIsAskedOnEveryRequest(t *testing.T) {
	t.Parallel()
	// A CATALOGUE OF READS, and one that also steers: `steer_turn` is a
	// gesture the act transport serves, and the first catalogue does not hold
	// it, so which catalogue answered is in the answer.
	reads := operatorSurface(t, operator.Options{
		Work: builtin.WorkDeps{Reader: stubWorkReader{}, Actor: operator.WorkActor(nil, nil)},
	}).server
	steers := operatorSurface(t, operator.Options{
		Work:  builtin.WorkDeps{Reader: stubWorkReader{}, Actor: operator.WorkActor(nil, nil)},
		Steer: builtin.SteerDeps{Asker: queuememory.New(), Actor: operator.WorkActor(nil, nil)},
	}).server
	current := &atomic.Pointer[operator.Server]{}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, Operator: switchedOperator{current: current}})

	act := operator.ActPathPrefix + builtin.SteerTurnTool
	code := func(path string) (int, string) {
		t.Helper()
		rec := send(t, a, http.MethodPost, path)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body["error"]
	}
	none := func(phase string) {
		t.Helper()
		for _, path := range []string{act, operator.MCPPath} {
			if rec := send(t, a, http.MethodPost, path); !absent(t, rec) {
				t.Errorf("%s: POST %s answered %d %s, want the mux's own 404",
					phase, path, rec.Code, rec.Body)
			}
		}
	}

	none("before any catalogue")

	current.Store(reads)
	// THE ACT TRANSPORT OF THE CATALOGUE OF READS, which holds no steer.
	if status, got := code(act); status != http.StatusNotFound ||
		got != string(operator.CodeUnknownTool) {
		t.Errorf("with the catalogue of reads: the steer answered %d %q, want its "+
			"act transport's %s", status, got, operator.CodeUnknownTool)
	}
	// AND ITS MCP TRANSPORT, which answers a body that is no JSON-RPC message
	// itself rather than leaving it to the mux.
	if rec := send(t, a, http.MethodPost, operator.MCPPath); absent(t, rec) {
		t.Errorf("with a catalogue: POST %s answered the mux's own 404", operator.MCPPath)
	}

	current.Store(steers)
	// THE NEXT CATALOGUE'S, which serves the steer: refused for its body
	// (`send` declares no JSON), which only a transport holding the tool says.
	if status, got := code(act); status != http.StatusUnsupportedMediaType ||
		got != string(operator.CodeUnsupportedMediaType) {
		t.Errorf("with the catalogue that steers: the steer answered %d %q, want its "+
			"act transport's %s", status, got, operator.CodeUnsupportedMediaType)
	}

	current.Store(nil)
	none("after the catalogue went")
}

// THE OPERATOR'S MCP TRANSPORT IS A POST AND NOTHING ELSE. It keeps no session,
// so it offers no server-to-client stream for a GET to open and no session for
// a DELETE to end, and both are answered the mux's own 405 naming POST: the
// status MCP's streamable-HTTP transport reads as "this server offers no
// stream", carrying a code like every other refusal the API writes — which the
// SDK's own 405, in plain text, would not.
func TestTheOperatorMCPTransportIsAPost(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, Operator: operatorSurface(t, operator.Options{
		Work: builtin.WorkDeps{Reader: stubWorkReader{}, Actor: operator.WorkActor(nil, nil)},
	})})
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec := send(t, a, method, operator.MCPPath)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusMethodNotAllowed ||
			body["error"] != string(httpjson.CodeMethodNotAllowed) {
			t.Errorf("%s %s answered %d %s, want the mux's own 405 %s",
				method, operator.MCPPath, rec.Code, rec.Body, httpjson.CodeMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Errorf("%s %s: Allow = %q, want %s", method, operator.MCPPath, allow,
				http.MethodPost)
		}
	}
	// AND THE POST REACHES THE TRANSPORT, which answers a body that is no
	// JSON-RPC message itself.
	if rec := send(t, a, http.MethodPost, operator.MCPPath); rec.Code == http.StatusNotFound ||
		rec.Code == http.StatusMethodNotAllowed {
		t.Errorf("POST %s answered %d %s, want the transport's own answer",
			operator.MCPPath, rec.Code, rec.Body)
	}
}
