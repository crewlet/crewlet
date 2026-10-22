package configapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// The config's addressable collections — what the dashboard's Config room
// edits one at a time instead of sending the whole document back.
//
// # Why per entity at all, when PUT /config exists
//
// The whole-document write is the honest primitive and it stays. But it makes
// every edit a company-wide one: a founder renaming one seat's goal sends back
// a document carrying every other seat, every provider and every integration,
// and a concurrent edit anywhere in it is theirs to lose. Editing one entity
// narrows what a write claims to have changed, which is what makes the
// revision summary mean something and what makes two people editing different
// parts of the company safe.
//
// Why not a patch format that addresses list members instead — RFC 6902, or a
// merge key — is the question this shape invites, and the answer is worth
// stating rather than leaving to be re-derived: a patch addresses by
// STRUCTURE, and a handle is deliberately not structural. A role's position in
// a unit's list is not its identity, so a patch that named it by index would
// rewrite a different seat the moment anything above it moved.
//
// # It is the same write underneath
//
// An entity PUT is not a patch protocol. It opens the active revision,
// SPLICES the entity in, restores masks against that same revision, validates
// the WHOLE document and stores a new revision — identical to PUT /config
// from that line on. A change that would leave the company invalid is refused
// even when the entity itself is fine, because a seat naming a provider that
// no longer exists is exactly the kind of break a per-entity surface invites.
//
// # A seat is addressed by its handle and a unit by its key
//
// Both are IMMUTABLE: a seat's agent id, its mailbox and its memory derive
// from its handle, and every `manages:` entry, unit `lead:` and root seat
// `unit:` names a seat or a unit by them. A display name is only ever an edit
// of a name — every seat's handle and every unit's key is written into the
// document on every write ([config.MintIdentities]), so nothing derives either
// from a name a body changed. Changing a handle or a
// key is a removal and a creation, made through the whole document.
const (
	EntityRoles        = "roles"
	EntityUnits        = "units"
	EntityLLMProviders = "llm-providers"
	EntityMCPServers   = "mcp-servers"
)

// ErrUnknownEntityKind reports a collection this surface does not address.
var ErrUnknownEntityKind = errors.New("configapi: unknown entity kind")

// ErrNoSuchEntity reports an id nothing in the active revision carries.
var ErrNoSuchEntity = errors.New("configapi: no such entity")

// ErrEntityExists reports a create-only write (`If-None-Match: *`) naming an id
// the active revision already carries.
//
// Refused rather than turned into a replacement, because the caller said it
// was adding something: a form that adds a server called "github" to a company
// that already has one is somebody about to overwrite a colleague's launch
// command and credentials without ever having seen them.
var ErrEntityExists = errors.New("configapi: entity already exists")

// ErrNotCreatable reports a create-only write to a collection whose members
// cannot be created by address alone.
//
// A seat and a unit have a PLACE — the unit they sit in, the position among
// their siblings — and the path names neither, so "create the seat called X"
// is a question this route cannot answer without inventing one. A seat or a
// unit is added through the whole document, or by replacing the unit it sits
// in, where the place is visible.
var ErrNotCreatable = errors.New("configapi: this collection is not created by id")

// ErrIdentityMismatch reports a body whose own identity disagrees with the id
// in the path — a rename, arriving dressed as a replacement.
//
// Refused rather than applied, because an identity here is not a label. A
// seat's durable id is a UUIDv5 over (company name, handle), so a handle that
// changes under an operator strands that seat's diary, its onboarding marker
// and its counterparty profiles behind an id nothing derives any more, and
// its inbox subject with them. A unit's key is referenced by every `manages:`
// entry and root seat `unit:` that names it, and an MCP server's name by every
// `mcp_env` block, a seat's or a unit's, keyed on it; a provider's key is what
// every model chain naming it holds. None of that moves with a splice, and the
// URL is left naming something that no longer exists.
var ErrIdentityMismatch = errors.New("configapi: identity mismatch")

// identityMismatch names both halves, because the caller has to be able to
// see which one they meant.
func identityMismatch(field, pathID, bodyID string) error {
	return &MismatchError{Field: field, Path: pathID, Body: bodyID}
}

// MismatchError is an [ErrIdentityMismatch]: the id a path addresses and the
// one its body carries, under the field that carries it.
//
// TYPED, so the answer tells a person both halves without the package's name
// and the sentinel's class words, which the log keeps: answered as its
// Error(), a rename read "configapi: identity mismatch: this path addresses
// …" on the screen.
type MismatchError struct {
	// Field is the body's identity field: handle, id or name.
	Field string
	// Path is the id the path addresses, and Body the one the body carries,
	// empty where it carries none.
	Path, Body string
}

