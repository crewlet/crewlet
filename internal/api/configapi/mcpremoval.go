package configapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// Removing an MCP server: `DELETE /config/mcp-servers/{id}`.
//
// An `mcp_servers` entry is named by each seat's and unit's `mcp_env` block
// and each `sandbox.mcp.servers` entry; removed alone it leaves them naming
// nothing, which validation refuses ([config.Company.MCPServerReferences]).
// So the route takes the entry and, with `clear`, every such field as ONE
// write; without it a server anything names is refused naming all of it, each
// at its field, so the person sees the whole of what goes with it.
//
// A FIELD IS CLEARED WHOLE, never repointed. One the engine also reads by the
// server's name (a seat's `mcp_env.github`, its GitHub App token) stays
// ([config.Company.MCPServerDependants]).

// MCPRemovalRequest is the body `DELETE /config/mcp-servers/{id}` reads.
type MCPRemovalRequest struct {
	// Clear clears every field that names the server alone, in the same
	// revision. Without it, a server anything names is refused naming all of
	// it.
	Clear bool `json:"clear"`
	SecretsRequest
}

// MCPHolderKind is whose block an [MCPBlockDependant] is.
type MCPHolderKind string

const (
	// MCPHolderSeat is a seat's own block.
	MCPHolderSeat MCPHolderKind = "seat"
	// MCPHolderUnit is a unit's block, which its agent seats inherit.
	MCPHolderUnit MCPHolderKind = "unit"
)

// Valid reports whether k is one of the two holders.
func (k MCPHolderKind) Valid() bool { return k == MCPHolderSeat || k == MCPHolderUnit }

// MCPBlockDependant is one seat's or unit's block keyed on the server: an
// `mcp_env` block of its values, or an `mcp_limits` block holding it back.
type MCPBlockDependant struct {
	// Kind is whose block it is; Key is the seat's handle or the unit's key,
	// and Name its display name.
	Kind MCPHolderKind `json:"kind"`
	Key  string        `json:"key"`
	Name string        `json:"name"`
	// Segments is the written field: ["units", 0, "mcp_env", "linear"],
	// ["roles", 3, "mcp_limits", "linear"].
	Segments config.Path `json:"segments"`
}

// MCPSandboxDependant is one entry of a seat's `sandbox.mcp.servers` listing
// the server.
type MCPSandboxDependant struct {
	// Seat is the seat's handle, and Name its display name.
	Seat string `json:"seat"`
	Name string `json:"name"`
	// Segments is the written entry:
	// ["roles", 2, "sandbox", "mcp", "servers", 0].
	Segments config.Path `json:"segments"`
}

// MCPCleared is every field a removal of a server clears with it, in document
// order. No list is ever null.
type MCPCleared struct {
	MCPEnv  []MCPBlockDependant   `json:"mcp_env"`
	Sandbox []MCPSandboxDependant `json:"sandbox"`
	// MCPLimits is each seat's and unit's `mcp_limits` block for the
	// server, which holds nothing back once it is gone.
	MCPLimits []MCPBlockDependant `json:"mcp_limits"`
}

// MCPDependants is what a removal of an MCP server takes with it: the fields it
// clears, and the stored values the server and those fields read.
type MCPDependants struct {
	MCPCleared
	RemovalSecrets
	// OAuth is whether somebody holds an OAuth grant for the server, which
	// the removal revokes at its vendor where it offers that, and forgets.
	OAuth bool `json:"oauth"`
}

// MCPGrantRevocation is what a removal did with the server's OAuth grant: what
// its vendor was asked (`revoked`, `not_offered` where it publishes no
// revocation endpoint, `failed` where it could not be asked or refused) and,
// where it refused, its words. The grant is forgotten here whatever the
// vendor answered.
type MCPGrantRevocation struct {
	Revocation string `json:"revocation"`
	Said       string `json:"said"`
}

