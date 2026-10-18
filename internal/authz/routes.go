package authz

import (
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
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
// is the prefix rule this replaces, where `/worklet/items/` and
// `/worklet/items/{key}/purge` were one gate because one is a prefix of the
// other.
type Policy struct {
	// Action is the verb this route asks for. Its zero is invalid and
	// [Handle] refuses it, so a route mounted without one is a build
	// failure rather than an open door.
	Action Action

	// Object builds the object from the matched request. Nil means the
	// action needs no object beyond its own kind — every [ClassEverybody],
	// [ClassApp], [ClassAnyApp] and [ClassAdmin] route — and a class that
	// reads a field of an absent object refuses with [ReasonUnnamed] rather
	// than guessing.
	Object func(*http.Request) Object

	// Absent is what a PUT to this route's address does where the address
	// holds nothing: it creates it, or it is refused and the member is added
	// elsewhere. Nil for every other route. See [Absent].
	Absent *Absent

	// Effect is what a POST to this route does to what its path names: it
	// creates, rewrites, destroys or only reads it. Every POST declares one
	// (a walk in internal/api holds them), and no other method may. See
	// [Effect].
	Effect Effect

	// DryRun says the write answers `?dry_run=true` as a check: the same
	// request, judged in the same order, that stores, activates and publishes
	// nothing. See [Policy.Checked].
	DryRun bool

	// Summary says the write records a revision summary for the history, which
	// it reads from the `X-Summary` header or a top-level `_summary` key in its
	// JSON body, and refuses a write without one. See [Policy.Summarised].
	Summary bool

	// Replaces says the write is a PUT that stores its body as the whole
	// object at its address, in the shape the GET on the same address
	// answers, so a field the body leaves out is removed. See
	// [Policy.Replacing].
	Replaces bool
}

// Absent is a PUT route's answer to an address that holds nothing, DECLARED
// WHERE THE ROUTE IS MOUNTED so the handler that decides it and every reader
// of the route table say the same thing.
//
// The reader it exists for is the Dashboard Assistant's preview of a generic
// change (internal/assistant/catalog): a PUT whose read found nothing is
// proposed as a create only where the route creates, and refused at once,
// with the route's own sentence about where one is added, where it does not.
// Learned only from the write, the difference was a proposal the person
// approved and the API then refused, every time: `PUT /config/roles/{id}`
// never adds a seat, which has a place in the chart its path cannot name.
//
// The alternative, asking the route itself with `dry_run=true`, would put a
// PUT on a read grant, whose whole guarantee is that it sends nothing but GET
// and HEAD (ADR-0050); a route that ignored the flag would then write.
type Absent struct {
	// Creates says a write there creates what it names, sent create-only
	// (`If-None-Match: *`) by a caller that must not replace one a racer
	// added.
	Creates bool
	// Elsewhere is where a member is added instead, for a route that does
	// not create by address: the sentence its handler refuses with.
	Elsewhere string
}

// CreatesWhenAbsent declares a PUT route that creates what its address names.
func CreatesWhenAbsent() *Absent { return &Absent{Creates: true} }

// AddedElsewhere declares a PUT route that does not create by address, and
// where a member is added instead.
func AddedElsewhere(where string) *Absent { return &Absent{Elsewhere: where} }

// WhenAbsent is p declaring what a PUT to an empty address does.
func (p Policy) WhenAbsent(a *Absent) Policy {
	p.Absent = a
	return p
}

// Effect is what a POST does to what its path names, DECLARED WHERE THE ROUTE
// IS MOUNTED beside [Absent], for the same reader: the Dashboard Assistant's
// preview of a generic change (internal/assistant/catalog) titles a change and
// rates its risk by it.
//
// THE METHOD SAYS IT FOR EVERY OTHER WRITE: a PUT replaces what its address
// holds, a PATCH merges into it and a DELETE takes it away. A POST's says
// nothing, and read off its method the assistant called every POST a create,
// so a person approving a purge read "Create purge" under "Approve and
// create", in the ordinary tone, with no word that anything goes for good.
// Only the route knows which it is, so the route says.
type Effect string

const (
	// EffectCreates is a POST that adds a record: a member of the collection
	// its path names (a task, a page, a view), or one beneath the record it
	// names (a comment, a version, an upload).
	EffectCreates Effect = "creates"
	// EffectRewrites is a POST that acts on what its path names and keeps
	// it: a rename, a restore, a move, a revert, a fleet gesture that can be
	// taken back.
	EffectRewrites Effect = "rewrites"
	// EffectDestroys is a POST that destroys what its path names, or a part
	// of it, for good: a purge, an erase, a prune, a member taken out of the
	// fleet's broker, sessions ended.
	EffectDestroys Effect = "destroys"
	// EffectReads is a POST that changes nothing: its body is a question too
	// large or too sensitive for a URL (the files a publish would take, the
	// names of the secrets to reveal, a credential to try at its vendor).
	EffectReads Effect = "reads"
	// EffectCalls is the act route's: a POST that calls the tool its path
	// names, which alone decides what changes. The act route decides inside
	// its handler and is mounted outside every [Router], so the table that
	// binds it says so (internal/api's AssistantRoutes), and no route
	// mounted here may.
	EffectCalls Effect = "calls"
)

// Effects are the five, in declaration order.
var Effects = []Effect{EffectCreates, EffectRewrites, EffectDestroys, EffectReads, EffectCalls}

// Valid reports whether e is an effect this build knows.
func (e Effect) Valid() bool { return slices.Contains(Effects, e) }

// Creating is p declaring that its POST adds a record ([EffectCreates]).
func (p Policy) Creating() Policy {
	p.Effect = EffectCreates
	return p
}

// Rewriting is p declaring that its POST acts on what its path names and keeps
// it ([EffectRewrites]).
func (p Policy) Rewriting() Policy {
	p.Effect = EffectRewrites
	return p
}

// Destroying is p declaring that its POST destroys what its path names, or a
// part of it, for good ([EffectDestroys]).
func (p Policy) Destroying() Policy {
	p.Effect = EffectDestroys
	return p
}

// Reading is p declaring that its POST changes nothing ([EffectReads]).
func (p Policy) Reading() Policy {
	p.Effect = EffectReads
	return p
}

// Checked is p declaring that its write answers `?dry_run=true` as a check
// (ADR-0055).
//
// DECLARED WHERE THE ROUTE IS MOUNTED, beside [Absent], for the same reader:
// the Dashboard Assistant's preview of a generic change runs the route's own
// check before it draws a card, so a body the route would refuse (a field it
// does not know, a document the company would not validate) is told to the
// model at once, never found by the person after they approved and confirmed
// it was them. A route that says nothing is never asked, because only a route
// that reads the flag stores nothing for it.
//
// A CHECK IS DECIDED AS THE WRITE, at the read's proof ([IsCheck]): the same
// verb, class, app and assistant column, without the step-up, because it
// changes nothing. So the person's assistant checks exactly what the person's
// own session could.
func (p Policy) Checked() Policy {
	p.DryRun = true
	return p
}

// Summarised is p declaring that its write requires a revision summary, read
// from `X-Summary` or a top-level `_summary` key in its JSON body. The
// assistant's generic change carries one in the body, which the proposal's
// digest binds, so the history line is the one the person approved.
func (p Policy) Summarised() Policy {
	p.Summary = true
	return p
}

// Replacing is p declaring that its PUT replaces the whole object at its
// address with the body, the object being the one the GET on the same address
// answers: whatever that representation holds and the body leaves out is
// removed.
//
// DECLARED WHERE THE ROUTE IS MOUNTED, beside [Policy.Checked], for the same
// reader: the Dashboard Assistant's preview of a generic change compares the
// body with the representation it read, and a field the body would drop by
// omission is never approved by accident (ADR-0055). A route whose GET answers
// fields its PUT does not take (a version, a timestamp) does not declare it,
// because there a missing field is not a removal.
func (p Policy) Replacing() Policy {
	p.Replaces = true
	return p
}

// DryRunQuery is the query parameter a [Policy.Checked] route reads.
const DryRunQuery = "dry_run"

// IsCheck reports a request that asks a check of a route declaring one:
// `dry_run` given exactly once, as exactly `true`, on a [Policy.Checked]
// route. Any other spelling is the route's own refusal, never a check.
func IsCheck(r *http.Request, p Policy) bool {
	return p.DryRun && AsksCheck(r)
}

// AsksCheck reports a request whose query asks a check, whatever route it
// reaches: `dry_run` given exactly once, as exactly `true`.
func AsksCheck(r *http.Request) bool {
	values, given := r.URL.Query()[DryRunQuery]
	return given && len(values) == 1 && values[0] == "true"
}

// writeFormProblem says why a route's [Policy.DryRun] or [Policy.Summary]
// cannot be mounted, or "".
func writeFormProblem(pattern string, p Policy) string {
	method, _, _ := strings.Cut(pattern, " ")
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return ""
	}
	if p.DryRun || p.Summary {
		return "declares a check or a revision summary, which only a write has"
	}
	return ""
}

