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
	"sort"
	"strconv"
	"sync"
)

// Errors a query surface reports precisely. Everything else a query returns is
// a failure whose reason reaches the log rather than the caller.
var (
	// ErrUnknown is a name nothing answers.
	ErrUnknown = errors.New("queries: unknown query")

	// ErrUnauthorized is a query that needs an operator and did not get
	// one. Returned rather than decided here, so the refusal reaches a
	// client as a code it already handles.
	ErrUnauthorized = errors.New("queries: query requires an operator")

	// ErrBadParams is a request this surface understood and refused.
	ErrBadParams = errors.New("queries: bad parameters")
)

// Params are one query's arguments.
//
// It exists so a single answer function can be fed from both transports: a
// socket frame carries a JSON object, and a REST call carries a query string.
// Without it each question would need two readers, which is where a filter
// honoured on one path and ignored on the other comes from.
type Params struct{ values map[string]any }

// FromMap reads a socket frame's params.
func FromMap(m map[string]any) Params { return Params{values: m} }

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

	// operator marks a question only an authenticated operator may ask.
	// The config surface is all of them: reading it exposes the whole
	// company document, including every ${VAR} reference by name.
	operator bool
}

// Registry is the set of questions this process can answer.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]entry
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{entries: map[string]entry{}} }

// Register adds a question.
//
// Registering a name twice is a programming error and panics, rather than
// silently taking one of them: two answers to one question is exactly the
// divergence this package exists to prevent, and a wiring mistake that
// resolved to whichever ran last would be invisible.
func (r *Registry) Register(name string, answer Answer) {
	r.register(name, entry{answer: answer})
}

// RegisterOperator adds a question only an authenticated operator may ask.
func (r *Registry) RegisterOperator(name string, answer Answer) {
	r.register(name, entry{answer: answer, operator: true})
}

func (r *Registry) register(name string, e entry) {
	if name == "" {
		panic("queries: a question needs a name")
	}
	if e.answer == nil {
		panic(fmt.Sprintf("queries: %q registered with no answer", name))
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
	out := make([]string, 0, len(r.entries))
	for name := range r.entries {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Answer runs one question.
//
// The signature is the socket's, because the socket is the surface with a
// name-to-question dispatch; a REST route knows its own question and calls the
// answer directly. Both reach the same function either way.
func (r *Registry) Answer(ctx context.Context, what string, params map[string]any, operatorID string) (any, error) {
	return r.AnswerWith(ctx, what, FromMap(params), operatorID)
}

// AnswerWith runs one question against already-read parameters.
//
// The REST route reaches this one, because its parameters are a query string
// rather than a JSON object. Both entry points meet at the same answer with the
// same authorization check — the alternative is a route that reads its own
// params and forgets the operator check, which is the shape of the bug this
// package exists to make impossible.
func (r *Registry) AnswerWith(ctx context.Context, what string, p Params, operatorID string) (any, error) {
	r.mu.RLock()
	e, known := r.entries[what]
	r.mu.RUnlock()
	if !known {
		return nil, fmt.Errorf("%w: %q", ErrUnknown, what)
	}
	if e.operator && operatorID == "" {
		return nil, fmt.Errorf("%w: %q", ErrUnauthorized, what)
	}
	return e.answer(ctx, p)
}

// RequiresOperator reports whether a question needs one, for a REST route that
// has to make the same decision before it calls the answer.
func (r *Registry) RequiresOperator(what string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[what].operator
}
