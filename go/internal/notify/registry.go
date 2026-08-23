package notify

import (
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/org"
)

// Registry answers "who is this?" for one epoch of the company.
//
// # Two halves, two lifetimes
//
// The SEAT INDEX is derived from the org and never changes. An epoch swap
// builds a NEW registry rather than mutating this one, which is what lets
// every seat read go through a plain map with no lock, no version counter
// and no rebuild-on-demand: the org a registry was built from is the org it
// answers for, permanently. A registry that mutated in place would have to
// answer "which org is this?" on every read, and the honest answer during a
// swap is "one of two".
//
// The EXTERNAL-IDENTITY MAP is written at runtime and does need a lock.
// Transports register their agents' bot ids once they have resolved them
// against the vendor, and human contact ids are reconciled at boot and on
// every org swap. It is small, written rarely and read on the inbound hot
// path, so a RWMutex is the right trade.
//
// # Resolution is ORG-derived, never node-derived
//
// Routing an event to a seat needs its handle and its agent id, both of
// which any node derives from the org with no database and no running
// instance. This matters because each engine event topic has ONE fleet-wide
// consumer group: the node that wins a delivery is rarely the node running
// the recipient, so a registry that could only resolve locally-running seats
// would turn most deliveries into terminal drops. Whether this node may RUN
// the seat is a separate question with a separate answer — the seat lease —
// and deliberately not asked here.
type Registry struct {
	orgName string

	// Immutable after construction. No lock guards these.
	byHandle map[string]Party
	byRole   map[string]string    // role name  -> handle
	byID     map[uuid.UUID]string // derived id -> handle
	byEmail  map[string]string    // lowercased -> handle
	parties  []Party              // org order, stable

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
}

// identityKey is one external identity: a namespace and an id within it.
type identityKey struct{ namespace, externalID string }

// NewRegistry indexes an organization's seats.
//
// BOTH kinds are indexed. A human seat has no agent id and no inbox, but it
// is addressable — a mention resolves to it, a prompt renders it as a
// colleague, and a notification can be routed to the surface it lives on.
// Indexing only agents would make every human sender an unknown stranger,
// which is exactly the annotation the prompt layer exists to avoid.
//
// A nil org yields an empty registry rather than a nil one. `crewlet
// validate` and a node that has not yet applied a revision both run without
// a company, and they should get "nobody matches" rather than a panic.
func NewRegistry(o *org.Organization) *Registry {
	r := &Registry{
		byHandle: make(map[string]Party),
		byRole:   make(map[string]string),
		byID:     make(map[uuid.UUID]string),
		byEmail:  make(map[string]string),
		external: make(map[string]map[string]string),
		reverse:  make(map[string]map[string]string),
		owned:    make(map[identityKey]string),
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
			if id, ok := org.DeriveAgentID(o.Name, handle); ok {
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
		if email := strings.ToLower(strings.TrimSpace(role.Email)); email != "" {
			if _, dup := r.byEmail[email]; !dup {
				r.byEmail[email] = handle
			}
		}
	}
	return r
}

// OrgName is the company the registry answers for. Empty without an org.
func (r *Registry) OrgName() string { return r.orgName }

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

// ByEmail resolves an address to a party.
//
// A plus-address names a seat DIRECTLY — notif+engineer@example.com is the
// engineer, whatever that seat's own configured address happens to be — so
// it is tried first. A seat's declared address is the fallback, matched
// case-insensitively because no mail system treats the local part's case as
// significant in practice and a vendor hands back whatever the sender typed.
func (r *Registry) ByEmail(email string) (Party, bool) {
	if handle := PlusAddress(email); handle != "" {
		if p, ok := r.ByHandle(handle); ok {
			return p, true
		}
	}
	handle, ok := r.byEmail[strings.ToLower(strings.TrimSpace(email))]
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
