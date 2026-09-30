package workapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// assertMintedWith holds one operation id a write was published under to the
// key it was derived from: in the engine's grammar, and carrying the KEY'S
// instant — the one the publisher vouches for a retry by.
func assertMintedWith(t *testing.T, opID, key string) {
	t.Helper()
	keyAt, ok := statelog.OpMintedAt(key)
	if !ok {
		t.Fatalf("the key %q carries no instant", key)
	}
	at, ok := statelog.OpMintedAt(opID)
	if !ok {
		t.Fatalf("the write was published as %q, which carries no instant: "+
			"once this node's ledger has swept anything it is answered "+
			"`unknown` without being published", opID)
	}
	if !at.Equal(keyAt) {
		t.Errorf("the write %q carries %s, want its key's %s — the instant a "+
			"retry under that key is vouched for by", opID, at, keyAt)
	}
}

// A KEY OUTSIDE THE OPERATION-ID GRAMMAR IS REFUSED BEFORE ANYTHING IS WRITTEN.
//
// Every id a request's writes derive from its key carries the key's instant,
// and one that carries none reads as minted at the epoch — before every loss
// the ledger will ever have — so once it had swept anything each such write
// was answered `unknown` without being published, for ever. The knowledge base
// refused such a key outright, so every page write through this surface
// failed. Held here to the one rule every surface holds a caller's id to, on
// every route family that writes: a tool-backed one, a person's record, the
// three the tracker's writer is called for directly, and the page verbs.
//
// Mutation: pass the header through unchecked and every case publishes.
func TestAKeyOutsideTheGrammarIsRefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, method, target string
		body                 any
	}{
		{"a tool-backed route", http.MethodPost, "/work/items",
			map[string]any{"title": "rotate the key", "project": "ENG"}},
		{"somebody's record", http.MethodPut, "/work/people/ana/pins",
			map[string]any{"items": []string{}}},
		{"a rank move", http.MethodPost, "/work/items/ENG-1/rank",
			map[string]any{"after": "ENG-2"}},
		{"a purge", http.MethodPost,
			"/work/items/t-1/purge?confirm=ENG-1&reason=why", nil},
		{"a page's trash", http.MethodDelete, "/pages/p-1", nil},
		{"a page written through its tool", http.MethodPost, "/pages",
			map[string]any{"container": "ENG", "title": "Notes", "body": "text"}},
	} {
		for _, key := range []string{"k-1", "0192f00d-0000-4000-8000-000000000001"} {
			t.Run(c.name+"/"+key, func(t *testing.T) {
				t.Parallel()
				r := newRig(t, chart{})
				got := r.do(as(admin("ana")), c.method, c.target, c.body,
					workapi.IdempotencyHeader, key)
				if got.status != http.StatusBadRequest ||
					got.body["error"] != string(httpjson.CodeOpIDInvalid) ||
					got.body["message"] != httpjson.CodeOpIDInvalid.Message() ||
					got.body["field"] != workapi.IdempotencyHeader {
					t.Fatalf("answered %d %v, want 400 op_id_invalid naming %s",
						got.status, got.body, workapi.IdempotencyHeader)
				}
				if len(r.writes.opIDs) != 0 || len(r.kb.did) != 0 {
					t.Errorf("a refused key still wrote %v %v", r.writes.opIDs,
						r.kb.did)
				}
			})
		}
	}
}

