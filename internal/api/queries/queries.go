// Package queries is the read surface, answered ONCE for both transports.
//
// Every question the dashboard asks has exactly one implementation here, and
// both the REST route and the WebSocket query frame call it. That is not tidy
// packaging — it is the whole point. Two surfaces answering one question from
// two implementations is how they end up disagreeing with nobody noticing,
// which happens repeatedly once they diverge: a filter honoured on one path
// and ignored on the other, a limit clamped differently, a field present over
// HTTP and missing over the socket.
//
// A REFUSAL ON AUTHORITY travels as the decision that made it ([Refusal]), and
// each transport words it through the authority table's one mapping, as every
// other surface does. That is also why no question is decided by a verb that
// asks a recent proof or a person present ([DecidedBy]): the socket has no code
// for either refusal, so a question refused that way could only be told there
// as a permission nobody could grant.
package queries

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Errors a query surface reports precisely. Everything else a query returns is
// a failure whose reason reaches the log rather than the caller.
var (
	// ErrUnknown is a name nothing answers.
	ErrUnknown = errors.New("queries: unknown query")

	// ErrUnauthorized is a question asked by somebody this node KNOWS,
	// who does not carry the permission it declares, or whose stage may
	// not act — or, for a question naming somebody's record, whose
	// relation to that record does not reach it.
	// Every such refusal is a [*Refusal], which carries the rule's reason
	// and the grants that would have admitted the caller; this sentinel is
	// what a transport branches on.
	ErrUnauthorized = errors.New("queries: refused on authority")

	// ErrUnauthenticated is a question asked by nobody: a resolver ran and
	// found no credential.
	//
	// DISTINCT FROM [ErrUnauthorized]: the two ask a client for opposite
	// things ("present a credential" vs "the one you presented does not carry
	// this"). Folded together, a narrow reader outside their access is told to
	// get a new credential.
	//
	// UNREACHABLE THROUGH A WIRED SURFACE TODAY (the guard answers an anonymous
	// request first, and every socket authenticates at its handshake), but the
	// middleware in front of one transport is not this package's to keep. See
	// [Registry.AnswerWith].
	ErrUnauthenticated = errors.New("queries: no credential was presented")

	// ErrBadParams is a request this surface understood and refused.
	ErrBadParams = errors.New("queries: bad parameters")

	// ErrArtifactTrashed is a question about the CONTENT of an artifact in
	// the trash — its datastore — asked by somebody the artifact is
	// disclosed to.
	//
	// NOT [ErrNotFound], which is what the same question is answered with
	// for a caller the artifact is answered as absent to: the trash is the
	// artifact's state, which its owner's restore ends, so both transports
	// answer it as every artifacts route does (`409 artifact_trashed`). It
	// was not found here, so the one artifact a datastore's REST routes
	// said was binned, its questions said did not exist.
	ErrArtifactTrashed = errors.New("queries: the artifact is in the trash")
)

// RefusalDetail is a [ErrBadParams] refusal's own sentence, for the caller.
//
// A refusal is WRITTEN FOR the caller (it names the parameter to change and the
// values it accepts), so both transports carry it beside the `bad_params` code.
// What is taken out is the sentinel's own text, a Go package's name for the
// class, wherever it sits (`%w: days=91, …` or `…, not both: %w`). A refusal
// that also names a finer CLASS a caller tests for is built with [refuseAs],
// whose sentence carries no class text at all. Everything else is kept
// verbatim, a wrapped cause's explanation included.
func RefusalDetail(err error) string {
	if err == nil {
		return ""
	}
	var r *classedRefusal
	if errors.As(err, &r) {
		return r.sentence
	}
	text := err.Error()
	sentinel := ErrBadParams.Error()
	text = strings.ReplaceAll(text, sentinel+": ", "")
	text = strings.ReplaceAll(text, ": "+sentinel, "")
	text = strings.ReplaceAll(text, sentinel, "")
	return strings.TrimSpace(text)
}

// classedRefusal is an [ErrBadParams] refusal that is also a finer class, with
// a sentence written for the person who will read it.
//
// NOT A [Refusal], which is a refusal on AUTHORITY and answers
// [ErrUnauthorized]: this one is about the REQUEST, and its remedy is a
// different parameter rather than a different credential.
//
// THE CLASS IS IN THE CHAIN AND NOT IN THE SENTENCE. `%w: %w: …` put both
// sentinels' Go text in front of the explanation, and a class name is for
// [errors.Is], never for a reader.
type classedRefusal struct {
	sentence string
	class    error
}