func (e *MismatchError) Error() string {
	return ErrIdentityMismatch.Error() + ": " + e.Sentence()
}

func (e *MismatchError) Unwrap() error { return ErrIdentityMismatch }

// Sentence is the mismatch as a person reads it.
func (e *MismatchError) Sentence() string {
	if e.Body == "" {
		return fmt.Sprintf("this address is %q, and the body has no %s", e.Path, e.Field)
	}
	return fmt.Sprintf("this address is %q, but the body's %s is %q", e.Path, e.Field, e.Body)
}

// entityAccess is how one collection is listed, read, replaced and added to.
//
// Typed rather than a JSON path grammar, deliberately: a path grammar would
// be a second description of the config's shape, free to drift from the Go
// types the loader and the validator actually use.
//
// EVERY MEMBER IS REQUIRED but the two a create needs, which are nil exactly
// for a collection whose members have a PLACE the path cannot name — the unit
// a seat sits in, its position among its siblings — and which answers a create
// with [ErrNotCreatable]. A table test holds every entry to that, so a
// collection cannot join the table without saying how a member of it is
// written — a nil anywhere else is a panic inside a request.
type entityAccess struct {
	// noun is what one member of the collection is called in a sentence a
	// person reads ([NotFound]): its URL segment ("mcp-servers") is a path
	// token, and a refusal that named one read like the path it came from.
	noun string
	// ids lists the identities in the document, in a stable order.
	ids func(*config.Company) []string
	// find returns the entity under an id, and whether it was there.
	find func(*config.Company, string) (any, bool)
	// replace splices a decoded entity in under an id, or reports why not.
	// The body is read as it was sent, so its failures name its own lines.
	// It never CREATES: an id that is not already there is refused, because
	// "PUT the entity called X" arriving for an X nobody has is far more
	// often a typo than an intent to add one. It never RENAMES either: a
	// body whose own identity disagrees with the id is ErrIdentityMismatch,
	// for the reasons on that sentinel.
	replace func(*config.Company, string, submitted) error
	// stored finds the entity under an id in a STORED document, decoded as
	// a tree: the same entity find returns, found the same way, so a write
	// replaces exactly the bytes of the entity it decoded.
	stored func(root map[string]any, id string) (map[string]any, bool)

	// create adds a decoded entity under an id the collection does not
	// carry — the create-only write, `If-None-Match: *` — or reports why
	// not: [ErrEntityExists] for an id already there, [ErrIdentityMismatch]
	// for a body naming another. Nil for a collection whose members have a
	// place the path cannot name ([ErrNotCreatable]).
	create func(*config.Company, string, submitted) error
	// place puts a new element into a STORED document tree where create
	// put its entity in the struct, so the two stay in the order find and
	// stored walk them.
	place func(root map[string]any, id string, element map[string]any)
}

