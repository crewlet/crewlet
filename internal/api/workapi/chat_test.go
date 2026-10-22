package workapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ---- chat -------------------------------------------------------------- //

// destroyed is one destruction the rooms were asked for.
type destroyed struct {
	verb     string
	actor    chatlet.Actor
	opID     string
	channel  string
	messages []string
	reason   string
	cutoff   time.Time
	days     *int
}

// rooms is chat's writer as the human write surface reaches it: it records
// every destruction and answers what a case tells it to — applied at a
// position unless an outcome or an error is set.
type rooms struct {
	mu      sync.Mutex
	made    []destroyed
	outcome statelog.Result
	err     error
}

func (r *rooms) record(d destroyed) (chatlet.Written, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.made = append(r.made, d)
	if r.err != nil {
		return chatlet.Written{}, r.err
	}
	out := r.outcome
	if out.Outcome == "" {
		out = statelog.Result{Outcome: statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "CREWLET_CHAT_LOG", Generation: 1, Seq: 40}}
	}
	return chatlet.Written{ChangeID: d.opID, Outcome: out}, nil
}

func (r *rooms) Erase(_ context.Context, actor chatlet.Actor, opID, channelID string,
	messageIDs []string, reason string) (chatlet.Written, error) {
	return r.record(destroyed{verb: "erase", actor: actor, opID: opID,
		channel: channelID, messages: messageIDs, reason: reason})
}

func (r *rooms) Prune(_ context.Context, actor chatlet.Actor, opID, channelID string,
	cutoff time.Time) (chatlet.Written, error) {
	return r.record(destroyed{verb: "prune", actor: actor, opID: opID,
		channel: channelID, cutoff: cutoff})
}

func (r *rooms) SetRetention(_ context.Context, actor chatlet.Actor, channelID string,
	days *int) (chatlet.Written, error) {
	return r.record(destroyed{verb: "retention", actor: actor, opID: actor.OpKey,
		channel: channelID, days: days})
}

// all is every destruction, in order.
func (r *rooms) all() []destroyed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]destroyed(nil), r.made...)
}

// theRoom is the room these cases destroy in.
const theRoom = "7c2a8b64-0d4e-4f9a-9a3b-2f6c1d8e5b70"

// eraseOf is an erase request: its path with ?confirm= and its body.
func eraseOf(confirm string, ids []string, reason string) (string, map[string]any) {
	body := map[string]any{"message_ids": ids}
	if reason != "" {
		body["reason"] = reason
	}
	path := "/chatlet/channels/" + theRoom + "/erase"
	if confirm != "" {
		path += "?confirm=" + confirm
	}
	return path, body
}

// pruneAt is a prune request's path, its cutoff and confirmation as given.
func pruneAt(cutoff, confirm string) string {
	q := url.Values{}
	if cutoff != "" {
		q.Set("cutoff", cutoff)
	}
	if confirm != "" {
		q.Set("confirm", confirm)
	}
	return "/chatlet/channels/" + theRoom + "/prune?" + q.Encode()
}

// retentionPath is the room's horizon route.
const retentionPath = "/chatlet/channels/" + theRoom + "/retention"

// retentionAt is the horizon route with its confirmation, "" for none.
func retentionAt(confirm string) string {
	if confirm == "" {
		return retentionPath
	}
	return retentionPath + "?confirm=" + url.QueryEscape(confirm)
}

