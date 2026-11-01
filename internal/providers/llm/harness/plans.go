package harness

import (
	"crypto/rand"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A SIT-OUT AND A READINESS VERDICT BELONG TO THE SIGN-IN, NOT TO THE ENTRY.
//
// A vendor's limit is a property of the account a call is made as: Claude
// Code's session and weekly windows are shared by every model on the plan, and
// a refused token is refused whichever entry hands it over. So the record of
// what the vendor said is kept per sign-in ([SignInID]) in one node-wide
// registry, [Plans], which every provider build is handed and which outlives
// every apply, and an entry reads and writes the [Plan] of the sign-in it
// calls as.
//
// What that settles, each of which keying by the entry got wrong:
//
//   - AN APPLY KEEPS A SIT-OUT. Every apply rebuilds every entry. A sit-out
//     kept on the provider died with it, and the one rescue, copying it to a
//     rebuilt entry whose config spec was unchanged, read the wrong thing: a
//     spec holds `${VAR}` names, not values, so a new token stored under the
//     same name stayed benched for up to an hour, while an edit to the
//     entry's timeout dropped a sit-out the vendor still held. A rebuilt
//     provider with the same sign-in finds the same Plan; one with a new
//     credential has a new id, and so a clean one.
//   - ENTRIES ON ONE CREDENTIAL PAUSE TOGETHER (decision D6). Each used to
//     learn the same plan-wide limit for itself, one process launch each.
//   - A LIMIT OF ONE MODEL HOLDS THAT MODEL ONLY. "Opus limit" leaves Sonnet
//     on the same plan answering, so a window whose report names
//     [llm.LimitModel] is kept under the model argument the CLI was given
//     ([Plan.scoped]) and holds no other entry. Matched on the exact
//     argument ("sonnet" is not "claude-sonnet-5-5"), which is conservative:
//     the other spelling learns at its first call.
//
// WHO HAS TO AGREE: this node alone. The registry is in memory and lost on a
// restart, as the sit-out always was, and every other node learns the same
// window from its own calls (the fleet-wide retry and clear are the engine's,
// over [Plan.Clear]).

// Plans is the node's registry of sign-ins, created once by the engine and
// handed to every provider build ([Config.Plans]), so that a rebuilt provider
// with the same sign-in finds the same [Plan].
//
// It also holds the key a [SignInID] is computed under, because the two are
// one decision: ids are only comparable between providers that share both.
type Plans struct {
	// key is the HMAC key every SignInID is computed under ([NewPlans]).
	key []byte

	mu    sync.Mutex
	plans map[SignInID]*Plan
}

// SignInKeyInfo is the HKDF info the engine derives the sign-in key under
// from its keyring's active key ([secrets.Keyring.Derive]).
const SignInKeyInfo = "crewlet/llm-sign-in-id/v1"

// NewPlans makes a registry whose ids are computed under key: the engine's
// keyring-derived key ([SignInKeyInfo]), which makes an id the same on every
// node and across restarts, so a run's row can name the sign-in it launched
// with. A nil or empty key draws a random one, for a process with no keyring
// to derive from (a `crewlet llm` command): its ids are then comparable within
// that process only, which is all such a process compares.
func NewPlans(key []byte) *Plans {
	if len(key) == 0 {
		key = make([]byte, 32)
		// crypto/rand.Read never returns an error on a supported platform,
		// and fails the process rather than return a short read.
		_, _ = rand.Read(key)
	}
	return &Plans{key: slices.Clone(key), plans: map[SignInID]*Plan{}}
}

// For is the Plan of the sign-in id names, created on first use. The empty id
// (nothing signs the entry in) is nobody's sign-in, so it is never shared: each
// call gets a Plan of its own, held by the provider that asked.
func (r *Plans) For(id SignInID) *Plan {
	if id == "" {
		return newPlan()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.plans[id]
	if !ok {
		plan = newPlan()
		r.plans[id] = plan
	}
	return plan
}

// Sweep drops every Plan no live provider names and that holds no window in
// force at now: a credential rotated away, an entry removed. One that still
// holds a window is kept, so a credential put back before its window ends is
// still sitting it out. Run by the engine at each apply, with the ids of the
// revision's providers.
func (r *Plans) Sweep(live []SignInID, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, plan := range r.plans {
		if slices.Contains(live, id) || plan.holdsWindow(now) {
			continue
		}
		delete(r.plans, id)
	}
}

// Plan is what one sign-in's calls have met on this node: the windows it sits
// out, the readiness verdict, and the last answer and the last failure, under
// one mutex so that a reader sees one consistent account.
type Plan struct {
	mu sync.Mutex
	// wide is the plan-wide window: a session, weekly or unnamed limit,
	// which holds every model on the sign-in.
	wide bench
	// scoped are windows of one model each, keyed by the model argument
	// the CLI was given, from a report whose window is [llm.LimitModel].
	scoped map[string]*bench
	// ready is the readiness memory ([Provider.Unready]).
	ready readiness
	// lastAnswer is when a call on the sign-in last answered.
	lastAnswer time.Time
	// lastFailure is the last call that set the readiness verdict: a
	// refused sign-in, or a CLI that could not be started
	// ([Provider.remember]). A limit is the sit-out's, not this.
	lastFailure *Failure
}

func newPlan() *Plan { return &Plan{scoped: map[string]*bench{}} }

// MaxSaid bounds the vendor's sentence a [Failure] or a [SitOut] carries. The
// useful part is its head ("You've hit your session limit · resets 4:30pm
// (America/Vancouver)" is 66 bytes), and what a node publishes about its
// models is re-sent on every heartbeat; the whole sentence is in the node's
// own log line.
const MaxSaid = 240

// Failure is a call on a sign-in that set its readiness verdict: a refused
// sign-in, or a CLI that could not be started.
type Failure struct {
	Kind llm.ErrorKind
	At   time.Time
	// Said is the vendor's own sentence, at most [MaxSaid] bytes.
	Said string
}

// SitOut is a window a sign-in sits out.
type SitOut struct {
	// Until is when the entry is tried again, ResetsAt when the vendor said
	// the limit lifts (zero where it did not), and Since when the report that
	// opened the window was made.
	Until, ResetsAt, Since time.Time
	// Window is which limit it is.
	Window llm.LimitWindow
	// Model is the model argument a [llm.LimitModel] window holds, "" for a
	// plan-wide one.
	Model string
	// Said is the vendor's sentence, at most [MaxSaid] bytes.
	Said string
}

// PlanState is a reading of a [Plan] at one instant.
type PlanState struct {
	// SittingOut is every window in force, the plan-wide one first.
	SittingOut []SitOut
	// LastAnswer is when a call on the sign-in last answered, zero for
	// never on this node since it started.
	LastAnswer time.Time
	// LastFailure is the last failure worth naming, nil for none.
	LastFailure *Failure
}

// State reads the plan at now.
func (pl *Plan) State(now time.Time) PlanState {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	var state PlanState
	if out, ok := pl.wide.sitOut(now, ""); ok {
		out.Said = boundSaid(out.Said)
		state.SittingOut = append(state.SittingOut, out)
	}
	for _, model := range slices.Sorted(maps.Keys(pl.scoped)) {
		if out, ok := pl.scoped[model].sitOut(now, model); ok {
			out.Said = boundSaid(out.Said)
			state.SittingOut = append(state.SittingOut, out)
		}
	}
	state.LastAnswer = pl.lastAnswer
	if pl.lastFailure != nil {
		f := *pl.lastFailure
		state.LastFailure = &f
	}
	return state
}

// Clear ends every window opened by a report made before `before`, plan-wide
// and scoped, and reports whether it ended any. A report made at or after it
// stands: the vendor said it after the person asked, so it is the newer word.
// That keeps a clear order-free and idempotent, which a request every node
// applies at its own moment needs.
//
// The backoff restarts from its first step, as an answer after the window
// would restart it: the person who cleared it is saying the limit is over.
func (pl *Plan) Clear(before time.Time) bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	cleared := pl.wide.clearBefore(before)
	for model, b := range pl.scoped {
		if b.clearBefore(before) {
			cleared = true
			delete(pl.scoped, model)
		}
	}
	return cleared
}

// Hold sits the sign-in out for out, a window another provider on the same
// sign-in met: one built with a record of its own, such as a probe that must
// reach the vendor whatever this plan's window is. A plan-wide window (Model
// "") holds every model, a scoped one its model alone. Like a report this
// plan heard itself, it never SHORTENS a window in force, and it does not step
// the backoff: the provider that met it stepped its own.
func (pl *Plan) Hold(out SitOut) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	b := &pl.wide
	if out.Model != "" {
		if pl.scoped[out.Model] == nil {
			pl.scoped[out.Model] = &bench{}
		}
		b = pl.scoped[out.Model]
	}
	if !out.Until.After(b.until) {
		return
	}
	b.until, b.said, b.resetsAt, b.since, b.window = out.Until, out.Said, out.ResetsAt, out.Since, out.Window
	if !b.window.Valid() {
		b.window = llm.LimitUnknown
	}
}

// holdsWindow reports whether any window is in force at now.
func (pl *Plan) holdsWindow(now time.Time) bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if now.Before(pl.wide.until) {
		return true
	}
	for _, b := range pl.scoped {
		if now.Before(b.until) {
			return true
		}
	}
	return false
}

// boundSaid is a vendor's sentence within [MaxSaid] bytes, marked where it was
// cut: a person reads it, and the bound is one a heartbeat enforces.
func boundSaid(said string) string { return textcut.Within(said, MaxSaid) }
