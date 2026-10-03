package configapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
)

// The config's addressable collections — what the dashboard's Config room
// edits one at a time instead of sending the whole document back.
//
// # Why per entity at all, when PUT /config exists
//
// The whole-document write is the honest primitive and it stays. But it makes
// every edit a company-wide one: a founder changing one provider's model sends
// back a document carrying every other provider, every MCP server and every
// integration, and a concurrent edit anywhere in it is theirs to lose. Editing
// one entity narrows what a write claims to have changed, which is what makes
// the revision summary mean something and what makes two people editing
// different parts of the company safe.
//
// Why not a patch format that addresses list members instead — RFC 6902, or a
// merge key — is the question this shape invites, and the answer is worth
// stating rather than leaving to be re-derived: a patch addresses by
// STRUCTURE, and a name is deliberately not structural. A server's position in
// `mcp_servers` is not its identity, so a patch that named it by index would
// rewrite a different server the moment one above it was removed.
//
// # It is the same write underneath
//
// An entity PUT is not a patch protocol. It opens the active revision,
// SPLICES the entity in, restores masks against that same revision, validates
// the WHOLE document and stores a new revision — identical to PUT /config
// from that line on. A change that would leave the company invalid is refused
// even when the entity itself is fine, because a worker template naming a
// provider that no longer exists is exactly the kind of break a per-entity
// surface invites. What the revision does not hold it cannot check: a seat's
// model chain is the org chart's, and only `crewlet config import`, which
// holds both halves of the company file, validates the two together — see
// [ErrIdentityMismatch].
//
// # The org chart is not addressed here at all
//
// A seat and a unit are the org chart's: a domain of its own, with its own
// records, its own per-object arbitration and its own routes (`/chart/*`),
// and no revision carries either — the import divides the company file, and
// the whole-document door refuses a body naming them (chartdoor.go). So there
// is no `roles` or `units` collection to list, read or write, and a path
// naming one is a route this surface does not serve, answered by the mux's
// own `404 no_route` like any other.
const (
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

// ErrIdentityMismatch reports a body whose own identity disagrees with the id
// in the path — a rename, arriving dressed as a replacement.
//
// Refused rather than applied, because an identity here is not a label: it is
// what the rest of the company refers to the entity by. An MCP server's name
// keys every `mcp_env` block that names it, a seat's or a unit's, and prefixes
// every tool it serves; a provider's key is what every model chain naming it
// holds — a seat's, and a worker template's `model`. None of that moves with
// a splice, so a rename here would unhook everything that named the old
// identity, and leave the URL naming something that no longer exists.
//
// AND THIS ROUTE CANNOT MOVE IT. Most of what names a server or a provider is
// the org chart's — a seat's and a unit's runtime half — which a revision does
// not carry, so not even a whole-document `PUT /config` can see it. The one
// write that holds both halves at once is the authored company file: renamed
// there, together with everything that names it, `crewlet config import`
// validates the two as one company before it writes either.
var ErrIdentityMismatch = errors.New("configapi: identity mismatch")

// identityMismatch names both halves, because the caller has to be able to
// see which one they meant.
func identityMismatch(field, pathID, bodyID string) error {
	if bodyID == "" {
		return fmt.Errorf("%w: this path addresses %q, and the body carries no %s",
			ErrIdentityMismatch, pathID, field)
	}
	return fmt.Errorf("%w: this path addresses %q, but the body's %s is %q",
		ErrIdentityMismatch, pathID, field, bodyID)
}

// entityAccess is how one collection is listed, read, replaced and added to.
//
// Typed rather than a JSON path grammar, deliberately: a path grammar would
// be a second description of the config's shape, free to drift from the Go
// types the loader and the validator actually use.
//
// EVERY MEMBER IS REQUIRED, for every collection: a collection is addressed
// here only if a member of it can be read, replaced and added BY ITS ADDRESS
// alone — a provider by its key, a server by its name. A collection whose
// members have a PLACE the path cannot name (the unit a seat sits in, its
// position among its siblings) is the org chart's, which this surface does
// not address at all. A table test holds every entry to every member, so a
// collection cannot join the table without saying how a member of it is
// written — the nil it would otherwise carry is a panic inside a request.
type entityAccess struct {
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
	// for a body naming another.
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
	EntityLLMProviders: {
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
		create: func(c *config.Company, id string, raw submitted) error {
			if _, ok := c.Providers.LLM[id]; ok {
				return ErrEntityExists
			}
			incoming, err := decodeEntity[config.LLMProvider](raw, config.Path{"providers", "llm", id})
			if err != nil {
				return err
			}
			if c.Providers.LLM == nil {
				c.Providers.LLM = map[string]config.LLMProvider{}
			}
			c.Providers.LLM[id] = incoming
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
			llm[id] = element
		},
	},
	EntityMCPServers: {
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
		return nil, fmt.Errorf("%w: %q (want one of %v)",
			ErrUnknownEntityKind, kind, EntityKinds())
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
		return nil, fmt.Errorf("%w: %q (want one of %v)",
			ErrUnknownEntityKind, kind, EntityKinds())
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
		company, revision, err := s.documentOf(r.Context())
		switch {
		case errors.Is(err, ErrNoActiveRevision):
			httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoActiveRevision,
				map[string]string{
					"hint": "this node has no active company revision to read",
				})
			return
		case err != nil:
			s.fail(w, "read the active revision", err)
			return
		}
		entity, found := entityKinds[kind].find(company, id)
		if !found {
			httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchEntity,
				map[string]string{
					"hint": "no " + kind + " called " + id + " in the active revision",
				})
			return
		}
		// THE DOCUMENT'S TAG, because an entity is a slice of it: the
		// entity changes when the revision does, and a caller who holds
		// this tag can send it straight back as If-Match on the write.
		if serveConditional(w, r, revision) {
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
func (s *Service) putEntity(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		// The same rule the whole-document write has, and for the same
		// reason: a list of revisions with no summaries is a list of
		// uuids. A per-entity write can say more, so the hint does. A
		// check stores nothing, so it needs none.
		summary, sent, ok := takeSummary(w, r, body, !dryRun,
			"this write needs an audit summary: the X-Summary header, "+
				"or a top-level _summary key in the body. Name what changed "+
				"about "+kind+"/"+id)
		if !ok {
			return
		}

		active, found, err := s.configs.Active(r.Context())
		if err != nil {
			s.fail(w, "read the active revision", err)
			return
		}
		if !found {
			// Nothing to splice into. Refused rather than treated as an
			// empty company: creating the first revision through an entity
			// route would build a company out of one provider or server.
			httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoActiveRevision,
				map[string]string{
					"hint": "this node has no active company revision to edit: " +
						"import one with `crewlet config import`",
				})
			return
		}
		create, ok := s.entityPrecondition(w, r, active, found)
		if !ok {
			return
		}
		d, err := entityDraft(kind, id, sent, active.ID, create)
		if err != nil {
			// A create of a kind that has none is the caller's request to
			// correct, answered like every other entity refusal.
			s.refuseEntity(w, kind, id, err)
			return
		}
		prepared, err := s.prepare(r.Context(), d)
		if err != nil {
			s.refuseEntity(w, kind, id, err)
			return
		}
		if dryRun {
			writeChecked(w, prepared)
			return
		}
		applied, err := s.commit(r.Context(), prepared, summary, attributionOf(r))
		if err != nil {
			s.refuseApply(w, err)
			return
		}
		writeApplied(w, applied)
	}
}