func (r *classedRefusal) Error() string { return ErrBadParams.Error() + ": " + r.sentence }

// Unwrap makes a classed refusal both [ErrBadParams] and its class to
// [errors.Is].
func (r *classedRefusal) Unwrap() []error { return []error{ErrBadParams, r.class} }

// refuseAs is a refusal of the given class, its sentence formatted from args.
func refuseAs(class error, format string, args ...any) error {
	return &classedRefusal{sentence: fmt.Sprintf(format, args...), class: class}
}

// refusedBy is a refusal a DOMAIN decided about what a question asked, carried
// as [ErrBadParams] with the domain's own error beneath it for [errors.Is] and
// told in the domain's own sentence — the tracker's, the knowledge base's and
// the state log's read refusals through [builtin.ReadRefusalSentence], chat's,
// the artifacts' and a datastore's through each domain's Sentence.
//
// WRAPPED AS `%w: %w` IT WAS TOLD IN THE DOMAIN'S GO ERROR: [RefusalDetail]
// takes out this package's sentinel and nothing else, so a question refused
// by chat read "chatlet: invalid: `cursor`: …", by the tracker "worklet: this
// read's request cannot be run: since: statelog: …", and by a datastore
// "artifactdata: invalid: …" — package names and class words on the screen a
// person asked the question from.
func refusedBy(err error, sentence string) error {
	return &classedRefusal{sentence: sentence, class: err}
}

// Refusal is a question refused on AUTHORITY: a caller this node knows, whose
// grants (or relation to the record it named) do not reach what they asked. It
// is [ErrUnauthorized] to [errors.Is].
//
// A TYPE, NOT A WRAPPED SENTENCE: a surface renders the rule's reason and the
// admitting grants as values a client branches on; a sentence beside the rule
// is a second statement of it and goes stale. The decision is embedded whole,
// so both transports answer it through [authz.RefusalFields]; copied fields
// would drop a fresher-proof window. Grants is empty where no permission would
// admit the caller.
//
// BUILT ONLY BY [NewRefusal], which a walk in this package's tests holds.
type Refusal struct {
	// What is what was refused: the question, or — where the question was
	// admitted and the record it named was not — the verb asked of that
	// record, which is the more useful half to a reader of the log.
	What string

	authz.Decision
}

// NewRefusal is the refusal an [authz.Decision] makes of one question, and the
// one way a [Refusal] is built — exported so a transport's own tests can hand
// its mapping the refusals a question reaches it with.
//
// A DECISION THAT REFUSED NOTHING PANICS. An admission and a decision this node
// could not reach are both answered before a caller gets here — the second is
// [ErrUnavailable], never a 403 — so one arriving is a wiring mistake in this
// package, and rendered as a refusal it would tell a reader holding the
// authority that they lack it.
func NewRefusal(what string, d authz.Decision) *Refusal {
	if d.Allowed || d.Unknown() {
		panic(fmt.Sprintf("queries: %q was refused on a decision that refused "+
			"nothing (allowed %t, error %v)", what, d.Allowed, d.Err))
	}
	return &Refusal{What: what, Decision: d}
}

// Error renders the refusal from its own values, so the sentence in a log can
// never say something the reason and the grants do not.
func (r *Refusal) Error() string {
	var remedy string
	if len(r.Grants) > 0 {
		remedy = "; carrying " + words.Or(r.Grants) + " would admit it"
	}
	return fmt.Sprintf("queries: %q refused (%s)%s", r.What, r.Reason, remedy)
}

// Unwrap makes a refusal [ErrUnauthorized], which is what every transport
// already maps.
func (r *Refusal) Unwrap() error { return ErrUnauthorized }

// NotPermitted is a question refused by a GRANT ITS OWN DOMAIN DERIVES for the
// record it named — Gitlet's on a repository, from the chart
// (gitlet.GrantOf, ADR-0088) — rather than by the authority table, which
// admitted the question and decides nothing a repository's grant does. It is
// [ErrUnauthorized] to [errors.Is].
//
// A TYPE OF ITS OWN BESIDE [Refusal]: it has no authority-table reason and no
// admitting permission (a repository's grant is derived, never granted). It
// carries the domain's own sentence (who asked, which record, why they are shut
// out and who does read it); a REST route answers `403 not_permitted` with it,
// the word a refused push prints for the same refusal.
type NotPermitted struct {
	// Detail is the domain's sentence, written for the caller.
	Detail string
}

