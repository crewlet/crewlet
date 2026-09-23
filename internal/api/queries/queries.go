// Package queries is the read surface, answered ONCE for both transports.
//
// Every question the dashboard asks has exactly one implementation here, and
// both the REST route and the WebSocket query frame call it. That is not tidy
// packaging — it is the whole point. Two surfaces answering one question from
// two implementations is how they end up disagreeing with nobody noticing,
// which happens repeatedly once they diverge: a filter honoured on one path
// and ignored on the other, a limit clamped differently, a field present over
// HTTP and missing over the socket.
package queries

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Errors a query surface reports precisely. Everything else a query returns is
// a failure whose reason reaches the log rather than the caller.
var (
	// ErrUnknown is a name nothing answers.
	ErrUnknown = errors.New("queries: unknown query")

	// ErrUnauthorized is a question asked by somebody this node KNOWS,
	// who does not carry the grant it declares — or, for a question naming
	// somebody's record, whose relation to that record does not reach it.
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
	// moment they open a screen outside their grants, which is the
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
)

// Refusal is a question refused on AUTHORITY: a caller this node knows, whose
// grants — or, for a personal question, whose relation to the record it named
// — do not reach what they asked. It is [ErrUnauthorized] to [errors.Is].
//
// A TYPE RATHER THAN A WRAPPED SENTENCE, because a surface renders what it
// carries: the rule's own reason and the grants that would have admitted the
// caller, as values a client branches on. A sentence written beside the rule
// — "needs the lead relation or fleet:operate" — is a second statement of the
// rule, and it is the copy that goes stale the day the rule's admin grant
// moves.
type Refusal struct {
	// What is what was refused: the question, or — where the question was
	// admitted and the record it named was not — the verb asked of that
	// record, which is the more useful half to a reader of the log.
	What string

	// Reason is the rule that decided, in the authority table's own words:
	// [authz.ReasonNoGrant] for a question's declared grant, and whatever
	// [authz.Decide] concluded for a record a personal question named.
	Reason authz.Reason

	// Grants are the capabilities any one of which would have admitted the
	// caller — [authz.Decision.Grants], or the one grant a question is
	// registered as needing. Empty where no capability would.
	Grants []iam.Grant
}

// refused is the refusal an [authz.Decision] makes of one question.
func refused(what string, d authz.Decision) *Refusal {
	return &Refusal{What: what, Reason: d.Reason, Grants: d.Grants}
}

// Error renders the refusal from its own values, so the sentence in a log can
// never say something the reason and the grants do not.
func (r *Refusal) Error() string {
	var remedy string
	if len(r.Grants) > 0 {
		names := make([]string, 0, len(r.Grants))
		for _, g := range r.Grants {
			names = append(names, string(g))
		}
		remedy = "; carrying " + strings.Join(names, " or ") + " would admit it"
	}
	return fmt.Sprintf("queries: %q refused (%s)%s", r.What, r.Reason, remedy)
}