// entityKinds is the table, and its keys are the paths the dashboard's Config
// room addresses — held against the client's own list by
// entities_client_test.go.
var entityKinds = map[string]entityAccess{
	EntityRoles: {
		noun: "seat",
		ids: func(c *config.Company) []string {
			var out []string
			for r := range c.EachRole() {
				out = append(out, roleID(r))
			}
			return sorted(out)
		},
		find: func(c *config.Company, id string) (any, bool) {
			for r := range c.EachRole() {
				if roleID(r) == id {
					return r, true
				}
			}
			return nil, false
		},
		replace: func(c *config.Company, id string, raw submitted) error {
			// FOUND FIRST, judged second. Both orders refuse the same
			// requests, but they answer a PUT to an id nothing carries
			// differently: identity-first calls that a rename and blames
			// the body, when the entity the caller addressed is simply not
			// there and the URL is what they got wrong. And the place it
			// was found is where a body that cannot be read is refused.
			var target *config.Role
			var at config.Path
			for r, p := range c.EachRole() {
				if roleID(r) == id {
					target, at = r, p
					break
				}
			}
			if target == nil {
				return ErrNoSuchEntity
			}
			incoming, err := decodeEntity[config.Role](raw, at)
			if err != nil {
				return err
			}
			// THE HANDLE IS THE ADDRESS, and a body that leaves it out
			// keeps the one the path names rather than deriving another
			// from its name: editing a seat's display name is the edit this
			// route is most often sent, and it is never a rename. A body
			// that DECLARES another handle is one, and is refused rather
			// than silently moved — the caller asked to replace the entity
			// at this id, and honouring a rename here would leave the URL
			// naming something that no longer exists.
			if strings.TrimSpace(incoming.Handle) == "" {
				incoming.Handle = id
			}
			if got := roleID(&incoming); got != id {
				return identityMismatch("handle", id, got)
			}
			*target = incoming
			return nil
		},
		stored: func(root map[string]any, id string) (map[string]any, bool) {
			return firstElement(root, false, id)
		},
	},
	EntityUnits: {
		noun: "unit",
		ids: func(c *config.Company) []string {
			var out []string
			for u := range c.EachUnit() {
				out = append(out, u.IdentityKey())
			}
			return sorted(out)
		},
		find: func(c *config.Company, id string) (any, bool) {
			for u := range c.EachUnit() {
				if u.IdentityKey() == id {
					return u, true
				}
			}
			return nil, false
		},
		replace: func(c *config.Company, id string, raw submitted) error {
			var target *config.Unit
			var at config.Path
			for u, p := range c.EachUnit() {
				if u.IdentityKey() == id {
					target, at = u, p
					break
				}
			}
			if target == nil {
				return ErrNoSuchEntity
			}
			incoming, err := decodeEntity[config.Unit](raw, at)
			if err != nil {
				return err
			}
			// THE SEAT'S RULE, by the unit's KEY and never its name: a
			// unit is referenced by its key from every `manages:` entry
			// that expands to it and every root seat whose `unit:` names
			// it, so renaming the team is an edit of its name and moving
			// the key is a rename this route refuses.
			if strings.TrimSpace(incoming.ID) == "" {
				incoming.ID = id
			}
			if got := incoming.IdentityKey(); got != id {
				return identityMismatch("id", id, got)
			}
			*target = incoming
			return nil
		},
		stored: func(root map[string]any, id string) (map[string]any, bool) {
			return firstElement(root, true, id)
		},
	},
	EntityLLMProviders: {
		noun: "model provider",
		ids: func(c *config.Company) []string {
			out := slices.Collect(maps.Keys(c.Providers.LLM))
			return sorted(out)
		},
		find: func(c *config.Company, id string) (any, bool) {
			p, ok := c.Providers.LLM[id]
			if !ok {
				return nil, false
			}
			return p, true
		},
		replace: func(c *config.Company, id string, raw submitted) error {
			if _, ok := c.Providers.LLM[id]; !ok {
				return ErrNoSuchEntity
			}
			incoming, err := decodeEntity[config.LLMProvider](raw, config.Path{"providers", "llm", id})
			if err != nil {
				return err
			}
			c.Providers.LLM[id] = incoming
			return nil
		},
		stored: func(root map[string]any, id string) (map[string]any, bool) {
			providers, _ := root["providers"].(map[string]any)
			llm, _ := providers["llm"].(map[string]any)
			provider, ok := llm[id].(map[string]any)
			return provider, ok
		},
		// A PROVIDER'S IDENTITY IS ITS KEY, and the body carries no name to
		// disagree with it: the key is the address and nothing else.
		//
		// AND IT IS ADDED LAST IN THE ORDER. The provider order is what an
		// unpinned seat's last resort reads ("the first provider
		// configured"), and a map has none, so a create records the new key
		// at the end of `llm_order` — here and in the stored document,
		// through the one reconciliation [config.Providers.ProviderOrder]
		// is. Left out, it fell into that order's sorted tail.
		create: func(c *config.Company, id string, raw submitted) error {
			if _, ok := c.Providers.LLM[id]; ok {
				return ErrEntityExists
			}
			incoming, err := decodeEntity[config.LLMProvider](raw, config.Path{"providers", "llm", id})
			if err != nil {
				return err
			}
			order := c.Providers.ProviderOrder()
			if c.Providers.LLM == nil {
				c.Providers.LLM = map[string]config.LLMProvider{}
			}
			c.Providers.LLM[id] = incoming
			c.Providers.LLMOrder = append(order, id)
			return nil
		},
		place: func(root map[string]any, id string, element map[string]any) {
			providers, _ := root["providers"].(map[string]any)
			if providers == nil {
				providers = map[string]any{}
				root["providers"] = providers
			}
			llm, _ := providers["llm"].(map[string]any)
			if llm == nil {
				llm = map[string]any{}
				providers["llm"] = llm
			}
			order := storedProviderOrder(providers, llm)
			llm[id] = element
			providers["llm_order"] = append(order, id)
		},
	},
	EntityMCPServers: {
		noun: "tool server",
		ids: func(c *config.Company) []string {
			out := make([]string, 0, len(c.MCPServers))
			for i := range c.MCPServers {
				out = append(out, c.MCPServers[i].Name)
			}
			return sorted(out)
		},
		find: func(c *config.Company, id string) (any, bool) {
			for i := range c.MCPServers {
				if c.MCPServers[i].Name == id {
					return &c.MCPServers[i], true
				}
			}
			return nil, false
		},
		replace: func(c *config.Company, id string, raw submitted) error {
			for i := range c.MCPServers {
				if c.MCPServers[i].Name != id {
					continue
				}
				incoming, err := decodeEntity[config.MCPServer](raw, config.Path{"mcp_servers", i})
				if err != nil {
					return err
				}
				// The name is the key a seat declares this server's
				// credentials under and the prefix its tools carry, so a
				// rename here silently unhooks every seat that named it.
				if incoming.Name != id {
					return identityMismatch("name", id, incoming.Name)
				}
				c.MCPServers[i] = incoming
				return nil
			}
			return ErrNoSuchEntity
		},
		stored: func(root map[string]any, id string) (map[string]any, bool) {
			for _, server := range objects(root["mcp_servers"]) {
				if name, _ := server["name"].(string); name == id {
					return server, true
				}
			}
			return nil, false
		},
		// APPENDED, so every server already declared keeps its position:
		// the prompt lists servers in declaration order, and an add that
		// reordered the list would move every seat's tool block.
		create: func(c *config.Company, id string, raw submitted) error {
			for i := range c.MCPServers {
				if c.MCPServers[i].Name == id {
					return ErrEntityExists
				}
			}
			incoming, err := decodeEntity[config.MCPServer](raw, config.Path{"mcp_servers", len(c.MCPServers)})
			if err != nil {
				return err
			}
			if incoming.Name != id {
				return identityMismatch("name", id, incoming.Name)
			}
			c.MCPServers = append(c.MCPServers, incoming)
			return nil
		},
		place: func(root map[string]any, _ string, element map[string]any) {
			list, _ := root["mcp_servers"].([]any)
			root["mcp_servers"] = append(list, element)
		},
	},
}

