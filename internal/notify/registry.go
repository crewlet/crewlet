package notify

import (
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// Registry answers "who is this?" for one epoch of the company.
//
// The SEAT INDEX is derived from the org and immutable: an epoch swap builds a
// NEW registry, so seat reads need no lock. The EXTERNAL-IDENTITY MAP is
// written at runtime and read on the inbound hot path, hence a RWMutex.
//
// Human contact identities also depend on one reading of the identity directory
// ([Standing]): a suspended or removed holder registers none. A moved directory
// is a NEW registry, never a patch, which would drop untouched identities.
//
// Resolution is ORG-derived, never node-derived: each event topic has ONE
// fleet-wide consumer group, so the winning node rarely runs the recipient.
type Registry struct {
	orgName string

	// Immutable after construction. No lock guards these.
	byHandle map[string]Party
	byRole   map[string]string    // role name  -> handle
	byID     map[uuid.UUID]string // derived id -> handle
	byEmail  map[string]string    // matched form (iam.NormalizeEmail) -> handle
	parties  []Party              // org order, stable
	// handles is every namespace whose ids ARE seat handles — see
	// [NewRegistry]. A fact about the engine rather than the org, and
	// fixed for the registry's life like the rest of this half.
	handles map[string]bool

	// external and reverse are exact INVERSES of each other within each
	// namespace — a bijection between the ids a namespace knows and the
	// seats holding them. [Registry.Register] and [Registry.Unregister]
	// each maintain both directions inside one critical section, and every
	// read relies on it: a seat holds at most one id per namespace, and an
	// id belongs to at most one seat. Breaking it does not raise — it
	// produces a seat whose outbound identity and inbound attribution name
	// two different accounts.
	mu       sync.RWMutex
	external map[string]map[string]string // namespace -> external id -> handle
	reverse  map[string]map[string]string // namespace -> handle -> external id
	// owned is the (namespace, external id) -> handle set the last human
	// reconcile registered, so the next one can withdraw exactly its own
	// stale pairs without ever touching an agent identity.
	owned map[identityKey]string

	// standing is the reading of the identity directory the last human
	// reconcile applied, and withheld the seats it left out because of it.
	// See [Standing].
	standing Standing
	withheld map[string]Withholding
}

// identityKey is one external identity: a namespace and an id within it.
type identityKey struct{ namespace, externalID string }

// NewRegistry indexes an organization's seats.
//
// BOTH kinds are indexed: indexing only agents would make every human a
// stranger. A nil org yields an empty registry, not a panic.
//
// LOOKUP RESOLVES EACH SEAT'S DECLARED ADDRESS ([org.Role.ResolvedEmail]), as
// [Registry.ReconcileHumanContacts] does; nil reads the process environment. A
// `${VAR}` address indexed as written matches no payload.
//
// handleNamespaces are the sources whose ids ARE seat handles (the engine's own
// surfaces): [Registry.ByExternalID] there answers [Registry.ByHandle].
// DECLARED by the caller, which knows domains this package cannot import.
func NewRegistry(o *org.Organization, lookup org.EnvLookup, handleNamespaces ...string) *Registry {
	r := &Registry{
		byHandle: make(map[string]Party),
		byRole:   make(map[string]string),
		byID:     make(map[uuid.UUID]string),
		byEmail:  make(map[string]string),
		handles:  make(map[string]bool, len(handleNamespaces)),
		external: make(map[string]map[string]string),
		reverse:  make(map[string]map[string]string),
		owned:    make(map[identityKey]string),
	}
	for _, ns := range handleNamespaces {
		if ns = strings.TrimSpace(ns); ns != "" {
			r.handles[ns] = true
		}
	}
	if o == nil {
		return r
	}
	r.orgName = o.Name
	for role := range o.AllRoles() {
		handle := role.Handle()
		if handle == "" {
			// A seat with no derivable handle cannot be addressed,
			// named in a topic or given an id. Validation rejects
			// one, so reaching here means a caller built an
			// Organization by hand; skipping is the only answer that
			// does not corrupt the index with an empty key.
			continue
		}
		// FIRST WINS, on every index. Validation rejects a duplicate
		// handle, so a collision here is a hand-built org — and a later
		// seat silently displacing an earlier one would re-point an
		// address that other rows already reference.
		if _, dup := r.byHandle[handle]; dup {
			continue
		}
		p := Party{Handle: handle, Name: role.Name, Human: role.IsHuman()}
		if !p.Human {
			if id, ok := o.AgentIDFor(role); ok {
				p.AgentID = id
			}
		}
		r.byHandle[handle] = p
		r.parties = append(r.parties, p)
		if _, dup := r.byRole[role.Name]; !dup {
			r.byRole[role.Name] = handle
		}
		if p.AgentID != uuid.Nil {
			if _, dup := r.byID[p.AgentID]; !dup {
				r.byID[p.AgentID] = handle
			}
		}
		if email := iam.NormalizeEmail(role.ResolvedEmail(lookup)); email != "" {
			if _, dup := r.byEmail[email]; !dup {
				r.byEmail[email] = handle
			}
		}
	}
	return r
}

// OrgName is the company the registry answers for. Empty without an org.
func (r *Registry) OrgName() string { return r.orgName }

// Standing is the reading of the identity directory this registry's human
// contacts were reconciled against — the zero, chart-only value until a
// reconcile applies one.
//
// It is what lets the engine tell a directory that moved from one that did not
// WITHOUT rebuilding: a fresh reading equal to this one is a registry that
// would come out identical.
func (r *Registry) Standing() Standing {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.standing
}

// Withholding reports whether a human seat's contact identities were left out
// of this registry because of its holder's standing, and why.
//
// The seat itself is still a party — it resolves by handle, a mention of it
// still names it and a prompt still renders it as a colleague. What is gone is
// every external identity it declared, which is what stops a suspended
// person's message on a vendor surface being attributed to the seat.
func (r *Registry) Withholding(handle string) (Withholding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	why, ok := r.withheld[handle]
	return why, ok
}

// Withheld is every seat [Registry.Withholding] answers for, by its handle,
// sorted — the count a log line reports, which is the number of SEATS
// this registry withdrew rather than the number of bindings the reading named.
func (r *Registry) Withheld() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.withheld))
}

