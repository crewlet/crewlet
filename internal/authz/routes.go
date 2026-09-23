package authz

import (
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
)

// Policy is what one route requires, declared where the route is mounted.
//
// IT TRAVELS WITH THE REGISTRATION, which is the whole design. The
// alternative is a middleware that decides from the request — and a
// middleware runs BEFORE the mux matches, so `r.Pattern` is empty there and
// anything it decided from would be a second router that has to agree with
// ServeMux about which handler runs. That divergence is not hypothetical: it
// is the prefix rule this replaces, where `/work/items/` and
// `/work/items/{key}/purge` were one gate because one is a prefix of the
// other.
type Policy struct {
	// Action is the verb this route asks for. Its zero is invalid and
	// [Handle] refuses it, so a route mounted without one is a build
	// failure rather than an open door.
	Action Action

	// Object builds the object from the matched request. Nil means the
	// action needs no object beyond its own kind — every [ClassRead] and
	// [ClassOperator] route — and a class that reads a field of an
	// absent object refuses with [ReasonUnnamed] rather than guessing.
	Object func(*http.Request) Object
}

// Guard decides a request, and is what [Handle] wraps a route with.
//
// A SEAM RATHER THAN A CALL TO [Decide]: the principal comes from the request
// context and the chart from the running engine, neither of which this
// package holds, and both of which a test wants to write down.
type Guard func(r *http.Request, p Policy) Decision

// ContextGuard is the [Guard] every surface builds that takes its caller from
// the request context — the principal internal/api's guard resolved — and its
// lead relations from chart.
//
// ONE FUNCTION RATHER THAN A COPY PER SURFACE. Four surfaces wrote this body
// for themselves, and the one clause each had to get right is the one that
// is easy to drop: a caller this node could not RESOLVE is decided UNKNOWN,
// never as the zero principal. Decided as the zero principal it is a 403
// naming a grant the caller may very well hold, on every request, for as
// long as the identity estate is unreadable.
//
// A surface whose rules ask no relation — the deployment's own controls, the
// company document, the credential store — passes [NoChart], which says so
// where a nil would look like an omission.
func ContextGuard(chart Chart) Guard {
	return func(r *http.Request, p Policy) Decision {
		principal, how := iam.From(r.Context())
		if how == iam.Unknown {
			return Decision{Err: iam.Reason(r.Context())}
		}
		var object Object
		if p.Object != nil {
			object = p.Object(r)
		}
		// NOW, at the request: how recently the caller proved who they
		// are is judged at the instant they ask, against the deadlines
		// the guard composed for this very request.
		return Decide(r.Context(), principal, p.Action, object, chart, time.Now())
	}
}

// Admit decides one more verb from INSIDE a handler the router already
// admitted, and answers the refusal itself when there is one. It reports
// whether the handler may go on.
//
// FOR A QUESTION THE PATTERN CANNOT ASK. A route is mounted at the verb its
// pattern names; whether the request ALSO needs another is sometimes only
// visible once it is read — `?reveal=true` on a credential, a setup
// submission that turns out to carry one. Both questions go to the same guard
// over the same table, so this is the route's policy asked twice rather than
// a second gate with rules of its own, and its refusal is the router's.
func Admit(w http.ResponseWriter, r *http.Request, guard Guard, p Policy) bool {
	d := guard(r, p)
	if d.Unknown() || !d.Allowed {
		EnvelopeRefusal(w, r, p, d)
		return false
	}
	return true
}

// Refusal renders a request the guard did not admit: refused, or undecidable.
//
// A SEAM because the WORDING of a refusal belongs to the surface that owns the
// verb, not to the router that happened to stop it. A surface whose verbs are
// also tools answers in the tools' own sentence, so a person reading a 403 and
// a model reading a refused call read the same words — and a router that wrote
// its own sentence would be the one place the two diverged. The decision is
// still the router's alone: this is handed a refusal and can only render it.
type Refusal func(w http.ResponseWriter, r *http.Request, p Policy, d Decision)

