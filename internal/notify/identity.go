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
// consults both, so a fellow agent's message is annotated exactly as a
// person's is. The companion holds what a surface gives an agent as a BOT —
// a chat bot's user id, a GitHub App's bot login — and the transport's own
// namespace everything else: the people's ids (a Slack member id, the user id
// a Mattermost username resolves to), and an agent's own account where the
// surface gives it no bot (a GitLab username, a Jira account id). Neither
// holds an address a message is written with: a Mattermost username is
// resolved to the id its posts carry before it is registered.
//
// Kept apart rather than in one map because the two are written by
// different owners on different schedules — the human reconcile and the
// people's transport, an integration's agents at connect or on an apply —
// and each withdraws only what it wrote.
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
// A seat's external id is not fixed for the life of a process: an apply can
// correct a person's member id, and a Mattermost person's username can come
// to name another account, which the transport then registers in its
// place. Writing only the handle -> id direction would leave the OLD id
// still resolving to this seat — a stale alias that outlives the seat's own
// decommission (which removes only the current id) and then silently
// swallows a later seat legitimately registered under the freed id. So a
// re-registration withdraws the id this handle previously held in the same
// namespace.
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
// Pure map lookups: this is the inbound hot path, run once per sender per
// changelog line, so there are no org walks here. The registry IS the
// maintained index.
//
// Both namespaces are consulted — the transport's own, then its bot
// companion — so a fellow agent's message annotates exactly the way a
// human's does. See [botSuffix].
//
// AN ENGINE SURFACE'S ID IS THE HANDLE: on a namespace [NewRegistry] was
// handed as one whose ids are seat handles, this is [Registry.ByHandle]. So a
// person's comment on the engine's own tracker resolves to the seat they
// write as, exactly as their message on a vendor's chat resolves to the seat
// their member id is registered for.
//
// A miss is ORDINARY, not an error: most senders on a shared channel are
// not colleagues, and the caller renders them as an outside party.
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
// And an id two human seats both declare is registered for NEITHER, and
// reported ([SharedIdentity]): whichever the walk reached last used to keep
// it, which is a guess about which person an account is, made silently.
//
// # Human ids need no integration — except a NAME
//
// Unlike agent identities, which each transport resolves against the
// third-party app at connect, a human's ids are declared in config.
// Registration is therefore synchronous and runs at boot and on every org
// swap, with no network and nothing to await.
//
// The exception is a contact the chart declares as the name a person is
// addressed by on a surface whose events carry an account id instead
// ([org.Transport.AddressedByName] — Mattermost's username). That name is
// skipped here, because registered as written it is a key no event looks up:
// every post its owner wrote reached the agents from a stranger. The
// surface's transport resolves it against its server and registers the id it
// is answered with, into this same registry.
//
// lookup resolves ${VAR} references; nil reads the process environment. An
// identity whose reference does not resolve is skipped and counted, never
// registered as its literal text — a raw ${VAR} matches no payload any
// third-party app will ever send.
//
// # And the directory decides who may still be reached
//
// standing is one reading of the identity directory ([Standing]): a seat whose
// holder is suspended, retired or removed has its declared identities left out
// — so on this registry an inbound message from them resolves to an outside
// party, and nothing attributes their word to the seat. The reading is applied
// to o through [Standing.Seats]. The zero Standing is chart-only and withholds nothing. The
// registry remembers which reading it was built from ([Registry.Standing]),
// because that reading is half of what it answers for.
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
					// The transport's to register: see "except a
					// NAME" above. Still counted as declared and
					// resolved, because an unset ${VAR} behind it
					// is this function's to report either way.
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