// ByHandle resolves a handle to a party.
func (r *Registry) ByHandle(handle string) (Party, bool) {
	p, ok := r.byHandle[handle]
	return p, ok
}

// ByRole resolves an exact role name to a party.
//
// Exact, never fuzzy. Fuzzy matching over role names is what lookup_colleague
// does for a model that typed a name from memory; a router doing the same
// would deliver someone else's mail on a typo.
func (r *Registry) ByRole(name string) (Party, bool) {
	handle, ok := r.byRole[name]
	if !ok {
		return Party{}, false
	}
	return r.ByHandle(handle)
}

// ByAgentID resolves a derived agent id to a party — the inverse of
// [org.DeriveAgentID].
//
// Internal events carry an agent id rather than a handle, and every node can
// recompute every seat's id, so this is answerable anywhere with no store.
//
// A human seat NEVER matches here: it has no agent id. A caller that must
// tell "human recipient" from "unknown recipient" has to ask by handle or by
// role, and the two answers mean different things — one is a person who will
// reply on their own surface, the other is mail with nowhere to go.
func (r *Registry) ByAgentID(id uuid.UUID) (Party, bool) {
	if id == uuid.Nil {
		return Party{}, false
	}
	handle, ok := r.byID[id]
	if !ok {
		return Party{}, false
	}
	return r.ByHandle(handle)
}

// ByEmail resolves an address to a party. TWO ARMS, in this order:
//
//   - A PLUS TAG THAT SPELLS A SEAT'S HANDLE names that seat:
//     notif+engineer@example.com is the engineer, whatever address it declares
//     (how a shared mailbox reaches a seat), so a person's own sub-address tag
//     should not be a seat's handle.
//   - Otherwise THE SEAT WHOSE DECLARED ADDRESS HAS THE SAME MATCHED FORM, both
//     folded by [iam.NormalizeEmail] (lower-cased, plus tag dropped), the fold
//     the identity estate blinds a person's address under: sarah.chen+jira@
//     example.com is the seat declaring sarah.chen@example.com.
func (r *Registry) ByEmail(email string) (Party, bool) {
	if handle := PlusAddress(email); handle != "" {
		if p, ok := r.ByHandle(handle); ok {
			return p, true
		}
	}
	folded := iam.NormalizeEmail(email)
	if folded == "" {
		return Party{}, false
	}
	handle, ok := r.byEmail[folded]
	if !ok {
		return Party{}, false
	}
	return r.ByHandle(handle)
}

// All enumerates every addressable party, in the order the org declares
// them. Agent seats and human seats alike.
func (r *Registry) All() iter.Seq[Party] { return slices.Values(r.parties) }

// Len is the number of addressable parties.
func (r *Registry) Len() int { return len(r.parties) }

// PlusAddress extracts the handle from a plus-addressed email.
//
//	notif+engineer@example.com -> "engineer"
//	alice@example.com          -> ""
//
// Lowercased, because a handle is lowercase by construction and a sender who
// typed one in caps still means the same seat.
func PlusAddress(email string) string {
	local, _, ok := strings.Cut(email, "@")
	if !ok {
		return ""
	}
	_, handle, ok := strings.Cut(local, "+")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(handle))
}

// SeatEmail builds the plus-addressed email for a handle.
//
//	SeatEmail("engineer", "example.com", "notif") -> "notif+engineer@example.com"
//
// It refuses an invalid handle rather than emitting an address that cannot
// round-trip: [PlusAddress] lowercases, and the handle grammar forbids the
// characters that would otherwise need quoting, so an unchecked handle
// produces an address that either bounces or names a different seat.
func SeatEmail(handle, domain, prefix string) (string, error) {
	if !org.ValidHandle(handle) {
		return "", fmt.Errorf("notify: %q is not a valid handle", handle)
	}
	if domain = strings.TrimSpace(domain); domain == "" {
		return "", fmt.Errorf("notify: no domain for handle %q", handle)
	}
	if prefix = strings.TrimSpace(prefix); prefix == "" {
		prefix = "notif"
	}
	return prefix + "+" + handle + "@" + domain, nil
}