// A CHAT DESTRUCTION IS A PURGE'S DECISION: admin's, a person's or a
// credential's — never an agent's, which could erase the evidence of what it
// said — and a recent proof's, on every one of the three verbs. A colleague
// holding every app is refused naming the one grant that would admit them, an
// administrator whose proof is stale is told to prove who they are again, and
// an agent seat holding admin is refused as a seat. Each refusal is recorded,
// and none reaches the store.
//
// AND AN ADMINISTRATOR'S TOKEN ACTING AS ITSELF IS ADMITTED, unlike every chat
// verb a tool serves: none of the three is said in a room, so there is no seat
// it needs to speak as.
func TestAChatDestructionIsAdminsAndARecentProofs(t *testing.T) {
	t.Parallel()
	staleAdmin := person("cy", iam.GrantAdmin)
	agent := iam.Principal{ID: uuid.New(), Login: "sre", Seat: "sre",
		Kind: iam.KindSeat, Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour)}
	token := iam.Principal{ID: uuid.New(), Login: "token:ops", Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour)}
	cutoff := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	erasePath, eraseBody := eraseOf("2", []string{"m-1", "m-2"}, "a credential was pasted")
	requests := map[string]struct {
		method, path string
		body         any
		verb         authz.Action
	}{
		"an erase":    {http.MethodPost, erasePath, eraseBody, authz.ActionChatErase},
		"a prune":     {http.MethodPost, pruneAt(cutoff, cutoff), nil, authz.ActionChatPrune},
		"a retention": {http.MethodPut, retentionAt("30"), map[string]any{"retention_days": 30}, authz.ActionChatRetention},
	}
	for who, tc := range map[string]struct {
		principal iam.Principal
		status    int
		code      httpjson.Code
		reason    authz.Reason
	}{
		"an administrator with a fresh proof": {admin("ana"), http.StatusOK, "", ""},
		"an administrator's token":            {token, http.StatusOK, "", ""},
		"a colleague holding every app": {colleague("bo"), http.StatusForbidden,
			httpjson.CodeUnauthorized, authz.ReasonNoGrant},
		"an administrator whose proof is stale": {staleAdmin, http.StatusForbidden,
			httpjson.CodeStepUpRequired, authz.ReasonStepUp},
		"an agent seat holding admin": {agent, http.StatusForbidden,
			httpjson.CodeUnauthorized, authz.ReasonSeatRefused},
	} {
		for what, req := range requests {
			t.Run(who+"/"+what, func(t *testing.T) {
				t.Parallel()
				r := newRig(t, chart{})
				got := r.do(as(tc.principal), req.method, req.path, req.body)
				if got.status != tc.status {
					t.Fatalf("answered %d %v, want %d", got.status, got.body, tc.status)
				}
				acted := r.audit.acted()
				if len(acted) != 1 || acted[0].Tool != string(req.verb) {
					t.Fatalf("recorded %+v, want one record of %s", acted, req.verb)
				}
				if tc.status == http.StatusOK {
					if len(r.rooms.all()) != 1 || acted[0].Outcome != types.AuditApplied {
						t.Errorf("an admitted destruction reached the store %d times "+
							"and was recorded %s", len(r.rooms.all()), acted[0].Outcome)
					}
					return
				}
				if got.body["error"] != string(tc.code) ||
					(tc.reason != authz.ReasonStepUp && got.body["reason"] != string(tc.reason)) {
					t.Errorf("answered %v, want %s with reason %s", got.body, tc.code, tc.reason)
				}
				if tc.reason == authz.ReasonNoGrant {
					if grants, _ := got.body["grants"].([]any); len(grants) != 1 ||
						grants[0] != string(iam.GrantAdmin) {
						t.Errorf("the refusal names %v, want admin alone", got.body["grants"])
					}
				}
				if made := r.rooms.all(); len(made) != 0 {
					t.Errorf("a refused destruction reached the store: %+v", made)
				}
				if acted[0].Outcome != types.AuditRefused {
					t.Errorf("the refusal was recorded %s", acted[0].Outcome)
				}
			})
		}
	}
}