// inOrgChart reports whether a collection is part of the org chart, whose
// routes its leads may take as well as admin.
func inOrgChart(kind string) bool { return kind == EntityRoles || kind == EntityUnits }

// EntityKinds names every addressable collection, sorted — so a caller can
// discover the surface rather than carrying its own copy of this list.
func EntityKinds() []string {
	out := slices.Collect(maps.Keys(entityKinds))
	return sorted(out)
}

// Entities lists the identities in one collection of the active revision.
func (s *Service) Entities(ctx context.Context, kind string) ([]string, error) {
	access, ok := entityKinds[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q isn't one of %s",
			ErrUnknownEntityKind, kind, words.Or(EntityKinds()))
	}
	company, err := s.Document(ctx)
	if err != nil {
		return nil, err
	}
	ids := access.ids(company)
	if ids == nil {
		// An EMPTY collection, not a missing one. A company with no MCP
		// servers is a real company, and null here would render as a
		// failure.
		ids = []string{}
	}
	return ids, nil
}

// Entity reads one entity out of the active revision, redacted like the
// document it came from.
func (s *Service) Entity(ctx context.Context, kind, id string) (any, error) {
	access, ok := entityKinds[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q isn't one of %s",
			ErrUnknownEntityKind, kind, words.Or(EntityKinds()))
	}
	// REDACTED, because Document redacts: this is a slice of the same
	// document and a per-entity read that skipped the masking would be a
	// way to fetch every credential in the company one entity at a time.
	company, err := s.Document(ctx)
	if err != nil {
		return nil, err
	}
	entity, found := access.find(company, id)
	if !found {
		return nil, fmt.Errorf("%w: %s/%s", ErrNoSuchEntity, kind, id)
	}
	return entity, nil
}

// getEntity serves GET /config/{kind}/{id} — one entity, redacted.
//
// THE ENTITY ITSELF, not an envelope around it. The read has to be the thing
// the write takes: a caller who fetches, edits one field and sends it back is
// the loop this surface exists for, and a {kind, id, entity} wrapper puts a
// `jq` step in the middle of it. The websocket query keeps its envelope —
// there the kind and id are the answer to a question, not the resource.
//
// Redacted for the same reason [Service.Entity] is: a per-entity read that
// skipped the masking would fetch every credential in the company one entity
// at a time, past the masking the document read applies.
func (s *Service) getEntity(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		company, _, err := s.documentOf(r.Context())
		switch {
		case errors.Is(err, ErrNoActiveRevision):
			refuseNoCompany(w)
			return
		case err != nil:
			s.fail(w, r, "read the active revision", err)
			return
		}
		// A SEAT OR A UNIT IS READ BY ITS LEAD as well as by the admin
		// who reads the whole document, decided on where it sits — and
		// BEFORE it is looked for: an id the revision does not hold is
		// decided at the root, so only an admin is told it is missing.
		if inOrgChart(kind) && !s.mayReach(w, r, authz.ActionOrgRead, company, kind, id) {
			return
		}
		entity, found := entityKinds[kind].find(company, id)
		if !found {
			httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchEntity,
				map[string]string{"hint": NotFound(kind, id)})
			return
		}
		// THE ENTITY'S OWN TAG ([entityTag]), which a caller sends straight
		// back as If-Match on the write: a precondition on what they read,
		// not on the whole document.
		tag, err := entityTag(kind, entity)
		if err != nil {
			s.fail(w, r, "tag the entity", err)
			return
		}
		w.Header().Set("ETag", tag)
		if matchesTag(r.Header.Get("If-None-Match"), tag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeJSON(w, http.StatusOK, entity)
	}
}