// replacesProblem says why a route's [Policy.Replaces] cannot be mounted, or
// "": only a PUT replaces what its address holds.
func replacesProblem(pattern string, p Policy) string {
	if method, _, _ := strings.Cut(pattern, " "); p.Replaces && method != http.MethodPut {
		return "declares that it replaces what its address holds, which only a PUT does"
	}
	return ""
}

// absentProblem says why a route's [Absent] cannot be mounted, or "".
func absentProblem(pattern string, a *Absent) string {
	method, _, _ := strings.Cut(pattern, " ")
	switch {
	case a == nil:
		return ""
	case method != http.MethodPut:
		return "declares what an empty address does, which only a PUT's address has"
	case a.Creates == (strings.TrimSpace(a.Elsewhere) != ""):
		return "declares an empty address both created and added elsewhere, or neither"
	}
	return ""
}

// effectProblem says why a route's [Effect] cannot be mounted, or "": only a
// POST's method says nothing of what it does, and a route mounted here never
// calls a tool ([EffectCalls] is the act route's alone).
func effectProblem(pattern string, e Effect) string {
	method, _, _ := strings.Cut(pattern, " ")
	switch {
	case e == "":
		return ""
	case method != http.MethodPost:
		return "declares what a POST does, which its method already says"
	case !e.Valid() || e == EffectCalls:
		return "declares the effect " + strconv.Quote(string(e)) + ", which a route " +
			"mounted here does not have"
	}
	return ""
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
		check := Checking(r.Context()) || IsCheck(r, p)
		// A READ GRANT SENDS A WRITE'S METHOD ONLY AS A CHECK of a route
		// that declares one (ADR-0055). The edge admits such a grant's
		// write method only with `dry_run=true`, which says nothing about
		// whether the route reads it; this is where the route's own
		// declaration is asked, so a route that would ignore the flag and
		// write is never reached.
		if assistedRead(principal) && !readMethod(r.Method) && !check {
			return Decision{Reason: ReasonAssistScope}
		}
		var object Object
		if p.Object != nil {
			object = p.Object(r)
		}
		// NOW, at the request: how recently the caller proved who they
		// are is judged at the instant they ask, against the deadlines
		// the guard composed for this very request.
		return decide(r.Context(), principal, p.Action, object, chart, time.Now(), check)
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

// PolicyMux is a [Mux] that is also told the policy each route is mounted
// with, once the route is registered on it.
//
// FOR A TABLE THAT KEEPS EVERY PATTERN. internal/api's route table lists every
// route it is handed, and a walk over the guarded routes (the Dashboard
// Assistant's catalogue walk, which must classify each one by its verb) needs
// the verb beside the pattern, which only the router held. Reporting it to
// the mux keeps one table of both rather than a second list of verbs.
type PolicyMux interface {
	Mux
	MountedWith(pattern string, p Policy)
}

// Within is a mux that mounts every route on inner and reports each policy to
// outer: for a surface that serves its routes behind a sub-mux of its own (one
// wrapper round all of them) while the table it is mounted on keeps their
// verbs. Where outer keeps no policies it is inner itself.
func Within(inner, outer Mux) Mux {
	keeper, ok := outer.(PolicyMux)
	if !ok {
		return inner
	}
	return within{Mux: inner, outer: keeper}
}

// within is [Within]'s mux.
type within struct {
	Mux
	outer PolicyMux
}

func (w within) MountedWith(pattern string, p Policy) { w.outer.MountedWith(pattern, p) }

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
// surface takes from here rather than keeping a copy — and for its reason: what
// the caller waits for is this node applying the company it is one activation
// behind, or its identity read coming back — the scale of one batch, not of an
// outage. Longer leaves a person staring at a screen
// that could already answer; shorter turns a lagging node's every open tab
// into a retry storm against the node least able to take it.
const RetryUndecidedSeconds = 2

// EnvelopeRefusal is the rendering every surface that states no wording of its
// own gets: the engine's refusal envelope, the same one every other JSON
// surface answers with.
//
// NOT [net/http.Error], which is what it was. That writes `text/plain` and
// `nosniff` — the pairing that stops a strict client parsing the answer at
// all — and a sentence rather than a code, so the /iam surface refused a
// request in a shape no client of the rest of this API could read: no `error`
// to branch on, no grant to name, and a 503 with no `Retry-After`, which a
// client cannot tell from a node that is down for good.
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
//   - A MACHINE TOKEN on a verb that needs a person present is `403
//     token_refused`, its own code for step_up_required's reason: the
//     `unauthorized` code's sentence is about a grant the credential lacks,
//     and no grant on a token would admit it. It was that code with a
//     sentence of its own as `detail`, so the envelope's `message` and its
//     `detail` contradicted each other.
//   - THE DASHBOARD ASSISTANT on a verb of the human-only list is `403
//     unauthorized` with the `assist_refused` reason, and its `detail` is the
//     sentence the list gives that verb's family ([AssistNeverSentence]), so
//     the person reads why it stays with them and where they do it.
//   - A CALLER WITH NO RELATION to an artifact is `404 not_found` with
//     nothing beside it ([Decision.NotFound]): no reason and no grants, the
//     answer a surface gives for an id nothing holds, so the refusal cannot
//     say whether the artifact exists.
func EnvelopeRefusal(w http.ResponseWriter, _ *http.Request, p Policy, d Decision) {
	switch {
	case d.Unknown():
		httpjson.Unavailable(w, httpjson.CodeUnavailable, RetryUndecidedSeconds)
	case d.NotFound():
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
	case d.Reason == ReasonStepUp:
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeStepUpRequired,
			StepUpDetail(d))
	case d.Reason == ReasonTokenRefused:
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeTokenRefused,
			RefusalDetail(d.Reason, d.Grants))
	case d.Reason == ReasonAssistRefused:
		// THE PERSON IS TOLD WHY IT STAYS WITH THEM, and where they do it,
		// in the sentence the human-only list gives the verb's family: the
		// drawer shows `detail` as the refusal's words.
		detail := RefusalDetail(d.Reason, d.Grants)
		if tell := AssistNeverSentence(p.Action); tell != "" {
			detail["detail"] = tell
		}
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeUnauthorized, detail)
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
	if problem := absentProblem(pattern, p.Absent); problem != "" {
		return newPolicyError(pattern, problem)
	}
	if problem := effectProblem(pattern, p.Effect); problem != "" {
		return newPolicyError(pattern, problem)
	}
	if problem := writeFormProblem(pattern, p); problem != "" {
		return newPolicyError(pattern, problem)
	}
	if problem := replacesProblem(pattern, p); problem != "" {
		return newPolicyError(pattern, problem)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, dup := t.policies[pattern]; dup {
		return newPolicyError(pattern, "is mounted twice")
	}
	t.policies[pattern] = p
	t.mux.Handle(pattern, t.wrap(p, h))
	if keeper, ok := t.mux.(PolicyMux); ok {
		keeper.MountedWith(pattern, p)
	}
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
		if IsCheck(r, p) {
			// A CHECK FOR THE WHOLE REQUEST, so the handler's own decisions
			// are asked as the route's was ([WithCheck]).
			r = r.WithContext(WithCheck(r.Context()))
		}
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
// and what a reader thinks it matched: `/worklet/items/../config` reaches
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
// read different ones. The escaped check catches `/worklet/items/../config` as it
// was sent. The decoded check catches `/worklet/items/%2e%2e/config`, whose
// escaped form is already clean — and which ServeMux does NOT redirect
// (measured on go1.27): it MATCHES on the escaped path, segment by segment, so
// `/worklet/items/%2e%2e` reaches the `{key}` route with a key of `..`. What
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
// KEPT, because ServeMux treats `/worklet/items/` and `/worklet/items` as different
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