// Unwrap makes a refusal [ErrUnauthorized], which is what every transport
// already maps.
func (r *Refusal) Unwrap() error { return ErrUnauthorized }

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
func FromQuery(q url.Values) Params {
	values := make(map[string]any, len(q))
	for key, list := range q {
		if len(list) > 0 {
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

	// needs is the grant a caller must carry to ask it.
	//
	// A GRANT RATHER THAN A BOOL, which is what makes a narrow reader
	// possible at all. It was `operator bool` — is there a credential,
	// yes or no — and under that a token that could read the board could
	// also read the company document and every ${VAR} reference in it by
	// name. A deliberately public read surface is now a credential
	// holding `state:read` and nothing else, and this is the field that
	// makes that mean something.
	//
	// THE ZERO IS REFUSED AT REGISTRATION, not at the first request: an
	// unstated grant would make a question this build ships ungated,
	// looking exactly like one somebody decided to leave open.
	needs iam.Grant
}

// Registry is the set of questions this process can answer.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]entry
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{entries: map[string]entry{}} }

// Register adds a question and DECLARES the grant it needs.
//
// # Why the grant is at the registration
//
// It is the only place that knows. A question's authority is a property of
// what it discloses — `config` serves the whole company document, `events`
// serves LLM transcripts, `work_items` serves the board — and none of that is
// visible from the name, the route or the caller. Stated anywhere else it
// would be a second list somebody has to keep in step with this one.
//
// A ZERO OR UNKNOWN GRANT PANICS, at wiring time, on every build. A question
// registered without one would answer to any credential at all, which is the
// exact shape of an ungated surface that looks deliberate — and the panic is
// at start-up rather than at the first request because a company finding out
// on the first request has already served it.
//
// Registering a name twice panics too, rather than silently taking one of
// them: two answers to one question is exactly the divergence this package
// exists to prevent, and a wiring mistake that resolved to whichever ran last
// would be invisible.
func (r *Registry) Register(name string, needs iam.Grant, answer Answer) {
	r.register(name, entry{answer: answer, needs: needs})
}

func (r *Registry) register(name string, e entry) {
	if name == "" {
		panic("queries: a question needs a name")
	}
	if e.answer == nil {
		panic(fmt.Sprintf("queries: %q registered with no answer", name))
	}
	if !e.needs.Valid() {
		panic(fmt.Sprintf("queries: %q registered needing grant %q, which is "+
			"not one this build knows — a question with no grant answers to "+
			"any credential at all, and looks exactly like one somebody "+
			"decided to leave open", name, e.needs))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.entries[name]; dup {
		panic(fmt.Sprintf("queries: %q registered twice", name))
	}
	r.entries[name] = e
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
	switch principal, resolution := iam.From(ctx); {
	case resolution == iam.Unknown:
		return nil, unresolved(ctx, what)
	case resolution == iam.Anonymous:
		// NOBODY IS ASKING, which is a different refusal from a
		// principal who lacks the grant — see [ErrUnauthenticated].
		return nil, fmt.Errorf("%w: %q needs %s", ErrUnauthenticated, what, e.needs)
	case !principal.Can(e.needs):
		return nil, &Refusal{What: what, Reason: authz.ReasonNoGrant,
			Grants: []iam.Grant{e.needs}}
	}
	data, err := e.answer(ctx, p)
	return data, unavailableIfTransient(err)
}

// unavailableIfTransient turns a read this node could not serve YET into
// [ErrUnavailable], leaving every other failure alone.
//
// AT THE REGISTRY, ONCE, rather than at each answer that reads something
// that can be briefly unreachable. It was per answer, and the answers that
// did not call it were exactly the ones reported: an unreadable lease table
// reached `fleet` as a plain failure and `sandbox_runs` likewise, so a
// coordination blip rendered as `query_failed` and a 500, telling a client to
// give up on a screen that would work in a few seconds. The reference had
// promised a 503 for both.
//
// Two sources of "not yet", and each is its own subsystem's classification
// rather than a second list here:
//
//   - a state-log read refusal whose code is retryable
//     ([statelog.ReadRefusal.Retryable]). A node that is behind will catch
//     up; a node holding a record it cannot decode will not, however long a
//     caller waits, so that one stays a failure.
//   - [coord.ErrUnavailable], the coordination contract's own third answer:
//     the store could not be reached, which is neither "held" nor "absent".
//
// A refusal about the REQUEST is never reclassified, even when it wraps one of
// those: the caller has to change what it asks, and "come back" would send the
// identical request round a loop.
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

func unavailableIfTransient(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrUnavailable),
		errors.Is(err, ErrBadParams),
		errors.Is(err, ErrNotFound),
		errors.Is(err, ErrUnauthenticated),
		errors.Is(err, ErrUnauthorized):
		return err
	}
	var refused *statelog.Refused
	if errors.As(err, &refused) && refused.Code.Retryable() {
		// WRAPPED, NOT REPLACED, so the refusal's own code, detail and
		// derived hint survive for [RetryAfter] and for the log.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if errors.Is(err, coord.ErrUnavailable) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}

// Needs reports the grant a question is registered as needing.
//
// THE REGISTRY'S DECLARED POSTURE, readable without running the answer —
// which is what a gate asserting the posture needs and the only caller there
// is. Nothing enforces with it: [Registry.AnswerWith] makes the decision for
// both transports, and each maps the refusal its own way (the socket to
// `unauthorized`, a REST route to a 401 carrying the same code). It said it
// was "for a REST route that has to make the same decision before it calls the
// answer"; no route does, and a second enforcement point is exactly what
// AnswerWith's own comment says must not exist.
//
// An unregistered name answers the zero grant, which is invalid — so a caller
// asserting about one is told it is not a question rather than being handed a
// plausible-looking answer about nothing.
func (r *Registry) Needs(what string) iam.Grant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[what].needs
}