// MCPRemovalError is a removal that would leave a field naming the server it
// removes: the problems, each at its field, and what the removal takes with it.
type MCPRemovalError struct {
	Problems []config.Problem
	Cleared  MCPCleared
	// Before and After are what the document reads before the removal and,
	// at its paths, after it ([entryReads]): the refusal offers the values.
	Before, After []config.Reference
}

func (e *MCPRemovalError) Error() string {
	lines := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		lines = append(lines, p.Message)
	}
	return "configapi: " + strings.Join(lines, "; ")
}

// serverRemoval is `DELETE /config/mcp-servers/{id}`.
var serverRemoval = removalRoute{
	kind: EntityMCPServers, what: "an MCP server", noun: "MCP server",
	read: readServerRemoval,
	bodyHint: `send {"clear": true} to clear every field that names the server, and ` +
		`"delete_secrets" with the stored values it read that should go with it`,
	summaryHint: "name the server removed",
	logAs:       "server",
}

// readServerRemoval reads a server's removal from its body strictly; an empty
// body clears nothing and deletes nothing. It refuses a name given twice in
// `delete_secrets` and an empty one.
func readServerRemoval(s *Service, name string, text []byte) (removing, string, error) {
	var body struct {
		MCPRemovalRequest
		// Summary is the body's `_summary`, as every config write takes it.
		Summary string `json:"_summary"`
	}
	m := &mcpRemoval{s: s, name: name}
	if len(bytes.TrimSpace(text)) == 0 {
		return m, "", nil
	}
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return nil, "", fmt.Errorf("the body is not {\"clear\": true, "+
			"\"delete_secrets\": [...]}: %w", err)
	}
	if err := body.check(); err != nil {
		return nil, "", err
	}
	m.req = body.MCPRemovalRequest
	return m, strings.TrimSpace(body.Summary), nil
}

// mcpRemoval is one server's removal, read.
type mcpRemoval struct {
	s    *Service
	name string
	req  MCPRemovalRequest
}

func (m *mcpRemoval) deleting() []string { return m.req.DeleteSecrets }

func (m *mcpRemoval) readsAfter(prior *config.Company) ([]config.Reference, error) {
	return readAfter(prior, m.name)
}

// draft removes the server and, with clear, every field that names it alone,
// as one revision built on expect; it refuses a field left naming it.
func (m *mcpRemoval) draft(expect string) draft {
	return draft{
		expect: expect, requireActive: true,
		rules: authoredRules, authored: true,
		build: func(b base) (*config.Company, []byte, error) {
			next, err := config.DecodeCompany(b.document)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: decode the active revision: %w", err)
			}
			cleared, found := next.RemoveMCPServer(m.name)
			if !found {
				return nil, nil, &EntityError{Err: ErrNoSuchEntity}
			}
			if problems := m.problems(cleared); len(problems) > 0 {
				after, readErr := readAfter(b.prior, m.name)
				if readErr != nil {
					return nil, nil, readErr
				}
				return nil, nil, &MCPRemovalError{Problems: problems,
					Cleared: clearedOf(cleared), Before: b.prior.VariablesRead(), After: after}
			}
			document, err := json.Marshal(next)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: encode the config: %w", err)
			}
			// WHAT THIS BUILD CANNOT REPRESENT STAYS, as on every write of
			// the whole document: see unknown.go.
			if document, err = carryUnknown(b.document, document); err != nil {
				return nil, nil, err
			}
			return next, document, nil
		},
	}
}

// problems is each field still naming the server, when the request does not
// clear them.
func (m *mcpRemoval) problems(cleared []config.MCPServerReference) []config.Problem {
	if m.req.Clear {
		return nil
	}
	var out []config.Problem
	for _, ref := range cleared {
		out = append(out, config.Problem{
			Path: ref.Segments.String(), Segments: ref.Segments, Kind: "conflict",
			Seat: ref.Seat, Unit: ref.Unit, Message: stillNames(ref),
		})
	}
	return out
}