// putEntity replaces one entity and stores the resulting document.
//
// Through the entity draft ([entityDraft]), with the refusals an HTTP caller
// needs spelled out: which entity was missing, which one a create found, and
// why a rename is not an edit.
//
// With `dry_run=true` it is the same request, checked in the same order, that
// stores and activates nothing — exactly as the whole-document writes are.
// An entity write needs its check MORE than they do, not less: its caller
// never sees the rest of the document, so the whole-company validation behind
// the splice is the only place it can learn that a provider fine on its own
// leaves the company invalid, or that a ceiling it raised now sits above the
// company's own (a warning, which only a check can show before the save).
// One answer differs from the write's: a seat or unit sent back unchanged is
// checked for its lead, where the write is refused as a re-publish
// (admission.go).
func (s *Service) putEntity(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.refusedManaged(w, r) {
			return
		}
		id := r.PathValue("id")
		dryRun, ok := dryRunOf(w, r)
		if !ok {
			return
		}
		body, err := readBody(w, r)
		if err != nil {
			refuseBody(w, err)
			return
		}
		summary, sent, ok := takeSummary(w, r, body)
		if !ok {
			return
		}
		expected, create, ok := entityPrecondition(w, r)
		if !ok {
			return
		}
		principal, ok := principalOf(w, r)
		if !ok {
			return
		}
		write := entityWrite{kind: kind, id: id, sent: sent, summary: summary,
			dryRun: dryRun, create: create, expected: expected, principal: principal,
			by: attributionOf(r)}
		var raced error
		for range entityAttempts {
			done, writeErr := s.writeEntity(w, r, write)
			var lost *RacedError
			if errors.As(writeErr, &lost) {
				raced = writeErr
				continue
			}
			if done {
				return
			}
		}
		s.refuseApply(w, r, raced)
	}
}

// entityAttempts is how many times an entity write is tried when the company
// document moves under it for a reason that is not the entity: the charter's
// count ([charterAttempts]), for the charter's reason. The precondition is
// the entity's, so a revision another writer activated between this write's
// read and its compare-and-set (a seat added beside the one edited, a
// credential sealed) is no conflict the caller can resolve, and each attempt
// judges the precondition again against the revision active then.
const entityAttempts = charterAttempts

// entityWrite is one entity write, as every attempt makes it.
type entityWrite struct {
	kind, id  string
	sent      submitted
	summary   string
	dryRun    bool
	create    bool
	expected  string
	principal *iam.Principal
	by        iam.Actor
}