// Error renders the refusal for a log.
func (r *NotPermitted) Error() string { return "queries: not permitted: " + r.Detail }

// Unwrap makes it [ErrUnauthorized], a refusal of the caller.
func (r *NotPermitted) Unwrap() error { return ErrUnauthorized }

// NotFound is [ErrNotFound] saying WHICH absence it is: a Reason a client
// branches on and the sentence a person reads, for a surface whose absences
// are remedied differently — a code question's repository nobody declared
// (`not_declared`, declared in code.repos) and its company hosting no code
// (`not_hosted`, which declaring a repository changes nothing about until
// code.backend: gitlet is set). A REST route carries both, as `reason` and `detail`;
// the socket's frame carries codes and not prose, and answers `not_found`.
type NotFound struct {
	// Reason is which absence: a word a client switches on, never prose.
	Reason string

	// Detail is the sentence, written for the caller.
	Detail string
}

// Error renders the absence for a log.
func (e *NotFound) Error() string {
	return "queries: no such record (" + e.Reason + "): " + e.Detail
}

// Unwrap makes it [ErrNotFound].
func (e *NotFound) Unwrap() error { return ErrNotFound }

// Params are one query's arguments.
//
// It exists so a single answer function can be fed from both transports: a
// socket frame carries a JSON object, and a REST call carries a query string.
// Without it each question would need two readers, which is where a filter
// honoured on one path and ignored on the other comes from.
type Params struct{ values map[string]any }

// FromMap reads a socket frame's params.
func FromMap(m map[string]any) Params { return Params{values: m} }

// Values is the bag itself, for the one caller that needs to hand it on
// rather than read it: the tracker's own `view=`/`preset=` expansion merges a
// saved set UNDER the caller's own keys, which it can only do with the map.
//
// A COPY, because Params is passed by value and the caller must not be able to
// edit the request it was handed.
func (p Params) Values() map[string]any {
	out := make(map[string]any, len(p.values))
	maps.Copy(out, p.values)
	return out
}

// FromQuery reads a REST call's query string.
//
// Repeated keys take the FIRST value. A query string can carry a key twice and
// a JSON object cannot, so taking the last would make the two transports
// disagree about a request only one of them can even express.
//
// EXCEPT THE KEYS THAT REPEAT BY DESIGN ([worklet.RepeatedKeys]), which keep
// every value: `min_position` names one floor per log, and a JSON object
// expresses the same request as an array ([Params.List]), so here the two
// transports agree by keeping the whole list rather than by dropping all but
// one floor — which would serve the answer's other log from before the write
// that floor named.
func FromQuery(q url.Values) Params {
	values := make(map[string]any, len(q))
	for key, list := range q {
		switch {
		case len(list) == 0:
		case slices.Contains(worklet.RepeatedKeys, key):
			values[key] = slices.Clone(list)
		default:
			values[key] = list[0]
		}
	}
	return Params{values: values}
}

// With returns a copy carrying one more parameter.
//
// The PATH WINS over the query string, which is what a caller means: in
// GET /agents/{id} the id IS the route, so /agents/abc?id=xyz answers about
// abc. Silently answering about xyz would make a stray query parameter
// redirect a request to a different seat's memory.
//
// A copy rather than a mutation: Params is passed by value and a route that
// wrote into the map it was handed would be editing the caller's request.
func (p Params) With(key, value string) Params {
	values := make(map[string]any, len(p.values)+1)
	maps.Copy(values, p.values)
	values[key] = value
	return Params{values: values}
}

