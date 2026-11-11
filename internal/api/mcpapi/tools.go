package mcpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// `PUT /mcp-servers/{name}/tools` sets the levels of a server's tools:
// `{"default": "allow"|"approve"|"blocked"|null, "levels": {"<tool>": "allow"|"approve"|"blocked"|null}}`,
// each key the server's own name for a tool. A key the body leaves out stays
// as it is; null removes the default or a tool's own level. Written through
// the configuration's own pipeline ([configapi.Service.SetMCPTools]): decided
// as `config.write`, at a recent step-up for a write and the read's proof for
// a check (`dry_run=true`), validated as the whole document, and stored as one
// revision under the request's summary.
//
// A LEVEL NAMING NO TOOL THE FLEET LISTS IS KEPT AND SAID: a server may list
// the tool later (a server mid-upgrade, a per-agent server whose seats' own
// credentials reach it), so it is written, and the answer names every such
// level as `unlisted`, since one that decides nothing is most often a name
// spelled with the prefix, or one the server spells differently.

// ToolDocuments writes a server's tool levels ([configapi.Service]).
type ToolDocuments interface {
	Authorize(by iam.Actor) error
	SetMCPTools(ctx context.Context, w configapi.MCPToolsWrite) (configapi.MCPToolsWritten, error)
	RefuseMCPTools(w http.ResponseWriter, r *http.Request, server string, err error)
}

// toolsRequest is the body the route reads.
type toolsRequest struct {
	Default optionalLevel                   `json:"default"`
	Levels  map[string]*config.MCPToolLevel `json:"levels"`
	// Summary is the body's `_summary`, as every configuration write takes it.
	Summary string `json:"_summary"`
}

// optionalLevel is the body's `default`: absent leaves it, null removes it.
type optionalLevel struct {
	set   bool
	level *config.MCPToolLevel
}

// UnmarshalJSON records that the key was sent, a null included.
func (o *optionalLevel) UnmarshalJSON(raw []byte) error {
	o.set = true
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		o.level = nil
		return nil
	}
	var level config.MCPToolLevel
	if err := json.Unmarshal(raw, &level); err != nil {
		return err
	}
	o.level = &level
	return nil
}

// toolsShape is the body, as a refusal names it.
const toolsShape = `{"default": "allow"|"approve"|"blocked"|null, "levels": {"<tool>": "allow"|"approve"|"blocked"|null}}`

// maxToolsBody bounds the body: a level for each of the most tools one server
// lists (internal/mcp's MaxServerTools, 1,024), each name at a tool name's
// longest (about 150 bytes a level), with room for the summary.
const maxToolsBody = 256 << 10

// ToolsAnswer is what the route answers beside the configuration's own answer
// (a dry run's check, or the revision written): the server's tool levels as
// the write leaves them.
type ToolsAnswer struct {
	Server string `json:"server"`
	// DryRun is whether nothing was written.
	DryRun bool `json:"dry_run"`
	// Default is `tools.default` as written, null where it is unset, which
	// leaves every tool Levels does not name at the level its hints start
	// it at (allow for a tool the server marks read-only, approve for every
	// other).
	Default *config.MCPToolLevel `json:"default"`
	// Levels is every tool's own level, by the server's own name for it.
	// Always an object.
	Levels map[string]config.MCPToolLevel `json:"levels"`
	// Unlisted are the tools Levels names that no live node's listing of
	// the server names ([queries.UnlistedLevels]), sorted: kept, and said.
	// Empty where the listings cannot say. Always a list.
	Unlisted []string `json:"unlisted"`
	// Listing says whether Unlisted could be judged: `read`, or
	// `unavailable` where this node could not read the fleet's listing of
	// the server, which leaves Unlisted empty.
	Listing string `json:"listing"`
}

// The two readings of [ToolsAnswer.Listing].
const (
	ListingRead        = "read"
	ListingUnavailable = "unavailable"
)

