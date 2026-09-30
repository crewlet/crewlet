package chartapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// chartWrites are one request on every route that writes, as a person holding
// the company's and the deployment's grants — so a case about the operation id
// reaches each route's writer.
var chartWrites = []struct {
	name, method, path, body string
}{
	{"a unit's content", http.MethodPatch, "/chart/units/engineering", `{"name":"E"}`},
	{"a seat's content", http.MethodPatch, "/chart/seats/sre", `{"name":"S"}`},
	{"a batch", http.MethodPost, "/chart/batch",
		`{"operations":[{"kind":"create_seat","object":{"kind":"seat","id":"ops"},"parent":"engineering","seat_kind":"agent"}]}`},
	{"a rename", http.MethodPost, "/chart/units/engineering/rename", `{"to":"platform"}`},
}

// operator holds the company's grant and the deployment's.
func operator() iam.Principal {
	return leadOf(iam.GrantConfigWrite, iam.GrantFleetOperate)
}

// send runs one request, with the key a case chose where there is one.
func send(r rig, method, path, body, key string) (int, map[string]any, http.Header) {
	headers := map[string]string{}
	if key != "" {
		headers[opkey.Header] = key
	}
	var rec *httptest.ResponseRecorder
	switch method {
	case http.MethodPatch:
		rec = patchWith(r.mux, path, body, headers)
	default:
		rec = postWith(r.mux, path, body, headers)
	}
	res := rec.Result()
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out, res.Header
}

// derivedFrom holds the id a write was published under to the key its answer
// handed back: a step of that key, so it carries the key's instant and the
// trail finds it by the key the caller holds.
func derivedFrom(t *testing.T, op, key string) {
	t.Helper()
	if err := statelog.CheckCallerOpID(key); err != nil {
		t.Errorf("the answer handed back %q, which cannot be sent back: %v", key, err)
	}
	keyAt, _ := statelog.OpMintedAt(key)
	if at, ok := statelog.OpMintedAt(op); !ok || !at.Equal(keyAt) ||
		!strings.HasPrefix(op, key+".") {
		t.Errorf("published under %q, which is not a step of the key %q the "+
			"answer handed back", op, key)
	}
}

// EVERY CHART WRITE IS PUBLISHED UNDER AN OPERATION ID ITS LEDGER CAN VOUCH FOR.
//
// The publisher vouches for a retry by the instant its operation id carries.
// This surface minted a v4 uuid — no instant, read as minted at the epoch — so
// once the chart's ledger had swept anything every write it made was answered
// `unknown` without being published, on the first attempt too. The answer
// hands back the KEY the write's id is a step of, which is what a retry sends.
//
// Mutation: mint the key as a v4 again and every row goes red.
func TestEveryChartWriteIsPublishedUnderAnIDItsLedgerCanVouchFor(t *testing.T) {
	t.Parallel()
	for _, c := range chartWrites {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := serve(t, nil, operator(), leads())
			status, body, _ := send(r, c.method, c.path, c.body, "")
			if status != http.StatusOK || len(r.writer.opIDs) != 1 {
				t.Fatalf("answered %d %v (%v)", status, body, r.writer.opIDs)
			}
			key, _ := body["op_id"].(string)
			derivedFrom(t, r.writer.opIDs[0], key)
		})
	}
}

