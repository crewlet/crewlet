package configapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/crewlet/crewlet/internal/config"
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
// # It is the same write underneath
//
// An entity PUT is not a patch protocol. It opens the active revision,
// SPLICES the entity in, restores masks against that same revision, validates
// the WHOLE document and stores a new revision — identical to PUT /config
// from that line on. A change that would leave the company invalid is refused
// even when the entity itself is fine, because a seat naming a provider that
// no longer exists is exactly the kind of break a per-entity surface invites.
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

// entityAccess is how one collection is listed, read and replaced.
//
// Typed rather than a JSON path grammar, deliberately: a path grammar would
// be a second description of the config's shape, free to drift from the Go
// types the loader and the validator actually use.
type entityAccess struct {
	// ids lists the identities in the document, in a stable order.
	ids func(*config.Company) []string
	// find returns the entity under an id, and whether it was there.
	find func(*config.Company, string) (any, bool)
	// replace splices a decoded entity in under an id, or reports why not.
	// It never CREATES: an id that is not already there is refused, because
	// "PUT the entity called X" arriving for an X nobody has is far more
	// often a typo than an intent to add one.
	replace func(*config.Company, string, []byte) error
}

// entityKinds is the table, and the four keys are the paths the dashboard's
// Config room already addresses.
var entityKinds = map[string]entityAccess{
	EntityRoles: {
		ids: func(c *config.Company) []string {
			var out []string
			eachRole(c, func(r *config.Role) { out = append(out, roleID(r)) })
			return sorted(out)
		},
		find: func(c *config.Company, id string) (any, bool) {
			var found *config.Role
			eachRole(c, func(r *config.Role) {
				if found == nil && roleID(r) == id {
					found = r
				}
			})
			if found == nil {
				return nil, false
			}
			return found, true
		},
		replace: func(c *config.Company, id string, raw []byte) error {
			var incoming config.Role
			if err := json.Unmarshal(raw, &incoming); err != nil {
				return fmt.Errorf("decode role: %w", err)
			}
			var done bool
			eachRole(c, func(r *config.Role) {
				if done || roleID(r) != id {
					return
				}
				// THE IDENTITY IS THE ADDRESS, so a body that renames the
				// seat is refused rather than silently moved: the caller
				// asked to replace the entity at this id, and honouring a
				// rename here would leave the URL naming something that no
				// longer exists.
				*r = incoming
				done = true
			})
			if !done {
				return ErrNoSuchEntity
			}
			return nil
		},
	},
	EntityUnits: {
		ids: func(c *config.Company) []string {
			var out []string
			eachUnit(c, func(u *config.Unit) { out = append(out, u.Name) })
			return sorted(out)
		},
		find: func(c *config.Company, id string) (any, bool) {
			var found *config.Unit
			eachUnit(c, func(u *config.Unit) {
				if found == nil && u.Name == id {
					found = u
				}
			})
			if found == nil {
				return nil, false
			}
			return found, true
		},
		replace: func(c *config.Company, id string, raw []byte) error {
			var incoming config.Unit
			if err := json.Unmarshal(raw, &incoming); err != nil {
				return fmt.Errorf("decode unit: %w", err)
			}
			var done bool
			eachUnit(c, func(u *config.Unit) {
				if done || u.Name != id {
					return
				}
				*u = incoming
				done = true
			})
			if !done {
				return ErrNoSuchEntity
			}
			return nil
		},
	},
	EntityLLMProviders: {
		ids: func(c *config.Company) []string {
			out := make([]string, 0, len(c.Providers.LLM))
			for key := range c.Providers.LLM {
				out = append(out, key)
			}
			return sorted(out)
		},
		find: func(c *config.Company, id string) (any, bool) {
			p, ok := c.Providers.LLM[id]
			if !ok {
				return nil, false
			}
			return p, true
		},
		replace: func(c *config.Company, id string, raw []byte) error {
			if _, ok := c.Providers.LLM[id]; !ok {
				return ErrNoSuchEntity
			}
			var incoming config.LLMProvider
			if err := json.Unmarshal(raw, &incoming); err != nil {
				return fmt.Errorf("decode llm provider: %w", err)
			}
			c.Providers.LLM[id] = incoming
			return nil
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
		replace: func(c *config.Company, id string, raw []byte) error {
			for i := range c.MCPServers {
				if c.MCPServers[i].Name != id {
					continue
				}
				var incoming config.MCPServer
				if err := json.Unmarshal(raw, &incoming); err != nil {
					return fmt.Errorf("decode mcp server: %w", err)
				}
				c.MCPServers[i] = incoming
				return nil
			}
			return ErrNoSuchEntity
		},
	},
}

// EntityKinds names every addressable collection, sorted — so a caller can
// discover the surface rather than carrying its own copy of this list.
func EntityKinds() []string {
	out := make([]string, 0, len(entityKinds))
	for kind := range entityKinds {
		out = append(out, kind)
	}
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
		// An EMPTY collection, not a missing one. A company with no units
		// is a real company, and null here would render as a failure.
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
	// way to fetch every credential in the company one seat at a time.
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

// putEntity replaces one entity and stores the resulting document.
func (s *Service) putEntity(kind string) http.HandlerFunc {
	access := entityKinds[kind]
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		body, err := readBody(w, r)
		if err != nil {
			refuseBody(w, err)
			return
		}
		summary, body, err := splitSummary(body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid_body", "detail": err.Error(),
			})
			return
		}
		if header := r.Header.Get("X-Summary"); header != "" {
			summary = header
		}
		if summary == "" {
			// The same rule the whole-document write has, and for the same
			// reason: a list of revisions with no summaries is a list of
			// uuids. A per-entity write can say more, so the hint does.
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "summary_required",
				"hint": "this write needs an audit summary — the X-Summary header, " +
					"or a top-level _summary key in the body. Name what changed " +
					"about " + kind + "/" + id,
			})
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
			// route would build a company out of one seat.
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "no_active_revision",
				"hint": "this node has no active company revision to edit — " +
					"import one with `crewlet config import`",
			})
			return
		}
		if !s.checkPrecondition(w, r, active, found) {
			return
		}
		prior, err := s.open(active)
		if err != nil {
			s.fail(w, "open the active revision", err)
			return
		}

		// SPLICED INTO A COPY OF THE ACTIVE DOCUMENT, so everything the
		// caller did not send is exactly what is already stored — which is
		// the entire difference between this and PUT /config.
		spliced, err := s.open(active)
		if err != nil {
			s.fail(w, "open the active revision", err)
			return
		}
		switch err := access.replace(spliced, id, body); {
		case errors.Is(err, ErrNoSuchEntity):
			// NEVER CREATED. A PUT naming an id nothing carries is far more
			// often a typo than an intent to add one, and adding through
			// this route would let a caller grow the company without ever
			// seeing the document they changed.
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "no_such_entity",
				"hint": "no " + kind + " called " + id + " in the active revision; " +
					"add one through PUT /config, which shows the whole document",
			})
			return
		case err != nil:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid_body", "detail": err.Error(),
			})
			return
		}

		// The masks the caller was shown come back as the values they hide,
		// against the revision they were shown FROM.
		spliced.RestoreRedacted(prior)
		// VALIDATED WHOLE, not just the entity. A seat naming a provider
		// that no longer exists is valid on its own and breaks the company,
		// and a per-entity surface is exactly where that gets introduced.
		if err := spliced.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "validation_error", "detail": err.Error(),
			})
			return
		}
		s.store(w, r, spliced, active.ID, summary)
	}
}