// A DESTRUCTION IS MADE AS ITS PRINCIPAL, as every write here is: a person as
// the seat the identity directory binds them to, kind human, and a credential
// acting as itself under its own whole login, kind operator — the one chat
// write a credential makes in its own name, because none of the three is said
// in the room.
func TestAChatDestructionIsMadeAsItsPrincipal(t *testing.T) {
	t.Parallel()
	token := iam.Principal{ID: uuid.New(), Login: "token:ops", Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour)}
	for who, tc := range map[string]struct {
		principal iam.Principal
		name      string
		kind      chatlet.AuthorKind
	}{
		"a person":                 {admin("ana"), "ana", chatlet.AuthorHuman},
		"an administrator's token": {token, "token:ops", chatlet.AuthorOperator},
	} {
		t.Run(who, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, chart{})
			path, body := eraseOf("1", []string{"m-1"}, "pasted a key")
			if got := r.do(as(tc.principal), http.MethodPost, path, body); got.status != http.StatusOK {
				t.Fatalf("the erase answered %d %v", got.status, got.body)
			}
			made := r.rooms.all()
			if len(made) != 1 || made[0].actor.Name() != tc.name || made[0].actor.Kind != tc.kind {
				t.Fatalf("the erase was made as %+v, want %s (%s)", made, tc.name, tc.kind)
			}
		})
	}
}

// A DESTRUCTION IS BOUND TO WHAT IT DESTROYS, under the request's key: the
// same erase named in another order is the same operation, which is what a
// retry is, and the same key sent with other messages, another reason or a
// later cutoff is ANOTHER operation, which lands as asked. Bound to the key
// alone, a compliance erase of the messages a person meant to name second
// would have been answered `applied` from the first record while nothing it
// named was destroyed.
func TestAChatDestructionIsBoundToWhatItDestroys(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	key := statelog.NewOpID(time.Now(), "")
	erase := func(ids []string, reason string) string {
		t.Helper()
		path, body := eraseOf(fmt.Sprint(len(chatlet.EraseSet(ids))), ids, reason)
		got := r.do(as(admin("ana")), http.MethodPost, path, body, opkey.Header, key)
		if got.status != http.StatusOK {
			t.Fatalf("the erase answered %d %v", got.status, got.body)
		}
		made := r.rooms.all()
		return made[len(made)-1].opID
	}
	first := erase([]string{"m-1", "m-2"}, "a credential was pasted")
	if again := erase([]string{"m-2", "m-1", "m-2"}, "a credential was pasted"); again != first {
		t.Errorf("one erase named in another order derived %s, then %s", first, again)
	}
	if other := erase([]string{"m-1", "m-3"}, "a credential was pasted"); other == first {
		t.Errorf("an erase of other messages under the same key derived the first "+
			"one's operation %s", first)
	}
	if other := erase([]string{"m-1", "m-2"}, "a second reason"); other == first {
		t.Errorf("an erase for another reason under the same key derived the first "+
			"one's operation %s", first)
	}
	made := r.rooms.all()
	if strings.Join(made[1].messages, ",") != "m-1,m-2" {
		t.Errorf("the store was handed %v, want the set sorted and deduplicated",
			made[1].messages)
	}

	prune := func(cutoff string) string {
		t.Helper()
		got := r.do(as(admin("ana")), http.MethodPost, pruneAt(cutoff, cutoff), nil,
			opkey.Header, key)
		if got.status != http.StatusOK {
			t.Fatalf("the prune answered %d %v", got.status, got.body)
		}
		all := r.rooms.all()
		return all[len(all)-1].opID
	}
	at := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	utc := prune(at.UTC().Format(time.RFC3339))
	elsewhere := prune(at.In(time.FixedZone("UTC+3", 3*3600)).Format(time.RFC3339))
	if elsewhere != utc {
		t.Errorf("one cutoff written in two zones derived %s and %s", utc, elsewhere)
	}
	if later := prune(at.Add(time.Hour).UTC().Format(time.RFC3339)); later == utc {
		t.Errorf("a later cutoff under the same key derived the first prune's "+
			"operation %s", utc)
	}

	// A ROOM'S HORIZON IS BOUND BY THE STORE, which derives its id from the
	// key and what is asked: the key reaches it on the actor.
	if got := r.do(as(admin("ana")), http.MethodPut, retentionAt("7"),
		map[string]any{"retention_days": 7}, opkey.Header, key); got.status != http.StatusOK {
		t.Fatalf("the retention answered %d %v", got.status, got.body)
	}
	all := r.rooms.all()
	if set := all[len(all)-1]; set.actor.OpKey == "" || set.days == nil || *set.days != 7 {
		t.Errorf("the retention reached the store as %+v, want the request's key "+
			"and 7 days", set)
	}
}