// stillNames is a field the removal would leave naming the server, in a
// person's words.
func stillNames(ref config.MCPServerReference) string {
	holder := ref.Seat
	if ref.Unit != "" {
		holder = "unit " + ref.Unit
	}
	switch {
	case ref.Field == config.MCPReferenceSandbox:
		return fmt.Sprintf("%s's sandbox.mcp.servers still lists %q: send clear to take it "+
			"out in the same write", ref.Seat, ref.Server)
	case ref.Field == config.MCPReferenceLimits:
		return fmt.Sprintf("%s's mcp_limits block for %q still holds it back: send clear to "+
			"clear it in the same write", holder, ref.Server)
	case ref.Unit != "":
		return fmt.Sprintf("unit %s's mcp_env block for %q still holds credentials: send "+
			"clear to clear it in the same write", ref.Unit, ref.Server)
	}
	return fmt.Sprintf("%s's mcp_env block for %q still holds credentials: send clear "+
		"to clear it in the same write", ref.Seat, ref.Server)
}

// refuse answers what the request leaves unanswered: 422, each problem at its
// field, and every dependant, so a page draws the whole removal from the
// refusal.
func (m *mcpRemoval) refuse(w http.ResponseWriter, r *http.Request, err error) bool {
	var refused *MCPRemovalError
	if !errors.As(err, &refused) {
		return false
	}
	values, err := m.s.removalSecrets(r.Context(), refused.Before, refused.After)
	if err != nil {
		m.s.fail(w, r, "read which values the credential store holds", err)
		return true
	}
	httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError,
		httpjson.Detail{
			"detail":     refused.Error(),
			"problems":   refused.Problems,
			"dependants": MCPDependants{MCPCleared: refused.Cleared, RemovalSecrets: values},
			"hint": "send clear: true to clear every field that names the server in the " +
				"same write; delete_secrets may name only values listed under " +
				"dependants.secrets",
		})
	return true
}

// checked answers what the removal takes with it.
func (m *mcpRemoval) checked(ctx context.Context, p *prepared, values RemovalSecrets) (map[string]any, error) {
	dependants, err := m.s.serverDependants(ctx, p.prior, m.name, values)
	if err != nil {
		return nil, err
	}
	return map[string]any{"dependants": dependants}, nil
}

// ServerDependants is what removing the server called name from the fleet's
// active revision would take with it: what a dry run of the removal answers
// as `dependants`, and what a server's detail shows. False where the revision
// declares no such server.
//
// THE DRY RUN'S OWN COMPUTATION, on the same two documents: a second walk
// would be a detail page that promises one removal and a dry run another.
func (s *Service) ServerDependants(ctx context.Context, name string) (MCPDependants, bool, error) {
	cur, found, err := s.current(ctx)
	if err != nil {
		return MCPDependants{}, false, err
	}
	if !found {
		return MCPDependants{}, false, ErrNoActiveRevision
	}
	_, prior, err := s.openDocument(cur)
	if err != nil {
		return MCPDependants{}, false, err
	}
	after, err := readAfter(prior, name)
	if errors.Is(err, ErrNoSuchEntity) {
		return MCPDependants{}, false, nil
	}
	if err != nil {
		return MCPDependants{}, false, err
	}
	values, err := s.removalSecrets(ctx, prior.VariablesRead(), after)
	if err != nil {
		return MCPDependants{}, false, err
	}
	dependants, err := s.serverDependants(ctx, prior, name, values)
	return dependants, err == nil, err
}

// serverDependants is what removing the server called name from prior takes
// with it: every field naming it alone, values, and whether a grant goes.
func (s *Service) serverDependants(ctx context.Context, prior *config.Company, name string,
	values RemovalSecrets) (MCPDependants, error) {

	signedIn, err := s.grants.HeldMCPGrant(ctx, name)
	if err != nil {
		return MCPDependants{}, fmt.Errorf("configapi: read the server's OAuth grant: %w", err)
	}
	return MCPDependants{MCPCleared: clearedOf(prior.MCPServerDependants(name)),
		RemovalSecrets: values, OAuth: signedIn}, nil
}