// String reads a string parameter, or "".
func (p Params) String(key string) string {
	switch v := p.values[key].(type) {
	case string:
		return v
	case float64:
		// A socket frame's JSON has one number type, so an id sent as a
		// number arrives here rather than as a string.
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

// List reads a parameter that may repeat ([worklet.RepeatedKeys]): every value
// a query string gave it ([FromQuery] keeps them), every string of a socket
// frame's JSON array, and a plain string as a list of one — by the grammar's
// own reading of a list ([worklet.ListOf]), so a socket frame and a query
// string name the same floors.
func (p Params) List(key string) ([]string, bool) { return worklet.ListOf(p.values[key]) }

// Int reads an integer parameter, falling back to def.
//
// A value present but unreadable falls back rather than failing. These are a
// dashboard's own filters — a limit, a page size — and refusing the whole
// question over one malformed filter would blank a screen to report a typo.
func (p Params) Int(key string, def int) int {
	switch v := p.values[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// WholeInt reads a parameter that must be a WHOLE NUMBER, and reports whether
// it is one: a socket frame's number with no fraction and inside int64, or a
// query string's integer as [strconv.ParseInt] reads it.
//
// FOR A VALUE THE ANSWER DEPENDS ON EXACTLY, which [Params.Int] is not: Int
// truncates a socket frame's `12.5` to 12, because a dashboard's limit or page
// size is a filter a typo should not blank a screen over. An offset into a
// text is not a filter — `after: 12.5` names no byte — and read as 12 it is a
// different request, answered as though it were the one asked. A caller
// refuses a false here by name. An absent key is false too; ask [Params.Has]
// first where absence means something of its own.
func (p Params) WholeInt(key string) (int64, bool) {
	switch v := p.values[key].(type) {
	case float64:
		// 2^63 is the first float64 past int64's range: MaxInt64 itself is
		// not a float64, and converting what rounds to it is undefined.
		if v != math.Trunc(v) || v < -(1<<63) || v >= 1<<63 {
			return 0, false
		}
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// Bool reads a boolean parameter, falling back to def.
func (p Params) Bool(key string, def bool) bool {
	switch v := p.values[key].(type) {
	case bool:
		return v
	case string:
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// Keys returns every parameter name, sorted.
//
// FOR THE ONE READER THAT CANNOT NAME ITS KEYS IN ADVANCE: the tracker's
// custom-field filters are `f.<slug>`, where the slug is whatever a company
// declared, so the only way to find them is to enumerate. Sorted, because the
// order reaches a parsed query and two callers passing the same filters in a
// different order must produce the same one.
func (p Params) Keys() []string {
	out := slices.Collect(maps.Keys(p.values))
	slices.Sort(out)
	return out
}

// Has reports whether a key was supplied at all, which is distinct from it
// being empty: a filter set to "" asks for rows with no value, and a filter
// absent asks for all of them.
func (p Params) Has(key string) bool {
	_, present := p.values[key]
	return present
}

// Clamp bounds a requested limit.
//
// Both ends matter. A zero or negative limit is a request that would return
// nothing, which is never what a dashboard means by leaving one off; and an
// unbounded one lets a single query pull the whole event log through memory on
// a process every tab shares.
func Clamp(requested, fallback, max int) int {
	if requested <= 0 {
		requested = fallback
	}
	if requested > max {
		return max
	}
	return requested
}

// Answer produces one question's payload.
type Answer func(ctx context.Context, p Params) (any, error)

// entry is one registered question.
type entry struct {
	answer Answer

	// posture is who may ask it.
	//
	// A POSTURE RATHER THAN A BOOL, which is what makes a narrow reader
	// possible at all. It was `operator bool` — is there a credential,
	// yes or no — and under that a token that could read the board could
	// also read the company document and every ${VAR} reference in it by
	// name.
	//
	// THE ZERO IS REFUSED AT REGISTRATION, not at the first request: an
	// unstated posture would make a question this build ships ungated,
	// looking exactly like one somebody decided to leave open.
	posture Posture
}

// postureKind is which of the three answers a [Posture] gives.
type postureKind uint8

const (
	// postureUnset is the zero, which no question may be registered with.
	postureUnset postureKind = iota
	postureEverybody
	postureRequires
	postureDecidedBy
)

// Posture is who may ask a question, declared where it is registered.
//
// THREE ANSWERS, by what a question discloses (ADR-0033):
//   - [Everybody]: the overview, for any principal whose stage may act; a
//     question mixing app rows with others (the company feed) drops rows the
//     caller may not see through [authz.Shows].
//   - [Requires]: admin or one app; answers nothing BUT that app's rows or the
//     company's own machinery.
//   - [DecidedBy]: any principal may ASK; the answer decides per record through
//     the authority table's verb (`get_work_item`).
//
// THE ZERO IS INVALID: [Registry.Register] panics on it.
type Posture struct {
	kind  postureKind
	grant iam.Grant
	verb  authz.Action
}

// Everybody is the overview's posture: any resolved principal whose stage may
// act.
func Everybody() Posture { return Posture{kind: postureEverybody} }

// Requires is the posture of a question only admin or one app opens. g must
// be [iam.GrantAdmin] or one of [iam.Apps]; anything else is refused at
// registration.
func Requires(g iam.Grant) Posture { return Posture{kind: postureRequires, grant: g} }

// DecidedBy is the posture of a question whose ANSWER decides, per record,
// through the authority table's verb a. Any resolved principal whose stage may
// act may ask; what they are shown is that verb's decision. a must be a verb
// the table has a row for, and one that asks no recent proof and no person
// present ([asksAPerson]).
func DecidedBy(a authz.Action) Posture { return Posture{kind: postureDecidedBy, verb: a} }

// Valid reports whether p is a posture a question may be registered with.
func (p Posture) Valid() bool {
	switch p.kind {
	case postureEverybody:
		return p.grant == "" && p.verb == ""
	case postureRequires:
		return p.verb == "" && (p.grant == iam.GrantAdmin || p.grant.IsApp())
	case postureDecidedBy:
		_, known := authz.ClassOf(p.verb)
		return p.grant == "" && known && asksAPerson(p.verb) == ""
	}
	return false
}

// asksAPerson is what verb a asks of the party making it beyond a permission —
// a recent proof on any of its arms, or a person present — or "" when it asks
// neither.
//
// A QUESTION MAY NOT BE DECIDED BY ONE, because every question is answered over
// the dashboard's socket as well as over REST, and the socket can carry
// neither refusal: its vocabulary has no `step_up_required` and no
// `token_refused` (the dashboard replays a step-up over REST alone), so a
// question refused either way there would read as a missing permission nobody
// could grant. The verbs an answer decides by inside itself, which no
// registration names, are held to the same rule by a walk in internal/authz.
func asksAPerson(a authz.Action) string {
	recency, _ := authz.RecencyOf(a)
	own, _ := authz.SelfRecencyOf(a)
	presence, _ := authz.PresenceOf(a)
	return demands(recency, own, presence)
}

// demands is [asksAPerson] over a row's three values: the window it asks of
// everybody, the one its self arm states, and which arms need a person present.
// Over values so every clause can be held on its own — no row in the table
// today needs a person present without also asking a proof.
func demands(recency, own iam.Recency, presence authz.Presence) string {
	switch {
	case recency.Demands():
		return "a proof inside " + string(recency)
	case own.Demands():
		return "a proof inside " + string(own) + " of its self arm"
	case presence != authz.PresenceOptional:
		return "a person present (" + string(presence) + ")"
	}
	return ""
}

// String names the posture as a person reads it: "everybody", the permission
// a [Requires] posture asks for, or "decided_by:<verb>".
func (p Posture) String() string {
	switch p.kind {
	case postureEverybody:
		return "everybody"
	case postureRequires:
		return string(p.grant)
	case postureDecidedBy:
		return "decided_by:" + string(p.verb)
	}
	return "unset"
}

// Grant is the permission a [Requires] posture asks for, or "" for any other.
func (p Posture) Grant() iam.Grant {
	if p.kind != postureRequires {
		return ""
	}
	return p.grant
}

// Verb is the authority table's verb a [DecidedBy] posture's answer decides
// through, or "" for any other.
func (p Posture) Verb() authz.Action {
	if p.kind != postureDecidedBy {
		return ""
	}
	return p.verb
}

// IsEverybody reports whether p is [Everybody].
func (p Posture) IsEverybody() bool { return p.kind == postureEverybody }

// Registry is the set of questions this process can answer.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]entry
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{entries: map[string]entry{}} }

// Register adds a question and DECLARES who may ask it.
//
// Only the registration knows what a question discloses; a posture elsewhere
// would be a second list to keep in step.
//
// A ZERO OR INVALID POSTURE PANICS at wiring time: a [Requires] of anything
// but admin or an app, a [DecidedBy] of a verb with no row in the authority
// table, or one asking a recent proof or a person present, which the socket
// cannot refuse in its own words ([asksAPerson]).
//
// Registering a name twice panics too: last-wins would hide two answers to one
// question.
func (r *Registry) Register(name string, posture Posture, answer Answer) {
	r.register(name, entry{answer: answer, posture: posture})
}

func (r *Registry) register(name string, e entry) {
	if name == "" {
		panic("queries: a question needs a name")
	}
	if e.answer == nil {
		panic(fmt.Sprintf("queries: %q registered with no answer", name))
	}
	if e.posture.kind == postureDecidedBy {
		if asks := asksAPerson(e.posture.verb); asks != "" {
			panic(fmt.Sprintf("queries: %q is decided by %s, which asks %s — "+
				"and a question is answered over the dashboard's socket too, "+
				"which cannot carry a step-up or tell a token's refusal from a "+
				"missing permission. Decide it by a verb that asks neither",
				name, e.posture.verb, asks))
		}
	}
	if !e.posture.Valid() {
		panic(fmt.Sprintf("queries: %q registered with the posture %s, which "+
			"is not one a question may declare — Everybody, Requires(admin or "+
			"one of %s), or DecidedBy(a verb internal/authz has a row for). A "+
			"question with no posture answers to any credential at all, and "+
			"looks exactly like one somebody decided to leave open",
			name, e.posture.debug(), words.Or(iam.Apps)))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.entries[name]; dup {
		panic(fmt.Sprintf("queries: %q registered twice", name))
	}
	r.entries[name] = e
}

// debug spells out every field of p, for the registration panic — where the
// posture is invalid and [Posture.String] would hide what is wrong with it.
func (p Posture) debug() string {
	return fmt.Sprintf("{kind %d, grant %q, verb %q}", p.kind, p.grant, p.verb)
}

// Names lists the registered questions, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := slices.Sorted(maps.Keys(r.entries))
	return out
}

// Answer runs one question.
//
// The signature is the socket's, because the socket is the surface with a
// name-to-question dispatch; a REST route knows its own question and calls the
// answer directly. Both reach the same function either way.
func (r *Registry) Answer(ctx context.Context, what string, params map[string]any) (any, error) {
	return r.AnswerWith(ctx, what, FromMap(params))
}

// AnswerWith runs one question against already-read parameters.
//
// The REST route reaches this one, because its parameters are a query string
// rather than a JSON object. Both entry points meet at the same answer with the
// same authorization check — the alternative is a route that reads its own
// params and forgets the operator check, which is the shape of the bug this
// package exists to make impossible.
func (r *Registry) AnswerWith(ctx context.Context, what string, p Params) (any, error) {
	r.mu.RLock()
	e, known := r.entries[what]
	r.mu.RUnlock()
	if !known {
		return nil, fmt.Errorf("%w: %q", ErrUnknown, what)
	}
	// THE GRANT THE CALLER CARRIES comes from the principal the guard
	// resolved; personal questions read [iam.From], the one place the
	// caller is established.
	//
	// THE REFUSAL IS THREE-VALUED, so the resolution is read: no grants is
	// the same zero value whether nobody presented a credential or this node
	// could not CHECK it. Reported as the first, an outage tells holders of
	// good credentials theirs is no good.
	//
	// A STAGE THAT MAY NOT ACT IS REFUSED HERE, whatever the posture (the
	// table's own first check): an [Everybody] question decides nothing else,
	// so a suspended person's session would keep reading the overview.
	switch principal, resolution := iam.From(ctx); {
	case resolution == iam.Unknown:
		return nil, unresolved(ctx, what)
	case resolution == iam.Anonymous:
		// NOBODY IS ASKING, which is a different refusal from a
		// principal who lacks the permission — see [ErrUnauthenticated].
		return nil, fmt.Errorf("%w: %q is asked as %s", ErrUnauthenticated,
			what, e.posture)
	case !principal.Stage.MayAct():
		return nil, NewRefusal(what, authz.Decision{Reason: authz.ReasonStage})
	// A PRINCIPAL ACTING THROUGH THE ASSISTANT asks exactly what its
	// person may ask, at every posture (ADR-0052's parity): a question is
	// a read, and no question answers a credential or a secret in clear,
	// which is all the authority table's human-only list keeps from the
	// assistant among reads. A [DecidedBy] question decides each record
	// through the authority table, whose assistant column then applies.
	case e.posture.kind == postureRequires && !principal.Can(e.posture.grant):
		return nil, NewRefusal(what, authz.Decision{Reason: authz.ReasonNoGrant,
			Grants: []iam.Grant{e.posture.grant}})
	}
	data, err := e.answer(ctx, p)
	return data, ClassifyFailure(err)
}

// unresolved is the refusal for a question asked on a context whose principal
// nobody could establish. Neither cause is [ErrUnauthorized]: the caller's
// credential is not what is wrong.
//
//   - THE IDENTITY ESTATE COULD NOT BE READ (a resolver ran and left its
//     reason): "not yet", the same [ErrUnavailable] a state-log refusal gets,
//     with a Retry-After.
//   - NO RESOLVER EVER TOUCHED THIS CONTEXT ([iam.ErrUnresolved]): a wiring bug
//     (a route outside the guard, an unthreaded context) that waiting never
//     clears, so a plain failure to report. A Retry-After would send a client
//     round a loop only a routing edit ends.
func unresolved(ctx context.Context, what string) error {
	why := iam.Reason(ctx)
	if errors.Is(why, iam.ErrUnresolved) {
		return fmt.Errorf("query %q reached the registry on a context no "+
			"resolver has answered for: the route is not wired through the "+
			"guard, or the request context was not threaded: %w", what, why)
	}
	return fmt.Errorf("%w: %q cannot be authorized while this node cannot "+
		"read identity: %w", ErrUnavailable, what, why)
}

// ClassifyFailure sorts a failed answer into this registry's vocabulary: a read
// THIS NODE could not serve ([unavailableHere]) becomes [ErrUnavailable], a
// position named on another log becomes [ErrBadParams], the rest is left alone.
//
// AT THE REGISTRY, ONCE: an answer that skipped it rendered a blip as a 500.
// A refusal about the REQUEST is never reclassified: "come back" would loop.
// Only the read can raise [statelog.ErrForeignPosition] (a `min_position`,
// `cursor` or `since`); answered `unavailable` it would send a client to a
// node that refuses it alike.
//
// EXPORTED for Gitlet's verify routes (internal/api/code.go), which answer
// outside the registry and share this list of what clears by waiting.
func ClassifyFailure(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrUnavailable),
		errors.Is(err, ErrBadParams),
		errors.Is(err, ErrNotFound),
		errors.Is(err, ErrArtifactTrashed),
		errors.Is(err, ErrUnauthenticated),
		errors.Is(err, ErrUnauthorized):
		return err
	case errors.Is(err, statelog.ErrForeignPosition):
		return refusedBy(err, builtin.ReadRefusalSentence(err))
	}
	if unavailableHere(err) {
		// WRAPPED, NOT REPLACED, so the refusal's own code, detail and
		// derived hint survive for [statelog.RetryAfter], for the
		// transport that renders them, and for the log.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	// A SEAT'S HOLDER THAT COULD NOT ANSWER YET — its lease unreadable, its
	// node silent, or the seat still arriving on it — clears by waiting.
	if errors.Is(err, memread.ErrUnavailable) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}

// unavailableHere reports a read THIS NODE could not serve — which is never a
// fault of the request, and never a fault of the server either.
//
// IT IS THE TOOLS' OWN RULE ([builtin.Condition], which lists each arm), not a
// list of this registry's: a second list drifted. EVERY state-log refusal
// ([statelog.ErrUnavailable]) counts, whether or not waiting clears it: none is
// a FAULT, and a 500 would hide the remedy the refusal names. Whether to come
// back is [statelog.RetryAfter], ZERO where waiting cannot clear it, so no
// Retry-After and no loop.
//
// ASKED BY THE ANSWERS too: handed one of these they would tell a caller to
// change a question nothing was wrong with.
func unavailableHere(err error) bool {
	_, ok := builtin.Condition(err)
	return ok
}

// PostureOf reports who a question is registered as being asked by.
//
// THE DECLARED POSTURE, readable without running the answer, for the gate
// that asserts it. Nothing enforces with it: [Registry.AnswerWith] decides for
// both transports, and a second enforcement point must not exist.
//
// An unregistered name answers the zero posture, which is invalid, so a caller
// asserting about one is told it is not a question.
func (r *Registry) PostureOf(what string) Posture {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[what].posture
}