// A DESTRUCTION CONFIRMS WHAT IT DESTROYS, with nothing read from the room: an
// erase repeats how many messages it names, a prune repeats its cutoff, a
// horizon repeats its days or `company`, and a request that cannot say is
// refused before anything is written — a missing
// confirmation or cutoff the query's fault, a body that is not the verb's own
// the body's, and a confirmation that names something else `invalid`.
func TestAChatDestructionConfirmsWhatItDestroys(t *testing.T) {
	t.Parallel()
	cutoff := time.Now().Add(-48 * time.Hour).UTC()
	for name, tc := range map[string]struct {
		method, path string
		body         any
		status       int
		code         httpjson.Code
	}{
		"an erase with no confirmation": {http.MethodPost,
			"/chatlet/channels/" + theRoom + "/erase",
			map[string]any{"message_ids": []string{"m-1"}, "reason": "x"},
			http.StatusBadRequest, httpjson.CodeInvalidQuery},
		"an erase confirming another count": {http.MethodPost,
			"/chatlet/channels/" + theRoom + "/erase?confirm=3",
			map[string]any{"message_ids": []string{"m-1", "m-1", "m-2"}, "reason": "x"},
			http.StatusUnprocessableEntity, httpjson.CodeInvalid},
		"an erase with no reason": {http.MethodPost,
			"/chatlet/channels/" + theRoom + "/erase?confirm=1",
			map[string]any{"message_ids": []string{"m-1"}},
			http.StatusBadRequest, httpjson.CodeInvalidBody},
		"an erase naming no list": {http.MethodPost,
			"/chatlet/channels/" + theRoom + "/erase?confirm=1",
			map[string]any{"message_ids": "m-1", "reason": "x"},
			http.StatusBadRequest, httpjson.CodeInvalidBody},
		"an erase naming another verb's argument": {http.MethodPost,
			"/chatlet/channels/" + theRoom + "/erase?confirm=1",
			map[string]any{"message_ids": []string{"m-1"}, "reason": "x", "cutoff": "y"},
			http.StatusBadRequest, httpjson.CodeInvalidBody},
		"a prune with no cutoff": {http.MethodPost,
			pruneAt("", cutoff.Format(time.RFC3339)), nil,
			http.StatusBadRequest, httpjson.CodeInvalidQuery},
		"a prune with no confirmation": {http.MethodPost,
			pruneAt(cutoff.Format(time.RFC3339), ""), nil,
			http.StatusBadRequest, httpjson.CodeInvalidQuery},
		"a prune at no instant": {http.MethodPost, pruneAt("yesterday", "yesterday"), nil,
			http.StatusBadRequest, httpjson.CodeInvalidQuery},
		"a prune confirming another instant": {http.MethodPost,
			pruneAt(cutoff.Format(time.RFC3339), cutoff.Add(time.Second).Format(time.RFC3339)),
			nil, http.StatusUnprocessableEntity, httpjson.CodeInvalid},
		"a retention stating nothing": {http.MethodPut, retentionAt("30"), map[string]any{},
			http.StatusBadRequest, httpjson.CodeInvalidBody},
		"a retention of part of a day": {http.MethodPut, retentionAt("1"),
			map[string]any{"retention_days": 1.5},
			http.StatusBadRequest, httpjson.CodeInvalidBody},
		"a retention with no confirmation": {http.MethodPut, retentionPath,
			map[string]any{"retention_days": 30},
			http.StatusBadRequest, httpjson.CodeInvalidQuery},
		"a retention confirming another horizon": {http.MethodPut, retentionAt("31"),
			map[string]any{"retention_days": 30},
			http.StatusUnprocessableEntity, httpjson.CodeInvalid},
		"the company's horizon confirmed as a number": {http.MethodPut, retentionAt("0"),
			`{"retention_days": null}`,
			http.StatusUnprocessableEntity, httpjson.CodeInvalid},
		"for ever confirmed as the company's": {http.MethodPut, retentionAt("company"),
			map[string]any{"retention_days": 0},
			http.StatusUnprocessableEntity, httpjson.CodeInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, chart{})
			got := r.do(as(admin("ana")), tc.method, tc.path, tc.body)
			if got.status != tc.status || got.body["error"] != string(tc.code) {
				t.Fatalf("answered %d %v, want %d %s", got.status, got.body,
					tc.status, tc.code)
			}
			// WRITTEN FOR THE PERSON WHO SENT IT: these routes are a
			// person's alone, so what each refusal says is short sentences
			// saying what to change, with no dash-joined rationale.
			if detail := got.detail(); detail == "" || strings.Contains(detail, "—") ||
				!strings.HasSuffix(detail, ".") {
				t.Errorf("the refusal reads %q, want short sentences a person "+
					"reads, with no em dash", detail)
			}
			if made := r.rooms.all(); len(made) != 0 {
				t.Errorf("a request that confirmed nothing destroyed %+v", made)
			}
		})
	}

	// AND WHAT DOES CONFIRM IS SERVED: a count of the set once duplicates
	// are folded, a cutoff confirmed in another zone, a horizon taken back
	// with null.
	r := newRig(t, chart{})
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/chatlet/channels/" + theRoom + "/erase?confirm=2",
			map[string]any{"message_ids": []string{"m-1", "m-1", "m-2"}, "reason": "x"}},
		{http.MethodPost, pruneAt(cutoff.Format(time.RFC3339),
			cutoff.In(time.FixedZone("UTC-5", -5*3600)).Format(time.RFC3339)), nil},
		{http.MethodPut, retentionAt("company"), `{"retention_days": null}`},
		{http.MethodPut, retentionAt("0"), map[string]any{"retention_days": 0}},
	} {
		if got := r.do(as(admin("ana")), req.method, req.path, req.body); got.status != http.StatusOK {
			t.Errorf("%s %s answered %d %v, want it served", req.method, req.path,
				got.status, got.body)
		}
	}
	made := r.rooms.all()
	if len(made) != 4 || made[2].days != nil || made[3].days == nil || *made[3].days != 0 {
		t.Errorf("the store was asked %+v, want an erase, a prune, the company's "+
			"horizon and for ever", made)
	}
}