// EVERY WRITE A REQUEST MAKES CARRIES ITS KEY'S INSTANT, the key it was sent or
// the one minted for it.
//
// A request with no key was given a v4 uuid, and the tools' actor was handed
// the key without the instant it was minted at — so every id the tracker's
// tools derived from it read as minted at the epoch, and the page writes
// received a key their store refuses. The answer's `op_id` is the key, and
// every write is derived from it at its instant.
//
// Mutation: mint the key as a v4 again, or drop the actor's WorkSince, and a
// case here goes red.
func TestEveryWriteCarriesItsKeysInstant(t *testing.T) {
	t.Parallel()
	given := statelog.NewOpID(time.Now().Add(-time.Hour), "")
	for _, key := range []string{"", given} {
		name := "minted"
		if key != "" {
			name = "given"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			headers := []string{}
			if key != "" {
				headers = []string{workapi.IdempotencyHeader, key}
			}
			r := newRig(t, chart{})

			created := r.do(as(admin("ana")), http.MethodPost, "/work/items",
				map[string]any{"title": "rotate the key", "project": "ENG"},
				headers...)
			answeredKey, _ := created.body["op_id"].(string)
			if created.status != http.StatusOK || answeredKey == "" ||
				(key != "" && answeredKey != key) {
				t.Fatalf("the create answered %d with op_id %q: %v",
					created.status, answeredKey, created.body)
			}
			if err := statelog.CheckCallerOpID(answeredKey); err != nil {
				t.Errorf("the key handed back cannot be sent back: %v", err)
			}
			assertMintedWith(t, r.writes.opIDs[0], answeredKey)
			if since := r.writes.actors[0].WorkSince; since.IsZero() {
				t.Error("the tools' actor carries no instant for its key")
			}

			ranked := r.do(as(admin("ana")), http.MethodPost,
				"/work/items/ENG-1/rank", map[string]any{"after": "ENG-2"}, headers...)
			rankKey, _ := ranked.body["op_id"].(string)
			if ranked.status != http.StatusOK {
				t.Fatalf("the rank move answered %d: %v", ranked.status, ranked.body)
			}
			assertMintedWith(t, r.writes.opIDs[len(r.writes.opIDs)-1], rankKey)

			page := r.do(as(admin("ana")), http.MethodDelete, "/pages/p-1", nil,
				headers...)
			if page.status != http.StatusOK {
				t.Fatalf("the trash answered %d: %v", page.status, page.body)
			}
			// THE PAGE STORE IS HANDED THE KEY BOUND TO THIS REQUEST —
			// which it holds to the grammar and dates every id by — and
			// that carries the instant of the key the answer hands back.
			opKey := r.kb.actors[len(r.kb.actors)-1].OpKey
			if err := statelog.CheckCallerOpID(opKey); err != nil {
				t.Errorf("the page store was handed the key %q, which it "+
					"refuses: %v", opKey, err)
			}
			pageKey, _ := page.body["op_id"].(string)
			assertMintedWith(t, opKey, pageKey)
		})
	}
}

// AN UNKNOWN THIS NODE CANNOT SETTLE SENDS THE CLIENT ELSEWHERE; ONE IT CAN
// SENDS IT BACK HERE.
//
// Both are 503 with the key, and they differ in the one thing a client acts on:
// a lost acknowledgement is settled by the same request here, so it says when;
// an unvouched one was not published, and the same request here answers the
// same way until the change reaches this node — so it carries no Retry-After,
// says `unvouched`, and names another node. Held on a tool-backed route, whose
// tool reports the unknown as a failed result, and on a write this surface
// makes itself.
//
// Mutation: drop the unvouched arm and the second case carries a Retry-After.
func TestAnUnvouchedUnknownSaysAnotherNodeCanAnswer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, method, target string
		body                 any
	}{
		{"a tool-backed route", http.MethodPost, "/work/items",
			map[string]any{"title": "rotate the key", "project": "ENG"}},
		{"a write this surface makes", http.MethodPost, "/work/items/ENG-1/rank",
			map[string]any{"after": "ENG-2"}},
	} {
		for _, unvouched := range []bool{false, true} {
			name := c.name + "/a lost acknowledgement"
			if unvouched {
				name = c.name + "/unvouched"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := newRig(t, chart{})
				r.writes.result = statelog.Result{Outcome: statelog.OutcomeUnknown,
					Unvouched: unvouched}
				key := statelog.NewOpID(time.Now(), "")
				got := r.do(as(admin("ana")), c.method, c.target, c.body,
					workapi.IdempotencyHeader, key)
				if got.status != http.StatusServiceUnavailable || got.body["op_id"] != key {
					t.Fatalf("answered %d %v, want 503 carrying the key", got.status,
						got.body)
				}
				switch retry := got.header.Get("Retry-After"); {
				case unvouched && (retry != "" || got.body["unvouched"] != true):
					t.Errorf("an unvouched unknown answered Retry-After %q and "+
						"unvouched %v: the same request here answers the same way",
						retry, got.body["unvouched"])
				case !unvouched && (retry == "" || got.body["unvouched"] != nil):
					t.Errorf("a lost acknowledgement answered Retry-After %q and "+
						"unvouched %v: the same request here settles it", retry,
						got.body["unvouched"])
				}
			})
		}
	}
}

// WHAT A PURGE DESTROYED IS NOT FOUND, NOT UNAVAILABLE.
//
// A write refused because its object carries a permanent deletion marker will
// be refused on every node for ever; answered 503 it sent a client looking for
// a node that would take it.
//
// Mutation: drop the deletion arm and both answer 503.
func TestAWriteOnWhatAPurgeDestroyedIsNotFound(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, method, target string
		body                 any
	}{
		{"a tool-backed route", http.MethodPatch, "/work/items/ENG-1",
			map[string]any{"status": "done"}},
		{"a write this surface makes", http.MethodPost, "/work/items/ENG-1/rank",
			map[string]any{"after": "ENG-2"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, chart{})
			r.writes.err = &statelog.Unavailable{Reason: statelog.ReasonDeleted,
				Detail: "ENG-1 was purged"}
			if got := r.do(as(admin("ana")), c.method, c.target, c.body); got.status != http.StatusNotFound {
				t.Errorf("answered %d %v, want 404", got.status, got.body)
			}
		})
	}
}

