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
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// Removing an MCP server: `DELETE /config/mcp-servers/{id}`.
//
// # One revision, every field that names it cleared in it
//
// An `mcp_servers` entry is named by its name from two other places: each
// seat's and unit's `mcp_env` block keyed on it, which holds that seat's or
// team's credentials for it, and each `sandbox.mcp.servers` entry listing it,
// which gives a seat's executor box the server. Removed alone, the entry
// leaves every one of them naming nothing, which validation refuses
// ([config.Company.MCPServerReferences] is the one walk both read). So the
// route takes the entry and, with `clear`, every such field as ONE write.
// Without it, a server anything names is refused naming all of it, each at its
// field, so a page draws the list from the refusal and the person removing
// the server sees the whole of what goes with it before it does.
//
// A FIELD IS CLEARED WHOLE, never repointed as a model's chain is: a block of
// credentials for a server that no longer exists has nowhere to move. And one
// the engine reads by its name besides the server's (a seat's `mcp_env.github`
// is also its GitHub App token) is not the server's, so it stays
// ([config.Company.MCPServerDependants]).
//
// # The values only it read may go with it
//
// What a server and its blocks read are `${VAR}` references into the
// company's credential store. Once the removal is written some of them are
// read by nothing: the ones the store holds are offered as
// `dependants.secrets`, and the caller may name any of them in
// `delete_secrets`. ONLY those: a name anything else in the document reads
// (another server, a seat's integration, a model) is refused at its index,
// because deleting it would sign that reader out. "Read" is
// [config.Company.VariablesRead], the one answer `/secrets` gives an operator
// about to delete a row, so a value it calls unread is one deleting breaks
// nothing that answer knows of.
//
// DELETING A VALUE IS `secrets.write`, on top of the route's `config.write`,
// decided before anything is read or written: the Dashboard Assistant never
// takes `secrets.write`, so it may remove a server and never its credentials.
//
// THE VALUES GO AFTER THE REVISION IS WRITTEN, never before: deleted first, a
// removal that then lost its write would leave the server running with its
// credentials gone. And each is asked again of the fleet's revision just before
// it goes, so a value a write that landed meanwhile reads is kept, and the
// answer says which revision paths read it. Nothing here reads or answers a
// value: only names.

// MCPRemovalRequest is the body `DELETE /config/mcp-servers/{id}` reads.
type MCPRemovalRequest struct {
	// Clear clears every field that names the server alone, in the same
	// revision. Without it, a server anything names is refused naming all of
	// it.
	Clear bool `json:"clear"`
	// DeleteSecrets names stored values to delete once the removal is
	// written, each one nothing reads once the server and the fields it
	// clears are gone.
	DeleteSecrets []string `json:"delete_secrets"`
}

// MCPHolderKind is whose `mcp_env` block an [MCPEnvDependant] is.
type MCPHolderKind string

const (
	// MCPHolderSeat is a seat's own block.
	MCPHolderSeat MCPHolderKind = "seat"
	// MCPHolderUnit is a unit's block, which its agent seats inherit.
	MCPHolderUnit MCPHolderKind = "unit"
)

// Valid reports whether k is one of the two holders.
func (k MCPHolderKind) Valid() bool { return k == MCPHolderSeat || k == MCPHolderUnit }