// Mux is the part of *http.ServeMux registration uses, declared here so a
// test can pass a recorder and walk what was mounted.
type Mux interface {
	Handle(pattern string, h http.Handler)
}

// Router mounts guarded routes and remembers every pattern it mounted.
//
// THE MOUNT IS THE ONLY READER OF r.Pattern, which is what makes the gate and
// the handler the same decision: the wrapper closes over the policy of the
// pattern it was registered for, so there is no lookup at request time and no
// way for the two to disagree. A pattern reached without a policy cannot
// exist, because the only way to mount one is through here.
type Router struct {
	mux    Mux
	guard  Guard
	refuse Refusal

	mu       sync.Mutex
	policies map[string]Policy
}

// NewRouter wraps a mux so every route mounted through it is guarded.
func NewRouter(mux Mux, guard Guard) *Router {
	return &Router{mux: mux, guard: guard, refuse: EnvelopeRefusal,
		policies: map[string]Policy{}}
}

// Refusing replaces how a refusal is RENDERED, and nothing about whether one
// is made. Set before the first mount; see [Refusal].
//
// A NIL REFUSAL KEEPS [EnvelopeRefusal] rather than writing nothing, because a
// wrapper that stopped a request and wrote no status would answer 200 with an
// empty body — a refusal that reads, to every client, as the write landing.
func (t *Router) Refusing(render Refusal) *Router {
	if render != nil {
		t.refuse = render
	}
	return t
}

// RetryUndecidedSeconds is the `Retry-After` on a request this node could not
// decide, in seconds.
//
// TWO, the value the identity surface gives its own 503s — and the one the work
// surface and /chart take from here rather than keeping a copy — and for its
// reason: what the caller waits for is this node's chart
// view catching up by one apply, or its identity read coming back — the scale
// of one batch, not of an outage. Longer leaves a person staring at a screen
// that could already answer; shorter turns a lagging node's every open tab
// into a retry storm against the node least able to take it.
const RetryUndecidedSeconds = 2

// EnvelopeRefusal is the rendering every surface that states no wording of its
// own gets: the engine's refusal envelope, the same one every other JSON
// surface answers with.
//
// NOT [net/http.Error], which is what it was. That writes `text/plain` and
// `nosniff` — the pairing that stops a strict client parsing the answer at
// all — and a sentence rather than a code, so the /iam and /chart surfaces
// refused a request in a shape no client of the rest of this API could read:
// no `error` to branch on, no grant to name, and a 503 with no `Retry-After`,
// which a client cannot tell from a node that is down for good.
//
//   - UNKNOWN IS NOT A REFUSAL. This node could not decide — it is behind, or
//     it holds no company yet — so it is `503 unavailable` with a
//     `Retry-After`, never a 403 that sends somebody to ask for an authority
//     they already hold. See the package doc.
//   - A REFUSAL is `403 unauthorized`, carrying the rule's reason and the
//     grants that would have admitted the caller ([RefusalDetail]).
//   - A STALE PROOF is `403 step_up_required` — the code the sign-in surface
//     already answers for the same fact, so a client learns ONE spelling —
//     carrying the window it needs ([StepUpDetail]): the rule admitted the
//     caller, and confirming who they are and replaying the request is the
//     whole remedy, which a client can do without asking anybody.
func EnvelopeRefusal(w http.ResponseWriter, _ *http.Request, _ Policy, d Decision) {
	switch {
	case d.Unknown():
		httpjson.Unavailable(w, httpjson.CodeUnavailable, RetryUndecidedSeconds)
	case d.Reason == ReasonStepUp:
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeStepUpRequired,
			StepUpDetail(d))
	default:
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeUnauthorized,
			RefusalDetail(d.Reason, d.Grants))
	}
}

