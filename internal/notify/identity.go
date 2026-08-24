package notify

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// botSuffix names the companion namespace an agent's own bot identity lives
// in, alongside the namespace humans and tools address the same seat by.
//
// THE SPLIT IS REAL AND PER-BACKEND. The two halves of a chat integration
// see different identifiers for one seat: an inbound payload names a poster
// by bot/user id, while a human typing a mention and an MCP tool addressing
// a colleague both use the member id or username. Registering under one
// namespace only means a fellow agent's message resolves to nobody, and gets
// annotated as a stranger while a human's identical message is annotated as
// a colleague.
//
// DERIVED rather than a per-backend table. A table's failure mode is stated
// plainly: it silently stops covering the third backend nobody remembered to
// add. A derivation cannot forget one. A transport with no bot identities
// simply never has the companion namespace populated, and the extra lookup
// on a miss costs one absent map read.
const botSuffix = "_bot"

// BotNamespace is the companion namespace for a transport's own agents.
func BotNamespace(transport string) string { return transport + botSuffix }

// Register maps an external identity to a seat.
//
// # It is BIDIRECTIONAL, and that is the whole point
//
// A seat's external id is not fixed for the life of a process: a config
// apply can change a bot's username, and a transport routinely overwrites
// its configured guess with the name the SERVER reports at connect. Writing
// only the handle -> id direction would leave the OLD id still resolving to
// this seat — a stale alias that outlives the seat's own decommission
// (which removes only the current id) and then silently swallows a later
// seat legitimately provisioned under the freed name. So a re-registration
// withdraws the id this handle previously held in the same namespace.
//
// # A cross-seat steal is an ERROR, not a silent overwrite
//
// Re-registering the same pair is idempotent, which is what makes a
// reconcile cheap to re-run. But an id already held by a DIFFERENT seat
// means two seats claim one identity, and whichever registered last would
// quietly take every message addressed to the other. Refusing names both
// seats in a boot log line; overwriting produces a company where one agent's
// mail arrives at another with nothing to see.
func (r *Registry) Register(namespace, externalID, handle string) error {
	namespace, externalID = strings.TrimSpace(namespace), strings.TrimSpace(externalID)
	if namespace == "" {
		return fmt.Errorf("notify: cannot register %q under an empty namespace", externalID)
	}
	if externalID == "" {
		return fmt.Errorf("notify: cannot register an empty %s id for seat %q", namespace, handle)
	}
	if !org.ValidHandle(handle) {
		return fmt.Errorf("notify: %q is not a valid handle", handle)
	}
	// The seat must EXIST. A mapping onto an unknown handle is inert —
	// resolution ends at a handle lookup that misses — so without this
	// check a transport registering against a stale epoch's seat list
	// fails by having every message from that party resolve to nobody,
	// with nothing anywhere saying why.
	if _, ok := r.ByHandle(handle); !ok {
		return fmt.Errorf("notify: no seat %q in %s", handle, orgLabel(r.orgName))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if held, ok := r.external[namespace][externalID]; ok && held != handle {
		return fmt.Errorf("notify: %s id %q is already held by seat %q, not %q",
			namespace, externalID, held, handle)
	}
	if previous, ok := r.reverse[namespace][handle]; ok && previous != externalID {
		// Unconditional, and safe because of the bijection: a reverse
		// entry naming previous exists only while the forward entry for
		// previous names this handle. Guarding the delete on that would
		// read as though the id might belong to someone else by now,
		// which no path here can produce — and a branch that cannot be
		// taken is a claim that the invariant is weaker than it is.
		delete(r.external[namespace], previous)
	}
	if r.external[namespace] == nil {
		r.external[namespace] = make(map[string]string)
		r.reverse[namespace] = make(map[string]string)
	}
	r.external[namespace][externalID] = handle
	r.reverse[namespace][handle] = externalID
	return nil
}

// Unregister removes a mapping, but only while it still points at expected.
//
// The guard is not optional, and no unconditional form exists. Every removal
// in the system is a seat withdrawing its OWN identity — a decommission, a
// rename, a reconcile dropping a pair it registered last time — and between
// reading an id and removing it, that id may legitimately have moved to
// another seat. An unconditional delete at that moment strips a live seat's
// identity on behalf of a dead one.
//
// Reports whether a mapping was removed.
func (r *Registry) Unregister(namespace, externalID, expected string) bool {
	if namespace == "" || externalID == "" || expected == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.external[namespace][externalID] != expected {
		return false
	}
	delete(r.external[namespace], externalID)
	// Both halves, unconditionally — same bijection. The forward entry
	// named expected, so expected's reverse entry names this id.
	delete(r.reverse[namespace], expected)
	return true
}

// ByExternalID resolves a transport-scoped external id to a party.
//
// Pure map lookups: this is the inbound hot path, run once per sender per
// changelog line, so there are no org walks here. The registry IS the
// maintained index.
//
// Both namespaces are consulted — the transport's own, then its bot
// companion — so a fellow agent's message annotates exactly the way a
// human's does. See [botSuffix].
//
// A miss is ORDINARY, not an error: most senders on a shared channel are
// not colleagues, and the caller renders them as an outside party.
func (r *Registry) ByExternalID(transport, externalID string) (Party, bool) {
	if transport == "" || externalID == "" {
		return Party{}, false
	}
	r.mu.RLock()
	handle, ok := r.external[transport][externalID]
	if !ok {
		handle, ok = r.external[BotNamespace(transport)][externalID]
	}
	r.mu.RUnlock()
	if !ok {
		return Party{}, false
	}
	return r.ByHandle(handle)
}

// ExternalID is the id a seat is known by on a transport, for OUTBOUND
// addressing: which bot to post as, which member to mention. Empty when the
// seat has no identity there.
func (r *Registry) ExternalID(namespace, handle string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reverse[namespace][handle]
}

// Knows reports whether an external id belongs to any party in this company.
//
// The question a mention filter asks. A comment naming five people on a
// shared tracker must fan out only to the ones who are seats here — without
// the check, every outsider mentioned anywhere becomes a delivery attempt.
// Both namespaces, for the same reason [ByExternalID] consults both.
func (r *Registry) Knows(namespace, externalID string) bool {
	if namespace == "" || externalID == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.external[namespace][externalID]; ok {
		return true
	}
	_, ok := r.external[BotNamespace(namespace)][externalID]
	return ok
}

// Namespaces lists the namespaces holding at least one mapping, sorted.
func (r *Registry) Namespaces() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for ns, ids := range r.external {
		if len(ids) > 0 {
			out = append(out, ns)
		}
	}
	slices.Sort(out)
	return out
}