// --- walking the document -------------------------------------------------

// eachRole visits every seat in the company, root-level and unit-nested
// alike, because an operator editing "the CEO" does not think about which
// list it happens to live in.
func eachRole(c *config.Company, visit func(*config.Role)) {
	for i := range c.Roles {
		visit(&c.Roles[i])
	}
	for i := range c.Units {
		eachUnitRole(&c.Units[i], visit)
	}
}

func eachUnitRole(u *config.Unit, visit func(*config.Role)) {
	for i := range u.Roles {
		visit(&u.Roles[i])
	}
	for i := range u.Children {
		eachUnitRole(&u.Children[i], visit)
	}
}

// eachUnit visits every unit, nesting to any depth.
func eachUnit(c *config.Company, visit func(*config.Unit)) {
	for i := range c.Units {
		visitUnit(&c.Units[i], visit)
	}
}

func visitUnit(u *config.Unit, visit func(*config.Unit)) {
	visit(u)
	for i := range u.Children {
		visitUnit(&u.Children[i], visit)
	}
}

// roleID is a seat's address here: its handle, derived when the document
// leaves it out — the same derivation the org model uses, so the id in this
// URL is the handle every other surface shows.
func roleID(r *config.Role) string { return r.Seat().Handle() }

func sorted(in []string) []string {
	sort.Strings(in)
	return in
}