// entityPrecondition reads an entity write's condition, and reports whether
// it is a create.
//
// `If-None-Match: *` AT AN ENTITY'S ADDRESS IS ABOUT THE ENTITY (RFC 9110
// §13.1.2: "only if there is no current representation" of the TARGET
// resource). Handed to the document's own precondition it was read as "only
// if no company is configured" — a condition that can never hold on an entity
// route, which needs a company to splice into — so the one request that says
// "add this" was refused 412 `already_configured`, a sentence about something
// the caller never asked. It is the create-only write here.
//
// NOT BESIDE `If-Match`. That tag names the DOCUMENT's revision, and at an
// address that has no representation yet the two conditions describe two
// different resources; the create already lands on the revision active at the
// commit, compare-and-set, and is validated whole against it.
func (s *Service) entityPrecondition(w http.ResponseWriter, r *http.Request, active store.Revision, found bool) (create, ok bool) {
	if strings.TrimSpace(r.Header.Get("If-None-Match")) != "*" {
		_, ok := s.checkPrecondition(w, r, active, found)
		return false, ok
	}
	if strings.TrimSpace(r.Header.Get("If-Match")) != "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConflictingPreconditions,
			map[string]string{
				"hint": "If-None-Match: * asks for a create and If-Match names the " +
					"revision an edit was read from; send one. A create is checked " +
					"against the revision active when it lands",
			})
		return false, false
	}
	return true, true
}

// refuseEntity answers an entity write's own refusals, and every other one as
// [Service.refuseApply] does.
func (s *Service) refuseEntity(w http.ResponseWriter, kind, id string, err error) {
	var entityErr *EntityError
	switch {
	case !errors.As(err, &entityErr):
		s.refuseApply(w, err)
	case errors.Is(err, ErrNoSuchEntity):
		// NEVER CREATED. A PUT naming an id nothing carries is far more
		// often a typo than an intent to add one, and adding through this
		// route would let a caller grow the company without ever seeing the
		// document they changed.
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchEntity,
			map[string]string{
				"hint": "no " + kind + " called " + id + " in the active revision; " +
					"to add one, send the same request with If-None-Match: *",
			})
	case errors.Is(err, ErrEntityExists):
		// A CREATE THAT FOUND ONE. 412 because it is the precondition the
		// caller sent that failed, not the document.
		httpjson.FailWith(w, http.StatusPreconditionFailed, httpjson.CodeEntityExists,
			map[string]string{
				"hint": "If-None-Match: * asked for " + kind + "/" + id + " to be " +
					"added, and the active revision already has one: pick another " +
					"name, or edit that one under If-Match",
			})
	case errors.Is(err, ErrIdentityMismatch):
		// A RENAME, REFUSED. Not coerced back to the path's id either:
		// silently keeping the old identity would land every other edit in
		// the body and leave the caller believing the rename took, which is
		// the same surprise one revision later.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeIdentityMismatch, map[string]string{
			"detail": entityErr.Err.Error(),
			"hint": "the path is the address: send " + kind + "/" + id +
				" back under the id it already has. A rename has to move " +
				"everything that names it — an MCP server's name keys every " +
				"mcp_env block and prefixes its tools, and every model chain " +
				"names a provider by its key — and most of that is the org " +
				"chart's, which no write to /config can see: rename it in the " +
				"company file, with everything that names it, and import that " +
				"with crewlet config import, which validates both halves together",
		})
	default:
		// A BODY THIS KIND CANNOT READ, which is the same refusal the
		// whole-document write gives a document it cannot read.
		refuseDocument(w, httpjson.CodeInvalidBody, entityErr.Err.Error(), "",
			&DocumentError{Err: entityErr.Err})
	}
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
