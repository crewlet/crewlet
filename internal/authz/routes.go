package authz

import (
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
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
	return &Router{mux: mux, guard: guard, refuse: plainRefusal,
		policies: map[string]Policy{}}
}

// Refusing replaces how a refusal is RENDERED, and nothing about whether one
// is made. Set before the first mount; see [Refusal].
//
// A NIL REFUSAL KEEPS THE PLAIN ONE rather than writing nothing, because a
// wrapper that stopped a request and wrote no status would answer 200 with an
// empty body — a refusal that reads, to every client, as the write landing.
func (t *Router) Refusing(render Refusal) *Router {
	if render != nil {
		t.refuse = render
	}
	return t
}

// plainRefusal is the rendering every surface that states no wording of its
// own gets.
func plainRefusal(w http.ResponseWriter, _ *http.Request, _ Policy, d Decision) {
	if d.Unknown() {
		// UNKNOWN IS NOT A REFUSAL. This node could not decide — it is
		// behind, or it holds no company yet — and 403 would send
		// somebody to ask for an authority they already hold. See the
		// package doc.
		http.Error(w, "this node cannot decide authority for this "+
			"request yet; try again", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "forbidden: "+string(d.Reason), http.StatusForbidden)
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
// ServeMux cleans the path and answers 301 ITSELF, before it matches, so a
// check inside any handler never sees an uncanonical path at all: every case
// came back 307 with the route's guard never entered. So this is the one
// authority concern that belongs in the OUTER middleware, and it earns that
// place by the same rule the principal resolution does — it needs no route
// identity, and `r.Pattern` is empty out there anyway.
//
// REFUSED RATHER THAN REDIRECTED. ServeMux's own answer is a redirect, which
// every client follows, so the request arrives a second time with the
// decision still unmade and a reader of the access log sees the clean path
// alone. A surface that means to be strict about what it matched has to stop
// the first one.
func CanonicalPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw := r.URL.EscapedPath(); raw != cleanPath(raw) {
			http.Error(w, "the request path is not in canonical form",
				http.StatusBadRequest)
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