// writeEntity makes one attempt: it reads the fleet's revision now, decides
// the caller's reach and the entity's precondition against it, and writes onto
// it. done reports that the request has been answered; a [*RacedError] is a
// revision that moved meanwhile, which the caller tries again.
func (s *Service) writeEntity(w http.ResponseWriter, r *http.Request, e entityWrite) (done bool, err error) {
	ctx := r.Context()
	cur, found, err := s.current(ctx)
	if err != nil {
		s.fail(w, r, "read the fleet's revision", err)
		return true, nil
	}
	active := cur.revision
	if !found {
		// Nothing to splice into. Refused rather than treated as an empty
		// company: creating the first revision through an entity route
		// would build a company out of one provider or server.
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoActiveRevision,
			map[string]string{
				"hint": "the fleet runs no company to edit: create one " +
					"with PUT /config or `crewlet config import`",
			})
		return true, nil
	}
	company, err := s.open(cur)
	if err != nil {
		if !RefuseUnreadable(w, err) {
			s.fail(w, r, "open the active revision", err)
		}
		return true, nil
	}
	// A SEAT OR A UNIT IS WRITTEN BY ITS LEAD as well as by an admin, and
	// decided as a read of it is: where it sits, BEFORE the id is looked up
	// or the body decoded, so an id outside the caller's subtree and one the
	// revision does not hold are refused in the same bytes.
	if inOrgChart(e.kind) && !s.mayReach(w, r, authz.ActionOrgWrite, company, e.kind, e.id) {
		return true, nil
	}
	// THE PRECONDITION IS THE ENTITY'S, judged against what GET serves for
	// it now (redacted, as the caller read it). An entity the revision does
	// not hold is left to the draft, which refuses it in its own words.
	if e.expected != "" && !e.create {
		if current, held := entityKinds[e.kind].find(company.Redact(), e.id); held {
			tag, tagErr := entityTag(e.kind, current)
			if tagErr != nil {
				s.fail(w, r, "tag the entity", tagErr)
				return true, nil
			}
			if !matchesTag(e.expected, tag) {
				// 412, the answer RFC 9110 gives a failed If-Match, with the
				// entity as it stands, so a caller can show the change beside
				// what replaced it and resend with this tag to keep theirs.
				w.Header().Set("ETag", tag)
				httpjson.FailWithFields(w, http.StatusPreconditionFailed,
					httpjson.CodeVersionMoved, httpjson.Detail{
						"current": current, "etag": tag,
						"hint": e.kind + "/" + e.id + " changed after you read it; " +
							"read it again and make the change on top of it",
					})
				return true, nil
			}
		}
	}
	d, err := entityDraft(e.kind, e.id, e.sent, active.ID, e.create)
	if err != nil {
		// A create of a kind that has none is the caller's request to
		// correct, answered like every other entity refusal.
		s.refuseEntity(w, r, e.kind, e.id, err)
		return true, nil
	}
	d.principal = e.principal
	d.addressedCheck = e.dryRun && inOrgChart(e.kind)
	d.by = e.by
	prepared, err := s.prepare(ctx, d)
	var lost *RacedError
	switch {
	case errors.As(err, &lost):
		return false, err
	case err != nil:
		s.refuseEntity(w, r, e.kind, e.id, err)
		return true, nil
	}
	if e.dryRun {
		writeChecked(w, prepared)
		return true, nil
	}
	// The same rule the whole-document write has, and for the same reason,
	// asked as late: a per-entity write can say more, so the hint does.
	if !requireSummary(w, e.summary, "this write needs an audit summary: "+
		"the X-Summary header, or a top-level _summary key in the body. "+
		"Name what changed about "+e.kind+"/"+e.id) {
		return true, nil
	}
	applied, err := s.commit(ctx, prepared, e.summary)
	switch {
	case errors.As(err, &lost):
		return false, err
	case err != nil:
		s.refuseApply(w, r, err)
		return true, nil
	}
	writeApplied(w, applied)
	return true, nil
}

// entityTag is the strong entity-tag of one entity: a digest of what GET
// serves for it, redacted, prefixed with its kind.
//
// THE ENTITY, NOT THE REVISION, as the charter's tag is ([charterTag]): the
// precondition a caller editing one seat holds is "this seat is still what I
// read", and a revision that moved because a colleague added another seat,
// sealed a credential or changed a ceiling elsewhere is no conflict about
// anything they sent. Tagged with the document's revision, every such write
// was refused 409 revision_advanced, and the Dashboard Assistant's proposal
// of one seat's budget went stale when an unrelated seat was added.
func entityTag(kind string, entity any) (string, error) {
	encoded, err := json.Marshal(entity)
	if err != nil {
		return "", fmt.Errorf("configapi: encode %s for its tag: %w", kind, err)
	}
	sum := sha256.Sum256(encoded)
	return `"` + kind + "-" + hex.EncodeToString(sum[:16]) + `"`, nil
}

// entityPrecondition reads an entity write's condition: the tag an edit was
// read at (If-Match, the entity's own [entityTag], or `*` for whatever it
// holds now), or a create (`If-None-Match: *`).
//
// `If-None-Match: *` AT AN ENTITY'S ADDRESS IS ABOUT THE ENTITY (RFC 9110
// §13.1.2: "only if there is no current representation" of the TARGET
// resource). Handed to the document's own precondition it was read as "only
// if no company is configured", a condition that can never hold on an entity
// route, which needs a company to splice into. It is the create-only write
// here, and never beside If-Match: at an address with no representation yet
// the two conditions describe two different resources. Any other
// If-None-Match is no condition this route takes.
func entityPrecondition(w http.ResponseWriter, r *http.Request) (expected string, create, ok bool) {
	expected = strings.TrimSpace(r.Header.Get("If-Match"))
	switch none := strings.TrimSpace(r.Header.Get("If-None-Match")); {
	case none == "":
		return expected, false, true
	case none != "*":
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadIfMatch,
			map[string]string{
				"hint": "an entity write takes If-Match with the ETag its GET answered, " +
					"or If-None-Match: * to create one",
			})
		return "", false, false
	case expected != "":
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConflictingPreconditions,
			map[string]string{
				"hint": "If-None-Match: * asks for a create and If-Match names the " +
					"version an edit was read at; send one. A create is checked " +
					"against the revision active when it lands",
			})
		return "", false, false
	}
	return "", true, true
}