// A STOPPED SUBTREE IS FINISHED ON THIS SURFACE BY A NEW KEY, NOT BY AN
// ARGUMENT THE ROUTE REFUSES.
//
// The tools answer a removal whose walk stopped part of the way with an
// instruction for the operator's assistant — call again "WITHOUT an op_id" —
// and this route refuses an `op_id` in its body. What makes a new operation
// here is a new Idempotency-Key, which finishes the walk whether or not this
// node's ledger can vouch for the step it stopped at.
//
// Mutation: pass the tool's sentence through and it names `op_id`.
func TestAStoppedSubtreeSaysHowToFinishItHere(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	r.writes.err = &tracker.SubtreeStopped{Verb: "removal", Root: "t-1",
		Followed: 2, Of: 5, OpID: "op", Err: tracker.ErrStepUnvouched}
	got := r.do(as(admin("ana")), http.MethodDelete, "/work/items/ENG-1",
		map[string]any{"subtree": true})
	stopped, _ := got.body["subtree_stopped"].(string)
	if got.status != http.StatusOK || stopped == "" {
		t.Fatalf("answered %d %v, want the root's receipt with how to finish",
			got.status, got.body)
	}
	if strings.Contains(stopped, "op_id") ||
		!strings.Contains(stopped, workapi.IdempotencyHeader) ||
		!strings.Contains(stopped, "2 of the 5") {
		t.Errorf("the instruction is %q, want it to name a new %s and the "+
			"counts", stopped, workapi.IdempotencyHeader)
	}
}

// AN `op_id` IN A BODY IS REFUSED AS THE CALLER'S TO CHANGE.
//
// The tools served here derive every write from the request's key, so a body
// `op_id` is one they refuse — in a sentence for the operator's assistant, as a
// failure with no cause, which answered `422 refused`. It is a request shaped
// for another surface, so it is `400`, before anything is written.
//
// Mutation: drop the check and the tool-backed row answers 422.
func TestAnOperationIDInABodyIsTheCallersToChange(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, method, target string
	}{
		{"a tool-backed route", http.MethodPost, "/work/items"},
		{"somebody's record", http.MethodPut, "/work/people/ana/pins"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, chart{})
			got := r.do(as(admin("ana")), c.method, c.target, map[string]any{
				"title": "rotate the key", "project": "ENG",
				"op_id": statelog.NewOpID(time.Now(), "create_work_item")})
			if got.status != http.StatusBadRequest ||
				got.body["error"] != string(httpjson.CodeInvalidBody) {
				t.Errorf("answered %d %v, want 400 invalid_body", got.status, got.body)
			}
			if len(r.writes.opIDs) != 0 {
				t.Errorf("wrote %v", r.writes.opIDs)
			}
		})
	}
}

// A KEY THAT ALREADY NAMES ANOTHER WRITE IS A CONFLICT THE CALLER SETTLES.
//
// The state log refuses a write whose operation id names a record on another
// object ([statelog.ReasonOpReused]) — a key sent back with a request it was
// not answered for — and nothing is written. A new key settles it and no wait
// does, so it is 409 naming the header, where it was the 503 every refusal
// was.
//
// Mutation: drop the arm and both rows answer 503.
func TestAKeyNamingAnotherWriteIsAConflict(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, method, target string
		body                 any
	}{
		{"a tool-backed route", http.MethodPatch, "/work/items/ENG-1",
			map[string]any{"status": "done"}},
		{"a write this surface makes", http.MethodPost, "/work/items/ENG-1/rank",
			map[string]any{"after": "ENG-2"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, chart{})
			r.writes.err = &statelog.Unavailable{Reason: statelog.ReasonOpReused,
				Detail: "that operation names another record"}
			key := statelog.NewOpID(time.Now(), "")
			got := r.do(as(admin("ana")), c.method, c.target, c.body,
				workapi.IdempotencyHeader, key)
			if got.status != http.StatusConflict ||
				got.body["field"] != workapi.IdempotencyHeader || got.body["op_id"] != key {
				t.Errorf("answered %d %v, want 409 naming %s", got.status, got.body,
					workapi.IdempotencyHeader)
			}
		})
	}
}