// MCPEnvDependant is one `mcp_env` block keyed on the server.
type MCPEnvDependant struct {
	// Kind is whose block it is; Key is the seat's handle or the unit's key,
	// and Name its display name.
	Kind MCPHolderKind `json:"kind"`
	Key  string        `json:"key"`
	Name string        `json:"name"`
	// Segments is the written field: ["units", 0, "mcp_env", "linear"].
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
// order. Neither list is ever null.
type MCPCleared struct {
	MCPEnv  []MCPEnvDependant     `json:"mcp_env"`
	Sandbox []MCPSandboxDependant `json:"sandbox"`
}

// MCPDependants is what a removal of an MCP server takes with it: the fields it
// clears, and the stored values nothing reads once it is written, which it may
// delete.
type MCPDependants struct {
	MCPCleared
	// Secrets is every name the credential store holds a value under that
	// nothing reads once the removal is written, sorted: what
	// `delete_secrets` may name. Never null.
	Secrets []string `json:"secrets"`
}

// MCPSecretOutcome is what became of one value a removal was asked to delete.
type MCPSecretOutcome string

const (
	// MCPSecretDeleted is a value the store held and no longer does.
	MCPSecretDeleted MCPSecretOutcome = "deleted"
	// MCPSecretAbsent is a name the store held no value under: there was
	// nothing to delete.
	MCPSecretAbsent MCPSecretOutcome = "absent"
	// MCPSecretKept is a value a revision activated after the removal reads,
	// so it stays.
	MCPSecretKept MCPSecretOutcome = "kept"
	// MCPSecretFailed is a value the store or the fleet's revision did not
	// answer for: it may still be there, and `DELETE /secrets/{name}`
	// deletes it.
	MCPSecretFailed MCPSecretOutcome = "failed"
)

// Valid reports whether o is one of the four outcomes.
func (o MCPSecretOutcome) Valid() bool {
	switch o {
	case MCPSecretDeleted, MCPSecretAbsent, MCPSecretKept, MCPSecretFailed:
		return true
	}
	return false
}

// MCPSecretDeletion is one value a removal was asked to delete, and what
// became of it.
type MCPSecretDeletion struct {
	Name    string           `json:"name"`
	Outcome MCPSecretOutcome `json:"outcome"`
	// ReadBy is, for a value kept, the paths of the fleet's revision that
	// read it.
	ReadBy []string `json:"read_by,omitempty"`
}

// MCPRemovalError is a removal that would leave a field naming the server it
// removes, or that names a value it may not delete: the problems, each at its
// field, and what the removal takes with it.
type MCPRemovalError struct {
	Problems []config.Problem
	Cleared  MCPCleared
	// Unread is every name nothing reads once the removal is written; the
	// refusal offers the ones the store holds.
	Unread []string
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
	kind: EntityMCPServers, what: "an MCP server",
	read: readServerRemoval,
	bodyHint: `send {"clear": true} to clear every field that names the server, and ` +
		`"delete_secrets" with the stored values only it read that should go with it`,
	summaryHint: "name the server removed",
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
	first := map[string]int{}
	for i, secret := range body.DeleteSecrets {
		if strings.TrimSpace(secret) == "" {
			return nil, "", fmt.Errorf("delete_secrets[%d] names no value", i)
		}
		if j, seen := first[secret]; seen {
			return nil, "", fmt.Errorf("delete_secrets[%d] names %q, as delete_secrets[%d] "+
				"does: name each value once", i, secret, j)
		}
		first[secret] = i
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

// admit asks `secrets.write` of a removal that deletes a value, as a check
// too, so the Dashboard Assistant's preview is refused before a person
// approves a card that could never land. A removal that deletes nothing asks
// nothing beyond the route's `config.write`.
func (m *mcpRemoval) admit(w http.ResponseWriter, r *http.Request) bool {
	if len(m.req.DeleteSecrets) == 0 {
		return true
	}
	p, ok := principalOf(w, r)
	if !ok {
		return false
	}
	policy := authz.Policy{Action: authz.ActionSecretWrite}
	d := authz.Decide(r.Context(), *p, policy.Action, authz.Object{Kind: authz.KindCompany},
		authz.NoChart{}, authz.DecidedAt(r.Context()))
	if d.Unknown() || !d.Allowed {
		authz.EnvelopeRefusal(w, r, policy, d)
		return false
	}
	return true
}

// draft removes the server and, with clear, every field that names it alone,
// as one revision built on expect; it refuses what the request leaves
// unanswered ([mcpRemoval.problems]).
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
			unread := unreadAfter(b.prior, next)
			readers, err := readAfter(b.document, m.name)
			if err != nil {
				return nil, nil, err
			}
			if problems := m.problems(cleared, unread, readers); len(problems) > 0 {
				return nil, nil, &MCPRemovalError{Problems: problems,
					Cleared: clearedOf(cleared), Unread: unread}
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

// problems is what the request leaves wrong: each field still naming the
// server when it does not clear them, and each value it names to delete that
// something still reads ([readAfter]) or that the server never read.
func (m *mcpRemoval) problems(cleared []config.MCPServerReference, unread []string,
	readers []config.Reference) []config.Problem {

	var out []config.Problem
	if !m.req.Clear {
		for _, ref := range cleared {
			out = append(out, config.Problem{
				Path: ref.Segments.String(), Segments: ref.Segments, Kind: "conflict",
				Seat: ref.Seat, Unit: ref.Unit, Message: stillNames(ref),
			})
		}
	}
	readBy := map[string][]string{}
	for _, ref := range readers {
		readBy[ref.Name] = append(readBy[ref.Name], ref.Path)
	}
	for i, secret := range m.req.DeleteSecrets {
		if slices.Contains(unread, secret) {
			continue
		}
		// AT THE NAME ITSELF, by its index in the body, so a page marks the
		// one value it may not delete rather than the whole list.
		path := config.Path{"delete_secrets", i}
		problem := config.Problem{Path: path.String(), Segments: path}
		if paths := readBy[secret]; len(paths) > 0 {
			problem.Kind = "conflict"
			problem.Message = fmt.Sprintf("%s is still read by %s: a value stays while "+
				"anything reads it", secret, strings.Join(paths, ", "))
		} else {
			problem.Kind = "unknown_value"
			problem.Message = fmt.Sprintf("%s is not read by %q or a field that names it: "+
				"this removal deletes only a value it leaves nothing reading", secret, m.name)
		}
		out = append(out, problem)
	}
	return out
}

// stillNames is a field the removal would leave naming the server, in a
// person's words.
func stillNames(ref config.MCPServerReference) string {
	switch {
	case ref.Field == config.MCPReferenceSandbox:
		return fmt.Sprintf("%s's sandbox.mcp.servers still lists %q: send clear to take it "+
			"out in the same write", ref.Seat, ref.Server)
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
	held, err := m.held(r.Context(), refused.Unread)
	if err != nil {
		m.s.fail(w, r, "read which values the credential store holds", err)
		return true
	}
	httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError,
		httpjson.Detail{
			"detail":     refused.Error(),
			"problems":   refused.Problems,
			"dependants": MCPDependants{MCPCleared: refused.Cleared, Secrets: held},
			"hint": "send clear: true to clear every field that names the server in the " +
				"same write; delete_secrets may name only values listed under " +
				"dependants.secrets",
		})
	return true
}

// checked answers a dry run that passed: the check, and what the removal
// takes with it.
func (m *mcpRemoval) checked(w http.ResponseWriter, r *http.Request, p *prepared) {
	held, err := m.held(r.Context(), unreadAfter(p.prior, p.company))
	if err != nil {
		m.s.fail(w, r, "read which values the credential store holds", err)
		return
	}
	answer := checkedAnswer(p)
	answer["dependants"] = MCPDependants{
		MCPCleared: clearedOf(p.prior.MCPServerDependants(m.name)), Secrets: held,
	}
	writeJSON(w, http.StatusOK, answer)
}

// applied deletes the values the request named, now that the removal is
// written, and answers the write: what it cleared, and what became of each
// value.
func (m *mcpRemoval) applied(w http.ResponseWriter, r *http.Request, p *prepared, a Applied) {
	answer := appliedAnswer(a)
	answer["removed"] = m.name
	answer["cleared"] = clearedOf(p.prior.MCPServerDependants(m.name))
	answer["secrets"] = m.deleteSecrets(r.Context(), attributionOf(r))
	writeJSON(w, http.StatusCreated, answer)
}

// deleteSecrets deletes each value the request named that the fleet's revision
// still reads nothing of, and says what became of every one. Never null.
//
// ASKED AGAIN OF THE FLEET'S REVISION, which is this removal's unless a write
// landed since: a value that write reads is kept. And a revision that cannot be
// read deletes nothing, since nothing then says the value is unread.
func (m *mcpRemoval) deleteSecrets(ctx context.Context, by iam.Actor) []MCPSecretDeletion {
	out := make([]MCPSecretDeletion, 0, len(m.req.DeleteSecrets))
	if len(m.req.DeleteSecrets) == 0 {
		return out
	}
	readBy, err := m.s.Reads(ctx, m.req.DeleteSecrets)
	if err != nil {
		log.WarnContext(ctx, "secret_remove_failed", "server", m.name,
			"names", m.req.DeleteSecrets, "error", err, "by", by.Name, "operator", by.OperatorID,
			"detail", "the server is removed; which paths read its values could not be "+
				"read, so none was deleted: DELETE /secrets/{name} deletes each")
		for _, secret := range m.req.DeleteSecrets {
			out = append(out, MCPSecretDeletion{Name: secret, Outcome: MCPSecretFailed})
		}
		return out
	}
	for _, secret := range m.req.DeleteSecrets {
		if paths := readBy[secret]; len(paths) > 0 {
			out = append(out, MCPSecretDeletion{Name: secret, Outcome: MCPSecretKept, ReadBy: paths})
			continue
		}
		removed, err := m.s.secrets.Unset(ctx, secret)
		if err != nil {
			log.WarnContext(ctx, "secret_remove_failed", "name", secret, "server", m.name,
				"error", err, "by", by.Name, "operator", by.OperatorID)
			out = append(out, MCPSecretDeletion{Name: secret, Outcome: MCPSecretFailed})
			continue
		}
		// THE NAME, never the value, as `DELETE /secrets/{name}` logs it.
		log.InfoContext(ctx, "secret_removed", "name", secret, "removed", removed,
			"server", m.name, "by", by.Name, "operator", by.OperatorID)
		outcome := MCPSecretDeleted
		if !removed {
			outcome = MCPSecretAbsent
		}
		out = append(out, MCPSecretDeletion{Name: secret, Outcome: outcome})
	}
	return out
}

// held is the names the credential store holds a value under, of names, in
// their order. Never null.
func (m *mcpRemoval) held(ctx context.Context, names []string) ([]string, error) {
	out := []string{}
	for _, name := range names {
		_, stored, err := m.s.secrets.Describe(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("configapi: describe the stored value %s: %w", name, err)
		}
		if stored {
			out = append(out, name)
		}
	}
	return out, nil
}

// readAfter is every variable the stored document reads once the server called
// name is removed from it, at the paths the document has NOW: the removal made
// with an empty entry left where the server stood, so nothing after it moves.
// A person reads "still read by mcp_servers[1].env.TOKEN" against the document
// in front of them, where the next document has every later server one place
// up.
func readAfter(document []byte, name string) ([]config.Reference, error) {
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

// unreadAfter is every variable prior reads and next does not, sorted: the
// values a write leaves nothing reading ([config.Company.VariablesRead]).
func unreadAfter(prior, next *config.Company) []string {
	still := map[string]bool{}
	for _, ref := range next.VariablesRead() {
		still[ref.Name] = true
	}
	var out []string
	for _, ref := range prior.VariablesRead() {
		if !still[ref.Name] {
			out = append(out, ref.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// clearedOf is the fields a removal clears, as the answer names them.
func clearedOf(refs []config.MCPServerReference) MCPCleared {
	out := MCPCleared{MCPEnv: []MCPEnvDependant{}, Sandbox: []MCPSandboxDependant{}}
	for _, ref := range refs {
		switch ref.Field {
		case config.MCPReferenceEnv:
			kind, key := MCPHolderSeat, ref.Seat
			if ref.Unit != "" {
				kind, key = MCPHolderUnit, ref.Unit
			}
			out.MCPEnv = append(out.MCPEnv, MCPEnvDependant{Kind: kind, Key: key,
				Name: ref.Name, Segments: ref.Segments})
		case config.MCPReferenceSandbox:
			out.Sandbox = append(out.Sandbox, MCPSandboxDependant{Seat: ref.Seat,
				Name: ref.Name, Segments: ref.Segments})
		}
	}
	return out
}