// A DESTRUCTION ANSWERS BY ITS OUTCOME, in the surface's own codes: applied
// 200, pending 202 with the position to read at, and a write nobody can vouch
// for 503 `outcome: unknown` under the request's key — never the 200 a
// destruction that may not have happened would be read as done from. A
// refusal the store decided is told in chat's own words.
func TestAChatDestructionAnswersByItsOutcome(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		outcome  statelog.Result
		err      error
		status   int
		code     httpjson.Code
		answered string
	}{
		"applied": {status: http.StatusOK, answered: "applied"},
		"pending": {outcome: statelog.Result{Outcome: statelog.OutcomePending,
			Position: statelog.Position{Stream: "CREWLET_CHAT_LOG", Generation: 1, Seq: 9}},
			status: http.StatusAccepted, answered: "pending"},
		"unknown": {outcome: statelog.Result{Outcome: statelog.OutcomeUnknown},
			status: http.StatusServiceUnavailable, answered: "unknown"},
		"asked of a node that never answered": {
			err:    fmt.Errorf("%w (chat.erase, asked of data-b)", estate.ErrOutcomeUnknown),
			status: http.StatusServiceUnavailable, answered: "unknown"},
		"a room the store does not hold": {err: chatlet.ErrNotFound,
			status: http.StatusNotFound, code: httpjson.CodeNotFound},
		"a reason past the cap": {err: &chatlet.InvalidError{Field: "reason",
			Why: "an erase's reason is 700 bytes against a 600 cap"},
			status: http.StatusUnprocessableEntity, code: httpjson.CodeInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, chart{})
			r.rooms.outcome, r.rooms.err = tc.outcome, tc.err
			key := statelog.NewOpID(time.Now(), "")
			path, body := eraseOf("1", []string{"m-1"}, "pasted a key")
			got := r.do(as(admin("ana")), http.MethodPost, path, body, opkey.Header, key)
			if got.status != tc.status {
				t.Fatalf("answered %d %v, want %d", got.status, got.body, tc.status)
			}
			if tc.answered != "" && (got.body["outcome"] != tc.answered || got.body["op_id"] == nil) {
				t.Errorf("answered %v, want outcome %s under an op_id", got.body, tc.answered)
			}
			if tc.code != "" && got.body["error"] != string(tc.code) {
				t.Errorf("answered %v, want %s", got.body, tc.code)
			}
			if tc.status == http.StatusAccepted && got.body["position"] != "CREWLET_CHAT_LOG@1:9" {
				t.Errorf("a pending erase answered %v, want the position to read at", got.body)
			}
			// A REFUSAL NOBODY HAS WORDED FOR A PERSON CARRIES NO DETAIL
			// (ADR-0060): its code's own message stands alone, and chat's
			// sentence, written for a model, is nowhere in the answer.
			if tc.code == httpjson.CodeInvalid && (got.detail() != "" ||
				strings.Contains(fmt.Sprint(got.body), "600 cap")) {
				t.Errorf("the refusal reads %v, want its code's own message alone", got.body)
			}
		})
	}
}

