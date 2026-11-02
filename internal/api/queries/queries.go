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
	// DISTINCT FROM [ErrUnauthorized], because the two ask a client for
	// opposite things — "present a credential" and "the one you presented
	// does not carry this" — and a narrow reader meets the second the
	// moment they open a screen outside their access, which is the
	// ordinary case rather than the exceptional one. Folded together, the
	// surface tells that reader to go and get a new credential.
	//
	// UNREACHABLE THROUGH A WIRED SURFACE TODAY, because the guard answers
	// an anonymous request before this function is called and every socket
	// authenticates at its handshake. It is stated anyway: this package is
	// reached by two transports and the middleware in front of one of them
	// is not this package's to keep, so a refusal that is correct only
	// because something upstream happens to answer first is one edit from
	// being wrong. See [Registry.AnswerWith].
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
// A refusal here is WRITTEN FOR the caller — it names the parameter to change
// and the values it accepts — so both transports carry it beside the
// `bad_params` code rather than only logging it. What is taken out is the
// sentinel's own text, which is a Go package's name for the class and says
// nothing a reader can act on; the refusals wrap it at the front
// (`%w: days=91, …`) or at the end (`…, not both: %w`), and both are removed
// wherever they sit. A refusal that also names a finer CLASS a caller tests
// for — a spend window too long, or starting before the history — is built
// with [refuseAs], whose sentence carries no class text at all: the Spend
// screen used to show `tokens: the window reaches past the spend history:`
// in front of every such refusal, twice over with the question's own name.
// Everything else is kept verbatim, a wrapped cause's explanation included.
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
// grants — or, for a personal question, whose relation to the record it named
// — do not reach what they asked. It is [ErrUnauthorized] to [errors.Is].
//
// A TYPE RATHER THAN A WRAPPED SENTENCE, because a surface renders what it
// carries: the rule's own reason and the grants that would have admitted the
// caller, as values a client branches on. A sentence written beside the rule
// — "needs the lead relation or admin" — is a second statement of the rule,
// and it is the copy that goes stale the day the rule's admin path moves.
//
// THE DECISION WHOLE, embedded rather than copied into fields of its own, so
// both transports answer it through the authority table's one mapping
// ([authz.RefusalFields]) exactly as every other surface answers a refusal
// the table made. It carried the reason and the grants alone, and each
// transport then spelled `unauthorized` for itself: a refusal that needed a
// fresher proof would have reached a reader as a missing permission, with
// the window it names dropped on the way. Its Reason is the rule that decided
// — [authz.ReasonNoGrant] for a question's declared permission,
// [authz.ReasonStage] for a caller whose stage may not act, and whatever
// [authz.Decide] concluded for a record a question named — and its Grants the
// permissions any one of which would have admitted the caller, empty where no
// permission would.
//
// BUILT ONLY BY [NewRefusal], which a walk in this package's tests holds, so no
// refusal reaches a transport carrying a decision that refused nothing.
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
// record it named — the code host's on a repository, from the chart
// (code.GrantOf, ADR-0081) — rather than by the authority table, which
// admitted the question and decides nothing a repository's grant does. It is
// [ErrUnauthorized] to [errors.Is].
//
// A TYPE OF ITS OWN BESIDE [Refusal], because it carries none of what a
// Refusal does: no authority-table reason, and no permission that would admit
// the caller — a repository's grant is derived, never granted. What it
// carries is the domain's own sentence (who asked, which record, why they are
// shut out and who does read it), which is the whole of what the caller can
// act on; a REST route answers it `403 not_permitted` with that sentence, the
// word a refused push prints for the same refusal.
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
// code.native is set). A REST route carries both, as `reason` and `detail`;
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
// THREE ANSWERS, and each is a different statement about what the question
// discloses (ADR-0033):
//
//   - [Everybody]: the overview — the org chart, the agents' status, spend,
//     schedules, the tools — which every principal the guard resolves and
//     whose stage may act reads, a service account and a token included. A
//     question that discloses a row of an app beside rows of none (the
//     company feed, a page's links back to the work) is Everybody and drops
//     the rows its caller may not see, through [authz.Shows].
//   - [Requires]: admin, or one app. A question that answers nothing BUT one
//     app's rows — the board, the pages — or nothing but the company's own
//     machinery — the document, the audit trail, the deployment.
//   - [DecidedBy]: any resolved principal may ASK, and the answer decides per
//     record through the authority table's verb — a seat's trail
//     (`audit.seat.read`, its lead or admin), a work item somebody may have
//     been asked a question on (`get_work_item`). Stated here so the gate
//     that walks the registry can tell a question that decides inside its
//     answer from one that forgot to decide at all.
//
// THE ZERO IS INVALID and [Registry.Register] panics on it: a question with no
// posture answers to any credential at all, and looks exactly like one
// somebody decided to leave open.
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
// # Why the posture is at the registration
//
// It is the only place that knows. A question's authority is a property of
// what it discloses — `config` serves the whole company document, `events`
// serves LLM transcripts, `work_items` serves the board — and none of that is
// visible from the name, the route or the caller. Stated anywhere else it
// would be a second list somebody has to keep in step with this one.
//
// A ZERO OR INVALID POSTURE PANICS, at wiring time, on every build — a
// [Requires] of anything but admin or an app, and a [DecidedBy] of a verb the
// authority table has no row for, included. A question registered without
// one would answer to any credential at all, which is the exact shape of an
// ungated surface that looks deliberate — and the panic is at start-up rather
// than at the first request because a company finding out on the first
// request has already served it. So does a [DecidedBy] of a verb that asks a
// recent proof or a person present, which the socket a question is also
// answered over cannot refuse in its own words ([asksAPerson]).
//
// Registering a name twice panics too, rather than silently taking one of
// them: two answers to one question is exactly the divergence this package
// exists to prevent, and a wiring mistake that resolved to whichever ran last
// would be invisible.
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
	// THE GRANT THE CALLER ACTUALLY CARRIES, from the principal the guard
	// resolved. An operator id used to travel beside it, converted from
	// that same principal by each transport and stamped onto the context
	// here — so a question asking who was calling read a STRING derived
	// from an answer already in the context. It is gone: every personal
	// question reads [iam.From] directly, which is the one place the
	// caller is established.
	//
	// AND THE REFUSAL IS THREE-VALUED, which is why the resolution is read
	// rather than dropped. A principal with no grants is the same zero
	// value whether nobody presented a credential or this node could not
	// CHECK the one presented, and those want opposite answers: the first
	// is about the caller, who has to present something else, and the
	// second is about the node. Reported as the first, an outage tells
	// everybody holding a perfectly good credential that theirs is no
	// good — which is how a company gets taught to reset working passwords
	// while the identity estate is down.
	//
	// A STAGE THAT MAY NOT ACT IS REFUSED HERE, whatever the posture, which
	// is the authority table's own first check: an [Everybody] question
	// decides nothing else, so without it a suspended person's live
	// session would go on reading the overview that every other gesture of
	// theirs is refused.
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
// nobody could establish, classified into the two things that actually cause
// it — because this package already reports three kinds of failure and these
// are two of them.
//
//   - THE IDENTITY ESTATE COULD NOT BE READ. A resolver ran, said so, and left
//     its reason behind. That is "not yet": the same [ErrUnavailable] a
//     state-log read refusal gets, carrying a Retry-After rather than advice
//     to go and fix a credential that was never wrong.
//   - NO RESOLVER EVER TOUCHED THIS CONTEXT ([iam.ErrUnresolved]). That is a
//     wiring bug — a handler reached by a path nobody put through the guard,
//     or a context somebody forgot to thread — and it never clears by waiting.
//     So it is a plain failure, which this surface renders as the third thing:
//     a bug to report. A Retry-After here would send a client round a loop
//     that can only ever end when somebody edits the routing.
//
// Neither is [ErrUnauthorized], and that is the whole point of reading the
// resolution: the caller's credential is not what is wrong in either case.
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