// serveTools sets the levels of the server its path names.
func (s *Service) serveTools(w http.ResponseWriter, r *http.Request, rec *types.MCPServerActed) {
	ctx := r.Context()
	name := r.PathValue("name")
	dryRun, ok := toolsQuery(w, r)
	if !ok {
		return
	}
	rec.DryRun = dryRun
	if s.tools == nil {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "this node serves no configuration surface to write the levels " +
				"through; ask through a node that serves the dashboard"})
		return
	}
	by := auth.AttributionOf(ctx)
	var managed *configapi.ManagedError
	if err := s.tools.Authorize(by); errors.As(err, &managed) {
		configapi.RefuseManaged(w, managed)
		return
	}
	var req toolsRequest
	if !decodeBodyWithin(w, r, &req, toolsShape, maxToolsBody) {
		return
	}
	if !req.Default.set && len(req.Levels) == 0 {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "the body changes nothing: send the default, or the levels of the " +
				"tools to change, as " + toolsShape})
		return
	}
	summary := strings.TrimSpace(req.Summary)
	if header := strings.TrimSpace(r.Header.Get("X-Summary")); header != "" {
		// THE HEADER WINS, as on every configuration write.
		summary = header
	}
	write := configapi.MCPToolsWrite{
		Server: name, DryRun: dryRun, Summary: summary, By: by,
		Change: configapi.MCPToolsChange{SetDefault: req.Default.set, Default: req.Default.level,
			Levels: req.Levels},
	}
	if principal, how := iam.From(ctx); how == iam.Resolved {
		write.Principal = &principal
	}
	written, err := s.tools.SetMCPTools(ctx, write)
	if err != nil {
		s.tools.RefuseMCPTools(w, r, name, err)
		return
	}
	answer := ToolsAnswer{Server: name, DryRun: !written.Applied, Levels: map[string]config.MCPToolLevel{},
		Unlisted: []string{}, Listing: ListingRead}
	if written.Tools.Default != "" {
		level := written.Tools.Default
		answer.Default = &level
	}
	maps.Copy(answer.Levels, written.Tools.Levels)
	// AFTER THE WRITE, and never in its way: the listing says what the
	// levels decide, and one that cannot be read leaves a written level
	// written.
	detail, err := s.detail(ctx, name)
	switch {
	case err == nil:
		answer.Unlisted = queries.UnlistedLevels(written.Tools, detail)
	case errors.Is(err, queries.ErrNotFound):
		// NEITHER RUN NOR LISTED ANYWHERE: nothing lists a tool to judge
		// a level by, which is an empty listing, not an unread one.
	default:
		log.WarnContext(ctx, "mcp_tools_listing_unread", "server", name, "error", err.Error())
		answer.Listing = ListingUnavailable
	}
	if len(answer.Unlisted) > 0 {
		log.InfoContext(ctx, "mcp_tool_levels_unlisted", "server", name, "tools", answer.Unlisted,
			"detail", "these levels name no tool the server lists, so they decide nothing until it lists one")
	}
	body := map[string]any{}
	maps.Copy(body, written.Answer)
	for key, value := range toolsFields(answer) {
		body[key] = value
	}
	httpjson.Write(w, http.StatusOK, body)
}

// toolsFields is answer's keys as the body carries them beside the
// configuration's own.
func toolsFields(answer ToolsAnswer) map[string]any {
	raw, err := json.Marshal(answer)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return map[string]any{}
	}
	return out
}

// toolsQuery reads `dry_run` (true or false, once), the only parameter.
func toolsQuery(w http.ResponseWriter, r *http.Request) (dryRun, ok bool) {
	query := r.URL.Query()
	for param, values := range query {
		if param != "dry_run" || len(values) != 1 || !slices.Contains([]string{"true", "false"}, values[0]) {
			httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
				httpjson.Detail{"detail": "this route takes dry_run=true|false, at most once, and no other " +
					"parameter: dry_run=true checks the change and writes nothing"})
			return false, false
		}
	}
	return query.Get("dry_run") == "true", true
}