// ONE KEY IS ONE OPERATION ONLY FOR ONE REQUEST.
//
// The ledger answers an operation it already holds before the write is
// decided, so a write published under the key itself made the same key sent
// with ANOTHER body the first request's operation: answered `applied`, with
// nothing of the second written — an edit reported as made and silently
// dropped. The same request under the same key is the same operation, which
// is what a retry is; any other request under it is another, and lands.
//
// Mutation: publish under the key again and the second row's operation is the
// first's.
func TestAKeySentWithAnotherRequestIsAnotherOperation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, method, path, first, second string
	}{
		{"a unit's content", http.MethodPatch, "/chart/units/engineering",
			`{"name":"E"}`, `{"name":"Engineering"}`},
		{"a seat's content", http.MethodPatch, "/chart/seats/sre",
			`{"goal":"keep it up"}`, `{"goal":"keep it up, cheaply"}`},
		{"a rename", http.MethodPost, "/chart/units/engineering/rename",
			`{"to":"platform"}`, `{"to":"infrastructure"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := serve(t, nil, operator(), leads())
			key := statelog.NewOpID(time.Now(), "")
			for _, body := range []string{c.first, c.first, c.second} {
				if status, answer, _ := send(r, c.method, c.path, body, key); status != http.StatusOK ||
					answer["op_id"] != key {
					t.Fatalf("answered %d %v, want 200 handing back the key", status,
						answer)
				}
			}
			ops := r.writer.opIDs
			if len(ops) != 3 {
				t.Fatalf("published %v, want three writes", ops)
			}
			if ops[1] != ops[0] {
				t.Errorf("the same request under the same key was two operations, "+
					"%q and %q: a retry would land twice", ops[0], ops[1])
			}
			if ops[2] == ops[0] {
				t.Errorf("another request under the same key was the first one's "+
					"operation %q: the ledger answers it as landed and writes "+
					"nothing of it", ops[0])
			}
			for _, op := range ops {
				derivedFrom(t, op, key)
			}
		})
	}
}

// A KEY OUTSIDE THE GRAMMAR IS REFUSED BEFORE ANY WRITE.
//
// It was accepted as it stood — "the ledger keys on the string" — which is the
// id a ledger can never vouch for: every retry of it, the only thing a key is
// for, was answered `unknown` without being published. And one over the length
// bound was IGNORED, publishing under a fresh id: the double write the key was
// sent to prevent. Both are refused naming the header; the control is a key
// the engine minted, answered as sent and published as a step of it.
//
// Mutation: take the header as it stands again and the refused rows publish.
func TestAKeyOutsideTheGrammarIsRefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	for _, c := range chartWrites {
		for _, key := range []string{"retry-1",
			"0192f00d-0000-4000-8000-000000000001"} {
			t.Run(c.name+"/"+key, func(t *testing.T) {
				t.Parallel()
				r := serve(t, nil, operator(), leads())
				status, body, _ := send(r, c.method, c.path, c.body, key)
				if status != http.StatusBadRequest ||
					body["error"] != string(httpjson.CodeOpIDInvalid) ||
					body["message"] != httpjson.CodeOpIDInvalid.Message() ||
					body["field"] != opkey.Header {
					t.Fatalf("answered %d %v, want 400 op_id_invalid naming %s",
						status, body, opkey.Header)
				}
				if len(r.writer.calls) != 0 {
					t.Errorf("a refused key still wrote %v", r.writer.calls)
				}
			})
		}
		t.Run(c.name+"/the control", func(t *testing.T) {
			t.Parallel()
			r := serve(t, nil, operator(), leads())
			key := statelog.NewOpID(time.Now(), "chart-retry")
			status, body, _ := send(r, c.method, c.path, c.body, key)
			if status != http.StatusOK || len(r.writer.opIDs) != 1 ||
				body["op_id"] != key {
				t.Fatalf("a key the engine minted answered %d %v, published "+
					"under %v", status, body, r.writer.opIDs)
			}
			derivedFrom(t, r.writer.opIDs[0], key)
		})
	}
}

// EVERY 503 CARRIES THE OPERATION, AND SAYS WHETHER ANOTHER NODE COULD ANSWER.
//
// A refusal answered only its sentence, so a caller had no id to find the
// write by or send back. And an `unknown` this node's operation ledger cannot
// vouch for was not published at all: asked here again it answers the same way
// until the change reaches this node, so it carries no Retry-After and says
// `unvouched` — the control, a lost acknowledgement, says when.
//
// Mutation: drop the op id from the refusal, or the unvouched arm, and a row
// goes red.
func TestEveryChartUnavailableCarriesTheOperation(t *testing.T) {
	t.Parallel()
	t.Run("a refusal", func(t *testing.T) {
		t.Parallel()
		r := serve(t, nil, leadOf(), leads())
		r.writer.err = &statelog.Unavailable{Reason: statelog.ReasonBehind,
			Detail: "this node is behind"}
		status, body, header := send(r, http.MethodPatch, "/chart/units/engineering",
			`{"name":"E"}`, "")
		key, _ := body["op_id"].(string)
		if status != http.StatusServiceUnavailable || key == "" ||
			header.Get("Retry-After") == "" {
			t.Fatalf("answered %d %v (Retry-After %q), want 503 naming the "+
				"operation %v", status, body, header.Get("Retry-After"),
				r.writer.opIDs)
		}
		derivedFrom(t, r.writer.opIDs[0], key)
	})
	for _, unvouched := range []bool{true, false} {
		name := "a lost acknowledgement"
		if unvouched {
			name = "an unvouched unknown"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := serve(t, nil, leadOf(), leads())
			r.writer.outcome = statelog.OutcomeUnknown
			r.writer.unvouched = unvouched
			status, body, header := send(r, http.MethodPatch,
				"/chart/units/engineering", `{"name":"E"}`, "")
			retry := header.Get("Retry-After")
			switch {
			case status != http.StatusServiceUnavailable || body["op_id"] == nil:
				t.Errorf("answered %d %v, want 503 naming the operation", status, body)
			case unvouched && (retry != "" || body["unvouched"] != true):
				t.Errorf("an unvouched unknown answered Retry-After %q, "+
					"unvouched %v", retry, body["unvouched"])
			case !unvouched && (retry == "" || body["unvouched"] != nil):
				t.Errorf("a lost acknowledgement answered Retry-After %q, "+
					"unvouched %v", retry, body["unvouched"])
			}
		})
	}
}

// A WRITE ON AN OBJECT A REMOVAL TOOK IS NOT FOUND.
//
// A removal installs a gate, and a record about what it removed is refused
// ([statelog.ReasonDeleted]) — nothing will ever write that object again. It
// was the generic 503, which sent a caller looking for a node that would.
//
// Mutation: drop the removal arm and this answers 503.
func TestAWriteOnARemovedObjectIsNotFound(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(), leads())
	r.writer.err = &statelog.Unavailable{Reason: statelog.ReasonDeleted,
		Detail: "that unit was removed"}
	status, body, _ := send(r, http.MethodPatch, "/chart/units/engineering",
		`{"name":"E"}`, "")
	if status != http.StatusNotFound || body["error"] != string(httpjson.CodeNotFound) {
		t.Errorf("answered %d %v, want 404", status, body)
	}
}

// postWith is [post] with request headers.
func postWith(mux *http.ServeMux, path, body string,
	headers map[string]string) *httptest.ResponseRecorder {

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x"+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	mux.ServeHTTP(rec, req)
	return rec
}

// A KEY THAT ALREADY NAMES ANOTHER WRITE IS A CONFLICT THE CALLER SETTLES.
//
// The state log refuses a write whose operation id names a record on another
// object ([statelog.ReasonOpReused]) and writes nothing. A new key settles it
// and no wait does, so it is 409 naming the header, where it was a 503.
//
// Mutation: drop the arm and this answers 503.
func TestAKeyNamingAnotherWriteIsAConflict(t *testing.T) {
	t.Parallel()
	r := serve(t, nil, leadOf(), leads())
	r.writer.err = &statelog.Unavailable{Reason: statelog.ReasonOpReused,
		Detail: "that operation names another record"}
	key := statelog.NewOpID(time.Now(), "chart-unit")
	status, body, _ := send(r, http.MethodPatch, "/chart/units/engineering",
		`{"name":"E"}`, key)
	if status != http.StatusConflict || body["field"] != opkey.Header ||
		body["op_id"] != key {
		t.Errorf("answered %d %v, want 409 naming %s", status, body,
			opkey.Header)
	}
}

// A FRESH KEY IS MINTED ON THE SERVICE'S OWN CLOCK.
//
// [chartapi.Options.Now] is the clock this surface reads, documented as the
// one a test pins an operation id's instant with — and the key was minted off
// the wall clock beside it, so the instant the ledger vouches for a retry by
// was the one reading of time here nothing could pin.
//
// Mutation: mint the key off time.Now again and the instant is today's.
func TestAFreshKeyIsMintedOnTheServicesClock(t *testing.T) {
	t.Parallel()
	pinned := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	w := &writer{}
	svc, err := chartapi.New(chartapi.Options{
		Reader: &reader{},
		Authority: func(string, chart.AuthorKind, []iam.Grant,
			chart.Provenance) chartapi.Writer {
			return w
		},
		Principal: resolved(func() iam.Principal { return operator() }),
		Chart:     leads(),
		Now:       func() time.Time { return pinned },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	status, body, _ := send(rig{mux: mux, writer: w}, http.MethodPatch,
		"/chart/units/engineering", `{"name":"E"}`, "")
	key, _ := body["op_id"].(string)
	if status != http.StatusOK {
		t.Fatalf("answered %d %v", status, body)
	}
	if at, ok := statelog.OpMintedAt(key); !ok || !at.Equal(pinned) {
		t.Errorf("the key %q was minted at %s, want the service's clock's %s",
			key, at, pinned)
	}
}
