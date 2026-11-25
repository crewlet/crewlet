package harness

import (
	"cmp"
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
// A vendor's limit belongs to the account a call is made as (Claude Code's windows
// are shared by every model on the plan), so it is kept per sign-in ([SignInID]) in
// one node-wide registry, [Plans], which every build is handed and every apply outlives.
//   - AN APPLY KEEPS A SIT-OUT: a spec holds `${VAR}` names, not values, so copying a
//     sit-out to an unchanged spec kept a new token benched and dropped one on a
//     timeout edit. A rebuild with the same sign-in finds its Plan; a new credential, a clean one.
//   - ENTRIES ON ONE CREDENTIAL PAUSE TOGETHER (decision D6).
//   - A LIMIT OF ONE MODEL HOLDS THAT MODEL ONLY ([llm.LimitModel], [Plan.scoped]),
//     matched exactly: "sonnet" is not "claude-sonnet-5-5".
// WHO HAS TO AGREE: this node alone; in memory, lost on restart (fleet clear: [Plan.Clear]).

// Plans is the node's registry of sign-ins, created once by the engine and
// handed to every provider build ([Config.Plans]), so that a rebuilt provider
// with the same sign-in finds the same [Plan].
//
// It also holds the key a [SignInID] is computed under, because the two are
// one decision: ids are only comparable between providers that share both.
type Plans struct {
	// key is the HMAC key every SignInID is computed under ([NewPlans]).
	key []byte

	// entryKey is the HMAC key every [Provider.EntryID] is computed under
	// ([FleetPlans]); nil in a registry made for sign-ins alone, whose
	// providers name no entry.
	entryKey []byte

	mu    sync.Mutex
	plans map[SignInID]*Plan
}

// SignInKeyInfo is the HKDF info the engine derives the sign-in key under
// from its keyring's active key ([secrets.Keyring.Derive]).
const SignInKeyInfo = "crewlet/llm-sign-in-id/v1"

// EntryKeyInfo is the HKDF info the entry key is derived under, apart from the
// sign-in key so that neither id is computed under the other's key.
const EntryKeyInfo = "crewlet/llm-entry-id/v1"

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

// FleetPlans is the engine's registry: its sign-in key derived under
// [SignInKeyInfo] and its entry key under [EntryKeyInfo], each from the
// keyring's active key (derive, [secrets.Keyring.Derive]), so both ids are
// the same on every node and across restarts. A keyring with no active key
// derives nothing, and the registry is [NewPlans](nil)'s.
func FleetPlans(derive func(info string) []byte) *Plans {
	plans := NewPlans(derive(SignInKeyInfo))
	if entry := derive(EntryKeyInfo); len(entry) > 0 {
		plans.entryKey = slices.Clone(entry)
	}
	return plans
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
	// the harness was given, from a report whose window is [llm.LimitModel].
	scoped map[string]*bench
	// ready is the readiness memory ([Provider.Unready]).
	ready readiness
	// lastAnswer is when a call on the sign-in last answered.
	lastAnswer time.Time
	// lastFailure is the last call that set the readiness verdict: a
	// refused sign-in, or a harness that could not be started
	// ([Provider.remember]). A limit is the sit-out's, not this.
	lastFailure *Failure
	// limits are the vendor's windows as the sign-in's calls and plan reads
	// last reported them, by [llm.LimitReading.Key] ([Plan.Observe]).
	limits map[string]Limit
	// readWanted is when a call last opened a window ([Plan.ReadWanted]).
	readWanted time.Time
}

func newPlan() *Plan { return &Plan{scoped: map[string]*bench{}, limits: map[string]Limit{}} }

// Limit is one of a sign-in's windows as its calls last reported it.
type Limit struct {
	llm.LimitReading
	// Seen is when the run that reported it was read.
	Seen time.Time
}

// Observe keeps what a run or a plan read at `at` reported of the sign-in's
// windows, and decides the plan-wide window by it ([Plan.reckon]): a window's
// reading replaces an older one, never a newer, so a run collected late does
// not undo what a later call heard.
func (pl *Plan) Observe(readings []llm.LimitReading, at time.Time) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	var newer []llm.LimitReading
	for _, r := range readings {
		if held, ok := pl.limits[r.Key()]; ok && !at.After(held.Seen) {
			continue
		}
		pl.limits[r.Key()] = Limit{LimitReading: r, Seen: at.UTC()}
		newer = append(newer, r)
	}
	pl.reckon(newer, onTheWall(at))
}

// MaxSaid bounds the vendor's sentence a [Failure] or a [SitOut] carries. The
// useful part is its head ("You've hit your session limit · resets 4:30pm
// (America/Vancouver)" is 66 bytes), and what a node publishes about its
// models is rewritten on its status record at least every reconcile interval;
// the whole sentence is in the node's own log line.
const MaxSaid = 240

// Failure is a call on a sign-in that set its readiness verdict: a refused
// sign-in, or a harness that could not be started.
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
	// Limits are the vendor's windows as the sign-in's calls last reported
	// them, in the order a person reads them.
	Limits []Limit
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
	for _, l := range pl.limits {
		state.Limits = append(state.Limits, l)
	}
	slices.SortFunc(state.Limits, func(a, b Limit) int {
		return cmp.Or(cmp.Compare(slices.Index(llm.LimitWindows, a.Window), slices.Index(llm.LimitWindows, b.Window)),
			cmp.Compare(a.Model, b.Model))
	})
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
	b.until, b.said, b.resetsAt, b.since, b.window = onTheWall(out.Until), out.Said,
		onTheWall(out.ResetsAt), onTheWall(out.Since), out.Window
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
