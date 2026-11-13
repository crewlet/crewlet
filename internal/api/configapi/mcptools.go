package configapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// Setting an MCP server's tool levels: `PUT /mcp-servers/{name}/tools`
// (internal/api/mcpapi), written here.
//
// A CHANGE, NOT A REPLACEMENT: the request names the default and the tools it
// sets, and every level it does not name stays. Two people blocking two tools
// of one server each land, as two edits of one document do, so the write takes
// no If-Match; it is built on the fleet's revision as it stands when it is
// written, again on each attempt a concurrent write wins
// ([entityAttempts]).
//
// THE WHOLE DOCUMENT IS HELD TO EVERY RULE an authored write is, so a level
// that leaves a server some executor box lists with a tool held back is
// refused at the box's entry ([config.Company.ValidateAdmission]), and a step-up
// older than the window is refused by the route's own verb before anything is
// read.

// MCPToolsChange is one change to an MCP server's tool levels
// (`mcp_servers[].tools`).
type MCPToolsChange struct {
	// SetDefault changes `tools.default` to Default: a level, or nil to
	// remove it, which leaves every tool no level names at allow.
	SetDefault bool
	Default    *config.MCPToolLevel
	// Levels sets each named tool's level, by the server's own name for
	// it; a nil level removes the tool's own, leaving it at the default.
	Levels map[string]*config.MCPToolLevel
}

// MCPToolsWrite is one write of a server's tool levels.
type MCPToolsWrite struct {
	Server string
	Change MCPToolsChange
	// DryRun checks the write and stores nothing.
	DryRun bool
	// Summary is the revision's audit summary, required of a write.
	Summary string
	// By is who the write is made as, and Principal who asked for it.
	By        iam.Actor
	Principal *iam.Principal
}

// MCPToolsWritten is what a write of a server's tool levels did.
type MCPToolsWritten struct {
	// Tools are the server's tool levels as the write leaves them.
	Tools config.MCPToolPolicy
	// Answer is the configuration's own answer: a dry run's check
	// (`valid`, `base_revision_id`, `warnings`, `derived`), or the stored
	// revision (`revision_id`, `epoch`, `warnings`, `derived`).
	Answer map[string]any
	// Applied reports a revision stored and activated: false for a dry run.
	Applied bool
}

// ErrMCPToolsSummary is a write of a server's tool levels sent with no audit
// summary.
var ErrMCPToolsSummary = errors.New("configapi: a write of an MCP server's tool levels needs an " +
	"audit summary in the X-Summary header or the body's `_summary`: name the server and what " +
	"the levels now are")

// SetMCPTools writes w's change to the server's tool levels as one revision
// and activates it, or with DryRun checks it and stores nothing. A server the
// fleet's revision does not declare is an [*EntityError] wrapping
// [ErrNoSuchEntity]; every other refusal is any write's
// ([Service.RefuseMCPTools] answers each).
func (s *Service) SetMCPTools(ctx context.Context, w MCPToolsWrite) (MCPToolsWritten, error) {
	var raced error
	for range entityAttempts {
		var tools config.MCPToolPolicy
		d := draft{
			requireActive: true, rules: authoredRules, authored: true,
			principal: w.Principal, by: w.By,
			build: func(b base) (*config.Company, []byte, error) {
				next, err := config.DecodeCompany(b.document)
				if err != nil {
					return nil, nil, fmt.Errorf("configapi: decode the active revision: %w", err)
				}
				at := slices.IndexFunc(next.MCPServers, func(m config.MCPServer) bool { return m.Name == w.Server })
				if at < 0 {
					return nil, nil, &EntityError{Err: ErrNoSuchEntity}
				}
				server := &next.MCPServers[at]
				server.Tools = w.Change.applied(server.Tools)
				tools = server.Tools
				document, err := json.Marshal(next)
				if err != nil {
					return nil, nil, fmt.Errorf("configapi: encode the config: %w", err)
				}
				// WHAT THIS BUILD CANNOT REPRESENT STAYS, as on every write
				// of the whole document: see unknown.go.
				if document, err = carryUnknown(b.document, document); err != nil {
					return nil, nil, err
				}
				return next, document, nil
			},
		}
		prepared, err := s.prepare(ctx, d)
		var lost *RacedError
		switch {
		case errors.As(err, &lost):
			raced = err
			continue
		case err != nil:
			return MCPToolsWritten{}, err
		}
		if w.DryRun {
			if err = s.checks(prepared, w.Summary); err != nil {
				return MCPToolsWritten{}, err
			}
			return MCPToolsWritten{Tools: tools, Answer: checkedAnswer(prepared)}, nil
		}
		// ASKED LAST, once the write is admitted and its document checked
		// ([requireSummary]'s rule).
		if w.Summary == "" {
			return MCPToolsWritten{}, ErrMCPToolsSummary
		}
		applied, err := s.commit(ctx, prepared, w.Summary)
		switch {
		case errors.As(err, &lost):
			raced = err
			continue
		case err != nil:
			return MCPToolsWritten{}, err
		}
		return MCPToolsWritten{Tools: tools, Answer: appliedAnswer(applied), Applied: true}, nil
	}
	return MCPToolsWritten{}, raced
}

// applied is policy with the change made: the default set or removed where
// the change says so, and each named tool's level set or removed. A policy
// left with no levels holds none, so it drops out of the document whole.
func (c MCPToolsChange) applied(policy config.MCPToolPolicy) config.MCPToolPolicy {
	out := config.MCPToolPolicy{Default: policy.Default, Levels: maps.Clone(policy.Levels)}
	if c.SetDefault {
		out.Default = ""
		if c.Default != nil {
			out.Default = *c.Default
		}
	}
	for tool, level := range c.Levels {
		if level == nil {
			delete(out.Levels, tool)
			continue
		}
		if out.Levels == nil {
			out.Levels = map[string]config.MCPToolLevel{}
		}
		out.Levels[tool] = *level
	}
	if len(out.Levels) == 0 {
		out.Levels = nil
	}
	return out
}

// RefuseMCPTools answers a write of a server's tool levels that did not land:
// a server the fleet's revision does not declare as 404, a write with no
// summary as 400, and every other refusal as any write of the document is
// answered ([Service.refuseApply]): a level the company cannot take is 422,
// each problem at its field.
func (s *Service) RefuseMCPTools(w http.ResponseWriter, r *http.Request, server string, err error) {
	switch {
	case errors.Is(err, ErrNoSuchEntity):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchEntity, map[string]string{
			"hint": "the active configuration declares no MCP server " + fmt.Sprintf("%q", server) +
				": add it first, then set its tools' levels",
		})
	case errors.Is(err, ErrMCPToolsSummary):
		requireSummary(w, "", "name the server and what its tools' levels now are")
	default:
		s.refuseApply(w, r, err)
	}
}