// howToAdd is how a member of kind is added, the one sentence both a plain PUT
// to a missing id and a create of an uncreatable one end on: the create-only
// write where the collection creates by address, and where it does not (a
// seat, a unit), the write that shows the place the path cannot name.
//
// ONE SENTENCE FOR BOTH, because the first used to tell every caller to send
// `If-None-Match: *`, which a seat and a unit then refuse `not_creatable`: a
// caller following the hint for a seat met a second refusal before it was
// told where a seat is added.
//
// [EntityAbsent] declares the same fact on the route itself.
func howToAdd(kind string) string {
	access := entityKinds[kind]
	if access.create != nil {
		return "to add it, send the same request with If-None-Match: *"
	}
	return "a " + nounOf(kind) + " is added by replacing the unit it sits in " +
		"(PUT /config/units/{id}), or through PUT /config, where its place in the " +
		"chart is visible"
}

// EntityAbsent is what a PUT to kind's address does where it holds nothing,
// as the route is mounted with it ([authz.Absent]): a create where the kind
// creates by address, and otherwise refused with [howToAdd]'s sentence, which
// is what the handler answers such a create. One table decides both.
func EntityAbsent(kind string) *authz.Absent {
	if entityKinds[kind].create != nil {
		return authz.CreatesWhenAbsent()
	}
	// As the refusal's envelope spells it, so the two read alike.
	return authz.AddedElsewhere(httpjson.Sentence(howToAdd(kind)))
}

// NotFound says that nothing in kind is called id, as a person reads it: the
// collection by what one of its members is called ("No tool server is called
// \"zzz\""), never by its URL segment. EXPORTED for the query surface's
// config_entities, which answers the same miss in the same words.
func NotFound(kind, id string) string {
	return fmt.Sprintf("no %s is called %q", nounOf(kind), id)
}

// nounOf is what one member of kind is called, as a person names it.
func nounOf(kind string) string {
	if noun := entityKinds[kind].noun; noun != "" {
		return noun
	}
	return "entry"
}

// refuseEntity answers an entity write's own refusals, and every other one as
// [Service.refuseApply] does.
func (s *Service) refuseEntity(w http.ResponseWriter, r *http.Request, kind, id string, err error) {
	var entityErr *EntityError
	var mismatch *MismatchError
	switch {
	case !errors.As(err, &entityErr):
		s.refuseApply(w, r, err)
	case errors.Is(err, ErrNoSuchEntity):
		// NEVER CREATED. A PUT naming an id nothing carries is far more
		// often a typo than an intent to add one, and adding through this
		// route would let a caller grow the company without ever seeing the
		// document they changed.
		//
		// AND THE WAY TO ADD ONE IS THE COLLECTION'S: a seat or a unit is
		// never created by id ([ErrNotCreatable]), so telling a caller to
		// send If-None-Match: * for one sent them to a second refusal.
		hint := NotFound(kind, id) + ". " + httpjson.Sentence(howToAdd(kind))
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchEntity,
			map[string]string{"hint": hint})
	case errors.Is(err, ErrEntityExists):
		// A CREATE THAT FOUND ONE. 412 because it is the precondition the
		// caller sent that failed, not the document.
		httpjson.FailWith(w, http.StatusPreconditionFailed, httpjson.CodeEntityExists,
			map[string]string{
				"hint": "If-None-Match: * asked for " + kind + "/" + id + " to be " +
					"added, and the active revision already has one: pick another " +
					"name, or edit that one under If-Match",
			})
	case errors.Is(err, ErrNotCreatable):
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeNotCreatable,
			map[string]string{"hint": howToAdd(kind)})
	case errors.As(err, &mismatch):
		// A RENAME, REFUSED. Not coerced back to the path's id either:
		// silently keeping the old identity would land every other edit in
		// the body and leave the caller believing the rename took, which is
		// the same surprise one revision later.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeIdentityMismatch, map[string]string{
			"detail": mismatch.Sentence(),
			"hint": "the path is the address: send " + kind + "/" + id +
				" back under the id it already has. A seat's handle and a " +
				"unit's key are permanent — a seat's durable id derives from " +
				"its handle — and an MCP server's name or a provider's key is " +
				"named by everything that uses it, so a rename is a removal " +
				"and a creation made through PUT /config, where everything " +
				"that names it is visible",
		})
	default:
		// A BODY THIS KIND CANNOT READ, which is the same refusal the
		// whole-document write gives a document it cannot read.
		refuseDocument(w, httpjson.CodeInvalidBody, entityErr.Err.Error(), "",
			&DocumentError{Err: entityErr.Err})
	}
}