// written gives up the server's grant and answers what the removal cleared.
// THE GRANT BEFORE THE VALUES: its revocation authenticates with the secret
// the server's entry names, which `delete_secrets` may be about to delete.
func (m *mcpRemoval) written(ctx context.Context, p *prepared, by iam.Actor) map[string]any {
	return map[string]any{
		"removed": m.name,
		"cleared": clearedOf(p.prior.MCPServerDependants(m.name)),
		"oauth":   m.revokeGrant(ctx, p.prior, by),
	}
}

// revokeGrant gives up the removed server's OAuth grant, now that the removal
// is written: revoked at its vendor where it offers that, and forgotten. Nil
// where nobody held one. A grant the engine could not give up is `failed`,
// logged with why; the removal stands, and the sign-out route ends it.
func (m *mcpRemoval) revokeGrant(ctx context.Context, prior *config.Company, by iam.Actor) *MCPGrantRevocation {
	at := slices.IndexFunc(prior.MCPServers, func(s config.MCPServer) bool { return s.Name == m.name })
	if at < 0 {
		return nil
	}
	revocation, said, held, err := m.s.grants.RevokeMCPServerGrant(ctx, &prior.MCPServers[at])
	switch {
	case err != nil:
		log.WarnContext(ctx, "mcp_grant_remove_failed", "server", m.name, "error", err,
			"by", by.Name, "operator", by.OperatorID,
			"detail", "the server is removed; its grant could not be given up here")
		return &MCPGrantRevocation{Revocation: "failed", Said: "the engine could not give the grant up"}
	case !held:
		return nil
	}
	log.InfoContext(ctx, "mcp_grant_removed", "server", m.name, "revocation", revocation,
		"by", by.Name, "operator", by.OperatorID)
	return &MCPGrantRevocation{Revocation: revocation, Said: said}
}

// readAfter is every variable prior reads once the server called name is
// removed from it, at prior's paths: an empty entry stays where the server
// stood, so "read by mcp_servers[1].env.TOKEN" names the document a person has.
func readAfter(prior *config.Company, name string) ([]config.Reference, error) {
	document, err := json.Marshal(prior)
	if err != nil {
		return nil, fmt.Errorf("configapi: encode the active revision: %w", err)
	}
	kept, err := config.DecodeCompany(document)
	if err != nil {
		return nil, fmt.Errorf("configapi: decode the active revision: %w", err)
	}
	at := slices.IndexFunc(kept.MCPServers, func(s config.MCPServer) bool { return s.Name == name })
	if _, found := kept.RemoveMCPServer(name); !found {
		return nil, &EntityError{Err: ErrNoSuchEntity}
	}
	kept.MCPServers = slices.Insert(kept.MCPServers, at, config.MCPServer{})
	return kept.VariablesRead(), nil
}

// clearedOf is the fields a removal clears, as the answer names them.
func clearedOf(refs []config.MCPServerReference) MCPCleared {
	out := MCPCleared{MCPEnv: []MCPBlockDependant{}, Sandbox: []MCPSandboxDependant{},
		MCPLimits: []MCPBlockDependant{}}
	block := func(ref config.MCPServerReference) MCPBlockDependant {
		kind, key := MCPHolderSeat, ref.Seat
		if ref.Unit != "" {
			kind, key = MCPHolderUnit, ref.Unit
		}
		return MCPBlockDependant{Kind: kind, Key: key, Name: ref.Name, Segments: ref.Segments}
	}
	for _, ref := range refs {
		switch ref.Field {
		case config.MCPReferenceEnv:
			out.MCPEnv = append(out.MCPEnv, block(ref))
		case config.MCPReferenceLimits:
			out.MCPLimits = append(out.MCPLimits, block(ref))
		case config.MCPReferenceSandbox:
			out.Sandbox = append(out.Sandbox, MCPSandboxDependant{Seat: ref.Seat,
				Name: ref.Name, Segments: ref.Segments})
		}
	}
	return out
}