// A NODE WHOSE CHAT IS NOT THE ENGINE'S SERVES NONE OF THE THREE, in the mux's
// own bytes, as a vendor tracker's node serves no /worklet route: a company on
// Slack has no Chatlet room to destroy anything in, and no wait brings one.
func TestChatsRoutesAreAbsentWhereChatIsNotTheEngines(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	halves := r.halves()
	halves.ChatStore = nil
	svc, err := workapi.New(workapi.Options{
		Chart: chart{}, Halves: serving(halves),
		Operator: r.operator(chart{}, serving(halves)), Audit: r.audit,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.mux = http.NewServeMux()
	if err := svc.Routes(r.mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	path, body := eraseOf("1", []string{"m-1"}, "pasted a key")
	got := r.do(as(admin("ana")), http.MethodPost, path, body)
	if got.status != http.StatusNotFound || got.body["error"] != string(httpjson.CodeNoRoute) {
		t.Errorf("an erase on a node with no Chatlet answered %d %v, want "+
			"404 %s", got.status, got.body, httpjson.CodeNoRoute)
	}
	if made := r.rooms.all(); len(made) != 0 {
		t.Errorf("a node with no Chatlet destroyed %+v", made)
	}
	if acted := r.audit.acted(); len(acted) != 0 {
		t.Errorf("an absent route recorded %+v", acted)
	}
}

// A DESTRUCTION'S BODY THAT DOES NOT DECODE IS TOLD IN WORDS
// ([plainjson.Unreadable]): the detail ended in encoding/json's own sentence
// ("invalid character …", "json: cannot unmarshal array into Go value of type
// map[string]interface {}") after the verb's shape. Mutation: answer
// err.Error() again and every row goes red.
func TestADestructionsBodyThatDoesNotDecodeIsToldInWords(t *testing.T) {
	t.Parallel()
	const shape = " Send one JSON object of this route's arguments."
	for _, tc := range []struct{ body, says string }{
		{`{"message_ids":`, "The body isn't valid JSON: it ends too soon." + shape},
		{`["m-1"]`, "The body is a list where an object belongs." + shape},
		{`{} {}`, "The body has more after its JSON value, which is all it may hold." + shape},
	} {
		r := newRig(t, chart{})
		got := r.do(as(admin("ana")), http.MethodPost,
			"/chatlet/channels/"+theRoom+"/erase?confirm=1", tc.body)
		if got.status != http.StatusBadRequest || got.detail() != tc.says {
			t.Errorf("%s answered %d %v, want 400 saying %q", tc.body, got.status,
				got.body, tc.says)
		}
	}
}