// firstElement is the first seat, or unit, of a stored document tree whose
// identity is id, visited in the order [config.Company.EachRole] and
// [config.Company.EachUnit] visit the struct, so it is the element find
// returned.
func firstElement(root map[string]any, isUnit bool, id string) (map[string]any, bool) {
	var found map[string]any
	consider := func(element map[string]any, unit bool) {
		if found != nil || unit != isUnit {
			return
		}
		if identityOfElement(element, unit) == id {
			found = element
		}
	}
	for _, seat := range objects(root["roles"]) {
		consider(seat, false)
	}
	var visit func(map[string]any)
	visit = func(unit map[string]any) {
		consider(unit, true)
		for _, seat := range objects(unit["roles"]) {
			consider(seat, false)
		}
		for _, child := range objects(unit["children"]) {
			visit(child)
		}
	}
	for _, unit := range objects(root["units"]) {
		visit(unit)
	}
	return found, found != nil
}

// identityOfElement is a stored seat's or unit's identity as the config model
// derives it, and empty for an element this build cannot read as one.
func identityOfElement(element map[string]any, isUnit bool) string {
	raw, err := json.Marshal(element)
	if err != nil {
		return ""
	}
	if isUnit {
		var unit config.Unit
		if json.Unmarshal(raw, &unit) != nil {
			return ""
		}
		return unit.IdentityKey()
	}
	var role config.Role
	if json.Unmarshal(raw, &role) != nil {
		return ""
	}
	return role.IdentityKey()
}

// objects is the object elements of a list, and nothing for anything else.
func objects(value any) []map[string]any {
	list, _ := value.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if object, ok := item.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}

// roleID is a seat's address here: its handle — declared, since every stored
// seat carries one ([config.MintIdentities]) — so the id in this URL is the
// handle every other surface shows.
func roleID(r *config.Role) string { return r.Seat().Handle() }

func sorted(in []string) []string {
	slices.Sort(in)
	return in
}

// decodeEntity decodes one entity body STRICTLY, and places every failure
// where the entity sits in the document, at.
//
// Unknown fields are refused, which is the same rule Tier B's document parser
// has and for the same reason: a mistyped setting that silently did nothing is
// the failure this build refuses to have. `json.Unmarshal` does the opposite:
// it drops what it does not recognise, so a `PUT /config/llm-providers/zulu`
// carrying `"modell"` would answer 201 and store the provider with its model
// silently gone. This is the surface most likely to be hand-edited in a
// hurry, so it is the worst place to accept a typo quietly.
//
// READ BY THE DOCUMENT'S OWN READER ([config.ParseMember]) and placed in the
// document, because a refusal's problems are located in the document the
// entity is spliced into, like the validation that follows. A strict JSON
// decoder refused the same typo naming the key and no place, as a problem of
// no kind, so it could not be put beside the field it was about.
//
// It also closes the one hole a whole-document write does not have: the body
// key that carries a revision summary is lifted out before this runs, and a
// route that forgot to lift it would be caught here rather than storing it.
func decodeEntity[T any](raw submitted, at config.Path) (T, error) {
	var out T
	read := func() error { return config.ParseMember(raw.text, &out) }
	if raw.doc != nil {
		read = func() error { return config.ParseMemberNode(raw.doc, &out) }
	}
	if err := read(); err != nil {
		var zero T
		eachFault(err, func(f *config.Fault) {
			f.Path = append(slices.Clone(at), f.Path...)
		})
		return zero, err
	}
	return out, nil
}

// storedProviderOrder is the provider order a STORED document's providers
// block resolves to, read off its tree: its `llm_order` reconciled against the
// keys of its `llm` map by [config.Providers.ProviderOrder] itself, so a
// stored order and a decoded one can never disagree about where a key sits.
func storedProviderOrder(providers, llm map[string]any) []any {
	shape := config.Providers{LLM: make(map[string]config.LLMProvider, len(llm))}
	for key := range llm {
		shape.LLM[key] = config.LLMProvider{}
	}
	listed, _ := providers["llm_order"].([]any)
	for _, key := range listed {
		if name, ok := key.(string); ok {
			shape.LLMOrder = append(shape.LLMOrder, name)
		}
	}
	order := shape.ProviderOrder()
	out := make([]any, 0, len(order)+1)
	for _, key := range order {
		out = append(out, key)
	}
	return out
}