// Handle mounts one guarded route.
//
// It REFUSES at mount time rather than at request time: an empty action, an
// action this build has no rule for, or a pattern mounted twice are all
// mistakes a person makes while writing the route table, and every one of
// them is invisible afterwards. Reported rather than panicking, because the
// caller is a route table that can collect them and name all of them at once.
func (t *Router) Handle(pattern string, p Policy, h http.Handler) error {
	switch {
	case strings.TrimSpace(pattern) == "":
		return ErrNoPattern
	case p.Action == "":
		return newPolicyError(pattern, "names no action")
	}
	if _, known := rules[p.Action]; !known {
		return newPolicyError(pattern,
			"names the action "+string(p.Action)+", which this build has no rule for")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, dup := t.policies[pattern]; dup {
		return newPolicyError(pattern, "is mounted twice")
	}
	t.policies[pattern] = p
	t.mux.Handle(pattern, t.wrap(p, h))
	return nil
}

// Patterns is every pattern mounted through this router, sorted.
//
// WHAT THE WALK READS. It sees only what this router mounted, which is the
// limit worth stating: a route registered straight on the mux is invisible
// here, and the gate against that is that internal/api holds no other
// reference to its mux.
func (t *Router) Patterns() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Sorted(maps.Keys(t.policies))
}

// PolicyFor reports the policy a pattern was mounted with.
func (t *Router) PolicyFor(pattern string) (Policy, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.policies[pattern]
	return p, ok
}

// wrap is the guard around one route's handler.
func (t *Router) wrap(p Policy, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := t.guard(r, p)
		if d.Unknown() || !d.Allowed {
			t.refuse(w, r, p, d)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// CanonicalPath refuses a request whose path is not already in canonical form.
//
// # It is OUTSIDE the mux, and it has to be
//
// A gate is worth exactly as much as the agreement between what it matched
// and what a reader thinks it matched: `/work/items/../config` reaches
// ServeMux as one pattern and reads to a person as another.
//
// The obvious home is the per-route wrapper, beside the decision it protects,
// and that home does not work — which this package's own test is what showed.
// ServeMux cleans a literal `..`, `.` or `//` and answers a 307 ITSELF,
// before it matches, so a check inside any handler never sees that path at
// all: every such case came back 307 with the route's guard never entered. So this is the one
// authority concern that belongs in the OUTER middleware, and it earns that
// place by the same rule the principal resolution does — it needs no route
// identity, and `r.Pattern` is empty out there anyway.
//
// REFUSED RATHER THAN REDIRECTED. ServeMux's own answer is a redirect, which
// every client follows, so the request arrives a second time with the
// decision still unmade and a reader of the access log sees the clean path
// alone. A surface that means to be strict about what it matched has to stop
// the first one.
//
// BOTH SPELLINGS OF THE PATH ARE CHECKED, because two readers of one request
// read different ones. The escaped check catches `/work/items/../config` as it
// was sent. The decoded check catches `/work/items/%2e%2e/config`, whose
// escaped form is already clean — and which ServeMux does NOT redirect
// (measured on go1.27): it MATCHES on the escaped path, segment by segment, so
// `/work/items/%2e%2e` reaches the `{key}` route with a key of `..`. What
// makes that a hole is everything that decides OUTSIDE the mux on the DECODED
// `r.URL.Path`: the cross-site write check's exemption list
// (`auth.Unguarded`) and the drain gate both read `/webhooks/%2e%2e/config`
// as `/webhooks/../config`, a path under an exempt prefix, while the mux
// routes it by segments that name something else.
// A decision made on one spelling and a dispatch made on the other are two
// readers disagreeing about one request, which is the thing this refuses.
//
// IN THE ENVELOPE, like every other refusal on this API, rather than
// [net/http.Error]'s `text/plain`.
func CanonicalPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, decoded := r.URL.EscapedPath(), r.URL.Path
		if raw != cleanPath(raw) || decoded != cleanPath(decoded) {
			httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeNonCanonicalPath)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cleanPath is path.Clean with the trailing slash kept.
//
// KEPT, because ServeMux treats `/work/items/` and `/work/items` as different
// patterns and path.Clean drops the slash — so comparing against a cleaned
// path without it would refuse every legitimately slash-terminated route.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	out := path.Clean(p)
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}
