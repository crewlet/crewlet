package notify

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// botSuffix names the companion namespace an agent's own bot identity lives
// in, beside the transport's own namespace.
//
// BOTH HOLD THE IDS AN EVENT NAMES ITS SENDER BY, and [Registry.ByExternalID]
// consults both. The companion holds what a surface gives an agent as a BOT
// (a chat bot's user id, a GitHub App's bot login); the transport's own
// namespace holds the rest: people's ids and an agent's account where there
// is no bot (GitLab username, Jira account id), never an address a message is
// written with. Kept apart because different owners write them on different
// schedules, each withdrawing only what it wrote. DERIVED rather than a
// per-backend table, which silently misses the backend nobody added.
const botSuffix = "_bot"

// BotNamespace is the companion namespace for a transport's own agents.
func BotNamespace(transport string) string { return transport + botSuffix }

// Register maps an external identity to a seat.
//
// BIDIRECTIONAL: a seat's external id changes within a process (an apply
// corrects it). Writing only handle -> id would leave the OLD id resolving to
// this seat, a stale alias that swallows a later seat registered under the
// freed id, so a re-registration withdraws the id the handle held.
// A cross-seat steal is an ERROR, not an overwrite: the same pair again is
// idempotent, but an id held by a DIFFERENT seat means two seats claim one
// identity, and the last would quietly take the other's mail. Refusing names
// both seats in a boot log line.
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
	// A HANDLE NAMESPACE HOLDS NO MAPPINGS: its ids are the seats' own
	// handles ([NewRegistry]), so a registration there is either the
	// handle again or a second name for a seat that nothing on the
	// surface ever writes — and a second name would answer before the
	// handle it contradicts.
	if r.handles[namespace] {
		return fmt.Errorf("notify: %s ids are seat handles, so %q cannot be "+
			"registered for seat %q there", namespace, externalID, handle)
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
// Pure map lookups, no org walks: the inbound hot path, once per sender per
// changelog line. Consults the transport's namespace, then its bot companion
// ([botSuffix]). On a namespace [NewRegistry] was handed as holding seat
// handles (an engine surface's id IS the handle) it is [Registry.ByHandle].
// A miss is ORDINARY: most senders in a shared channel are outside parties.
func (r *Registry) ByExternalID(transport, externalID string) (Party, bool) {
	if transport == "" || externalID == "" {
		return Party{}, false
	}
	if r.handles[transport] {
		return r.ByHandle(externalID)
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

// ExternalID is the id a seat is known by in a namespace: the reverse of
// [Registry.ByExternalID], which is what a writer reads to withdraw the id it
// registered for a seat. Empty when the seat has none there. It is the id an
// event names the sender by, never an address: a Mattermost person's is a
// 26-character user id, which no message can mention. On a namespace whose ids
// are seat handles it is the handle, for a seat the registry holds.
func (r *Registry) ExternalID(namespace, handle string) string {
	if r.handles[namespace] {
		if _, ok := r.ByHandle(handle); ok {
			return handle
		}
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reverse[namespace][handle]
}

// Knows reports whether an external id belongs to any party in this company.
//
// The question a mention filter asks. A comment naming five people on a
// shared tracker must fan out only to the ones who are seats here — without
// the check, every outsider mentioned anywhere becomes a delivery attempt.
// Both namespaces, for the same reason [ByExternalID] consults both, and a
// handle namespace by the handle, as [ByExternalID] answers it.
func (r *Registry) Knows(namespace, externalID string) bool {
	if namespace == "" || externalID == "" {
		return false
	}
	if r.handles[namespace] {
		_, ok := r.ByHandle(externalID)
		return ok
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
	// Withheld counts declared identities left out because the identity
	// directory says the seat's holder may not be reached — see
	// [Standing]. Counted apart from Unresolved because the remedies are
	// opposite: one is an environment variable, the other a person's
	// standing that an administrator set on purpose.
	Withheld  int
	Conflicts []Conflict
	// Shared is every id two or more human seats resolve to, registered for
	// none of them.
	Shared []SharedIdentity
}

// SharedIdentity is one id several human seats declared, which a reconcile
// registers for none of them.
//
// VALIDATION CANNOT SEE ONE: it compares contacts as the chart writes them,
// so two ${VAR} references that resolve to one value pass it. And one account
// is not two people, so keeping either seat would be a guess — the reconcile
// used to keep whichever it reached last, silently, and the other person's
// mail and attribution went to them.
//
// Only the seats whose identities are registered contend: a seat the
// directory withholds declares nothing this registry holds, so it shares
// nothing with anybody.
type SharedIdentity struct {
	Namespace  string
	ExternalID string
	// Handles are the seats that declared it, in chart order.
	Handles []string
}

// ReconcileHumanContacts brings the registry in line with the human seats'
// declared contact ids. A RECONCILE, not an append: pairs a previous pass
// registered that are no longer declared are withdrawn first, or an edited id
// keeps its old value and a moved id stays blocked by the conflict guard. Only
// pairs THIS function registered are withdrawn (a human never takes over an
// agent's mapping), and an id two human seats declare is registered for
// NEITHER and reported ([SharedIdentity]). lookup resolves ${VAR}, nil reading the process
// environment; an unresolved reference is skipped and counted, never
// registered as literal text. standing ([Standing], applied through
// [Standing.Seats]) leaves out a suspended, retired or removed holder's
// identities; the registry keeps it ([Registry.Standing]).
func (r *Registry) ReconcileHumanContacts(o *org.Organization, lookup org.EnvLookup,
	standing Standing) Reconciliation {

	var rec Reconciliation
	declared := make(map[identityKey][]string)
	withheld := make(map[string]Withholding)
	seats := standing.Seats(o)
	if o != nil {
		for role := range o.AllRoles() {
			if !role.IsHuman() || role.Contact == nil {
				continue
			}
			handle := role.Handle()
			if handle == "" {
				continue
			}
			resolved := role.Contact.ResolvedIdentities(lookup)
			rec.Unresolved += len(role.Contact.Identities()) - len(resolved)
			if why, ok := seats[handle]; ok {
				// NOT REGISTERED, which is the whole of a withdrawal:
				// a pair the previous pass registered is withdrawn
				// below exactly as an edited contact is, and a fresh
				// registry simply never holds it.
				withheld[handle] = why
				rec.Withheld += len(resolved)
				continue
			}
			for _, id := range resolved {
				if id.Transport.AddressedByName() {
					// A contact declared as the name a person is
					// addressed by where events carry an account id
					// (Mattermost's username) is a key no event
					// looks up: the transport registers its resolved
					// id. Still counted, since an unset ${VAR}
					// behind it is reported here either way.
					continue
				}
				key := identityKey{string(id.Transport), id.ExternalID}
				declared[key] = append(declared[key], handle)
			}
		}
	}
	desired := make(map[identityKey]string, len(declared))
	for _, key := range slices.SortedFunc(maps.Keys(declared), compareIdentityKey) {
		if handles := declared[key]; len(handles) > 1 {
			rec.Shared = append(rec.Shared, SharedIdentity{
				Namespace: key.namespace, ExternalID: key.externalID, Handles: handles,
			})
			continue
		}
		desired[key] = declared[key][0]
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
	r.standing, r.withheld = standing, withheld
	r.mu.Unlock()
	if rec.Registered > 0 || rec.Withdrawn > 0 || rec.Withheld > 0 ||
		len(rec.Conflicts) > 0 || len(rec.Shared) > 0 {
		log.Info("human_contacts_reconciled",
			"registered", rec.Registered, "withdrawn", rec.Withdrawn,
			"withheld", rec.Withheld, "unresolved", rec.Unresolved,
			"conflicts", len(rec.Conflicts), "shared", len(rec.Shared))
	}
	for _, c := range rec.Conflicts {
		log.Warn("human_contact_id_conflict", "transport", c.Namespace,
			"handle", c.Wanted, "already_held_by", c.HeldBy)
	}
	for _, shared := range rec.Shared {
		log.Warn("human_contact_id_shared", "transport", shared.Namespace,
			"handles", shared.Handles,
			"detail", "these seats' contacts resolve to one account, which is "+
				"registered for none of them: give each seat its own")
	}
	return rec
}

func (r *Registry) heldBy(key identityKey) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.external[key.namespace][key.externalID]
}

func compareIdentityKey(a, b identityKey) int {
	return cmp.Or(
		cmp.Compare(a.namespace, b.namespace),
		cmp.Compare(a.externalID, b.externalID),
	)
}

func orgLabel(name string) string {
	if name == "" {
		return "an empty organization"
	}
	return "organization " + name
}