// ClassifyFailure sorts a failed answer into this registry's own vocabulary: a
// read THIS NODE could not serve becomes [ErrUnavailable], a read whose request
// named another log's position becomes [ErrBadParams], and every other failure
// is left alone.
//
// AT THE REGISTRY, ONCE, rather than at each answer that reads something
// that can be unreachable or refused. It was per answer, and the answers that
// did not call it were exactly the ones reported: an unreadable lease table
// reached `fleet` as a plain failure and `sandbox_runs` likewise, so a
// coordination blip rendered as `query_failed` and a 500, telling a client to
// give up on a screen that would work in a few seconds. The reference had
// promised a 503 for both.
//
// What this node could not serve is [unavailableHere] — the tools' own rule,
// each arm of it a sentinel its own subsystem marks, rather than a second list
// here.
//
// A refusal about the REQUEST is never reclassified, even when it wraps one of
// those: the caller has to change what it asks, and "come back" would send the
// identical request round a loop.
//
// AND ONE REQUEST MISTAKE IS RAISED ONLY BY THE READ ITSELF, so it is
// classified here as [ErrBadParams]: a position the caller named on another
// domain's log ([statelog.ErrForeignPosition]) — a `min_position`, or a feed's
// `cursor` or `since`. The grammar that parses a position cannot know which
// log a question reads, and the reader can — so the reader refuses it, before
// it consults anything about this node, and this is where that refusal
// becomes the caller's. A floor was a `wrong_stream` refusal, answered
// `unavailable`: a 503 telling a client to ask another node, which refused it
// identically, and a dashboard telling a person an operator had to act.
//
// EXPORTED for the code host's verify routes (internal/api/code.go), which
// answer outside the registry — an operation on THIS node's copy, not a
// question — and in its vocabulary: their "not yet" and their misnamed
// position are this classification, never a second list of what clears by
// waiting.
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
// IT IS THE TOOLS' OWN RULE ([builtin.Condition]) and not a list of this
// registry's, because the two answer the same reads: the tracker and the
// knowledge base are read through the estate's router whichever surface asks,
// and a second list here had already drifted from the first — it knew the
// router's unserved answer while the tools did not, and the tools knew a queue
// that is not live, a broker that did not answer, a store held closed while it
// adopts a snapshot and a wait that ran out, which this one did not, so a read
// the router could not even send answered `bad_params` or a 500 here. What it
// covers:
//
//   - EVERY state-log refusal ([statelog.ErrUnavailable]), a read's or a
//     write's, whether or not waiting clears it. A node that is behind will
//     catch up; a node holding a record it cannot decode, one whose log is
//     full or whose broker refused the barrier will not — but none of those
//     is a FAULT, and reported as one (a 500 `query_failed`, with the
//     refusal's words sent only to the log) it told a caller the server had
//     broken while hiding the remedy the refusal names. Whether to come back
//     is carried separately, by [statelog.RetryAfter], which is ZERO for a
//     refusal waiting cannot clear — so a surface answers it with no
//     Retry-After and a client does not go round a loop.
//   - coord.ErrUnavailable, the coordination contract's own third answer:
//     the store could not be reached, which is neither "held" nor "absent".
//   - the estate router finding no copy to answer from (estate.ErrUnserved):
//     no live node holds data, or none that does answers — a copy out of
//     service or a peer restarting reaches this surface as this.
//   - this node's own queue or store not ready for the read, and a read that
//     ran out of time — see [builtin.Condition] for each.
//
// ASKED BY THE ANSWERS as well as the registry, for the answers that read what
// is left of a failure as a refusal of the request: handed one of these, they
// told a caller to change a question nothing was wrong with.
func unavailableHere(err error) bool {
	_, ok := builtin.Condition(err)
	return ok
}

// PostureOf reports who a question is registered as being asked by.
//
// THE REGISTRY'S DECLARED POSTURE, readable without running the answer —
// which is what a gate asserting the posture needs and the only caller there
// is. Nothing enforces with it: [Registry.AnswerWith] makes the decision for
// both transports, and each maps the refusal its own way (the socket to
// `unauthorized`, a REST route to a 403 carrying the same code); a second
// enforcement point is exactly what AnswerWith's own comment says must not
// exist.
//
// An unregistered name answers the zero posture, which is invalid — so a
// caller asserting about one is told it is not a question rather than being
// handed a plausible-looking answer about nothing.
func (r *Registry) PostureOf(what string) Posture {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[what].posture
}