// AN UNKNOWN STILL SAYS WHAT IT WAS ABOUT.
//
// A write this surface makes answers its receipt — the item, the comment, the
// page — beside its outcome, and the unknown answer used to drop it, leaving a
// client holding an operation key and nothing saying which object to read to
// see whether it landed.
//
// Mutation: drop the receipt from the unknown answer and `item` is gone.
func TestAnUnknownStillSaysWhatItWasAbout(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	r.writes.result = statelog.Result{Outcome: statelog.OutcomeUnknown}
	got := r.do(as(admin("ana")), http.MethodPost, "/work/items/ENG-1/rank",
		map[string]any{"after": "ENG-2"})
	if got.status != http.StatusServiceUnavailable || got.body["item"] != "ENG-1" ||
		got.body["error"] != string(httpjson.CodeUnavailable) {
		t.Errorf("answered %d %v, want 503 naming ENG-1", got.status, got.body)
	}
}

// ONE KEY IS ONE OPERATION ONLY FOR ONE REQUEST.
//
// The ledger answers an operation it already holds before the write is
// decided, so an id derived from the key, the verb and the object alone made
// the same key sent with ANOTHER request the first request's operation:
// answered `applied`, with nothing of the second written — a card dropped
// elsewhere, a remark rewritten, a page renamed or saved again, each reported
// as made and silently dropped. The tools behind the other routes already put
// a digest of their arguments in every id they derive; the writes this surface
// makes itself, and the key the knowledge base derives from, are held to the
// same rule here: the same request under the same key is the same operation
// (the retry), and any other request under it is another.
//
// Mutation: drop the arguments from [keyedOp] and the tracker rows go red;
// hand the knowledge base the request's key unbound and the page rows do.
func TestAKeySentWithAnotherRequestIsAnotherOperation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, method, target string
		first, second        any
		headers              []string
		// op is the operation the case's one write was published under.
		op func(r *rig) []string
	}{
		{name: "a rank move", method: http.MethodPost,
			target: "/work/items/ENG-1/rank",
			first:  map[string]any{"after": "ENG-2"},
			second: map[string]any{"before": "ENG-2"},
			op:     func(r *rig) []string { return r.writes.opIDs }},
		{name: "a remark's edit", method: http.MethodPatch,
			target: "/work/items/ENG-1/comments/c-1",
			first:  map[string]any{"body": "second"},
			second: map[string]any{"body": "third"},
			op:     func(r *rig) []string { return r.writes.opIDs }},
		{name: "a purge", method: http.MethodPost,
			target: "/work/items/t-1/purge?confirm=ENG-1&reason=",
			first:  "an erasure request", second: "a retention request",
			op: func(r *rig) []string { return r.writes.opIDs }},
		{name: "a page's rename", method: http.MethodPost,
			target: "/pages/p-1/rename",
			first:  map[string]any{"title": "Runbook v2"},
			second: map[string]any{"title": "Runbook v3"},
			op:     pageKeys},
		{name: "a page saved through its tool", method: http.MethodPut,
			target: "/pages/p-1", headers: []string{"If-Match", "2"},
			first:  map[string]any{"body": "new steps"},
			second: map[string]any{"body": "newer steps"},
			op:     pageKeys},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, chart{})
			r.reader.addComment("t-1", tracker.Comment{ID: "c-1", Task: "t-1",
				Author: "ana", AuthorKind: tracker.AuthorHuman, Body: "first"})
			key := statelog.NewOpID(time.Now(), "")
			send := func(body any) {
				t.Helper()
				target := c.target
				if reason, isReason := body.(string); isReason {
					target, body = target+strings.ReplaceAll(reason, " ", "+"), nil
				}
				headers := append([]string{workapi.IdempotencyHeader, key},
					c.headers...)
				if got := r.do(as(admin("ana")), c.method, target, body,
					headers...); got.status != http.StatusOK {
					t.Fatalf("answered %d: %v", got.status, got.body)
				}
			}
			send(c.first)
			send(c.first)
			send(c.second)
			ops := c.op(r)
			if len(ops) != 3 {
				t.Fatalf("published %d writes, want 3: %v", len(ops), ops)
			}
			if ops[1] != ops[0] {
				t.Errorf("the same request under the same key was two "+
					"operations, %q and %q: a retry would land twice", ops[0], ops[1])
			}
			if ops[2] == ops[0] {
				t.Errorf("another request under the same key was the first one's "+
					"operation %q: the ledger answers it as landed and writes "+
					"nothing of it", ops[0])
			}
			for _, op := range ops {
				assertMintedWith(t, op, key)
			}
		})
	}
}

// pageKeys is the key every page write was handed, in order.
func pageKeys(r *rig) []string {
	r.kb.mu.Lock()
	defer r.kb.mu.Unlock()
	out := make([]string, 0, len(r.kb.actors))
	for _, actor := range r.kb.actors {
		out = append(out, actor.OpKey)
	}
	return out
}