// Identities is every mapping in a namespace, external id to handle. A copy,
// so a caller iterating it cannot be racing a transport's registration.
func (r *Registry) Identities(namespace string) map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return maps.Clone(r.external[namespace])
}

// Conflict is one identity a reconcile refused to move.
type Conflict struct {
	Namespace  string
	ExternalID string
	// Wanted is the seat that declared this identity; HeldBy is the seat
	// the registry already has it mapped to.
	Wanted string
	HeldBy string
}

// Reconciliation is what one [Registry.ReconcileHumanContacts] pass did.
type Reconciliation struct {
	Registered int
	Withdrawn  int
	// Unresolved counts declared identities whose ${VAR} reference did
	// not resolve. They are not a failure — the variable may be exported
	// before the next pass — but a person who mysteriously never gets
	// mentioned is otherwise invisible, so the count gives it a number.
	Unresolved int
	Conflicts  []Conflict
}

// ReconcileHumanContacts brings the registry in line with the human seats'
// declared contact ids.
//
// # A RECONCILE, not an append
//
// Pairs a previous pass registered that are no longer declared — a corrected
// user id, a removed seat, a human seat flipped to an agent — are withdrawn
// first. Without that, an edited id keeps attributing the old value to the
// seat for the life of the process, and an id that legitimately moved to
// another seat stays blocked by the conflict guard until restart.
//
// Only pairs THIS function registered are ever withdrawn. An agent identity
// that happens to collide is left exactly where it is: a human must never
// silently take over an agent's mapping, or the reverse.
//
// # Human ids need no integration
//
// Unlike agent identities, which each transport resolves against the vendor
// at connect, a human's ids are declared in config. Registration is
// therefore synchronous and runs at boot and on every org swap, with no
// network and nothing to await.
//
// lookup resolves ${VAR} references; nil reads the process environment. An
// identity whose reference does not resolve is skipped and counted, never
// registered as its literal text — a raw ${VAR} matches no payload any
// vendor will ever send.
func (r *Registry) ReconcileHumanContacts(o *org.Organization, lookup org.EnvLookup) Reconciliation {
	var rec Reconciliation
	desired := make(map[identityKey]string)
	if o != nil {
		for role := range o.AllRoles() {
			if !role.IsHuman() || role.Contact == nil {
				continue
			}
			handle := role.Handle()
			if handle == "" {
				continue
			}
			declared := len(role.Contact.Identities())
			resolved := role.Contact.ResolvedIdentities(lookup)
			rec.Unresolved += declared - len(resolved)
			for _, id := range resolved {
				desired[identityKey{string(id.Transport), id.ExternalID}] = handle
			}
		}
	}

	// Withdraw ours that are no longer wanted, BEFORE registering — an id
	// moving between two human seats is one withdraw and one register, and
	// doing them in the other order makes the move collide with itself.
	r.mu.RLock()
	previous := maps.Clone(r.owned)
	r.mu.RUnlock()
	for key, handle := range previous {
		if desired[key] != handle && r.Unregister(key.namespace, key.externalID, handle) {
			rec.Withdrawn++
		}
	}

	owned := make(map[identityKey]string, len(desired))
	for _, key := range slices.SortedFunc(maps.Keys(desired), compareIdentityKey) {
		handle := desired[key]
		if err := r.Register(key.namespace, key.externalID, handle); err != nil {
			held := r.heldBy(key)
			if held == "" || held == handle {
				// Not a contention — an invalid handle, or a seat
				// the org does not have. Register named it; there
				// is no second seat to report.
				log.Warn("human_contact_rejected", "transport", key.namespace,
					"handle", handle, "error", err.Error())
				continue
			}
			// NOT recorded as owned: this pass did not register it,
			// and owned means exactly what this pass registered. The
			// next reconcile would be refused anyway — Unregister
			// checks the holder — so this is the second of two guards
			// on one rule, deliberately. One keeps the bookkeeping
			// honest; the other keeps a wrong entry from doing damage.
			rec.Conflicts = append(rec.Conflicts, Conflict{
				Namespace: key.namespace, ExternalID: key.externalID,
				Wanted: handle, HeldBy: held,
			})
			continue
		}
		owned[key] = handle
		rec.Registered++
	}

	r.mu.Lock()
	r.owned = owned
	r.mu.Unlock()
	if rec.Registered > 0 || rec.Withdrawn > 0 || len(rec.Conflicts) > 0 {
		log.Info("human_contacts_reconciled",
			"registered", rec.Registered, "withdrawn", rec.Withdrawn,
			"unresolved", rec.Unresolved, "conflicts", len(rec.Conflicts))
	}
	for _, c := range rec.Conflicts {
		log.Warn("human_contact_id_conflict", "transport", c.Namespace,
			"handle", c.Wanted, "already_held_by", c.HeldBy)
	}
	return rec
}

func (r *Registry) heldBy(key identityKey) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.external[key.namespace][key.externalID]
}

func compareIdentityKey(a, b identityKey) int {
	if c := strings.Compare(a.namespace, b.namespace); c != 0 {
		return c
	}
	return strings.Compare(a.externalID, b.externalID)
}

func orgLabel(name string) string {
	if name == "" {
		return "an empty organization"
	}
	return "organization " + name
}
