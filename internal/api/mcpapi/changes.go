package mcpapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcpreview"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/words"
)

// `POST /mcp-servers/{name}/changes` decides changes a server made to its tools
// since their levels were decided (ADR-0078):
// `{"accept": {"<tool>": "<digest>"|null}, "block": {"<tool>": "<digest>"|null}}`,
// each key the server's own name for a tool and each value the digest of the
// definition reviewed, the `after.digest` of its change in the detail's
// `changes`, or null for a removal.
//
// ACCEPTING releases the tool: its level holds for the definition accepted,
// so an allowed one runs without asking again. BLOCKING keeps it from every
// agent (its level set to blocked, as `PUT /mcp-servers/{name}/tools` sets it)
// and accepts the definition too, since that is the definition the decision
// was taken on: a tool unblocked later runs at the level set then, on it.
//
// A CONFIGURATION WRITE, decided as one: `config.write` at its recent step-up,
// since accepting can return a tool to running unasked, and a block is a
// revision of the company. Audited as `mcp_server_acted`, action
// `review_changes`.
//
// A SERVER CANNOT MAKE A CHANGE LOOK ACCEPTED. Each decision names the digest
// of the definition the person reviewed, and is refused `409 stale_version`
// unless that is a definition the fleet lists for the tool now and nobody has
// accepted: a server that swapped the definition between the review and the
// answer is answered with the review to read again, and nothing is written.

// Reviews is where a server's changes are read ([queries.Sources.MCPReview])
// and a decision about one written ([coord.MCPAccepted]).
type Reviews interface {
	MCPReview(ctx context.Context, server string) (queries.MCPReview, error)
	PutMCPAccepted(ctx context.Context, a coord.MCPAcceptedTool, version uint64) (coord.MCPAcceptedTool, bool, error)
}

// changesRequest is the body the route reads.
type changesRequest struct {
	Accept map[string]*string `json:"accept"`
	Block  map[string]*string `json:"block"`
	// Summary is the revision's audit summary for a block, as every
	// configuration write takes it; one naming the tools is written where
	// none is sent.
	Summary string `json:"_summary"`
}

// changesShape is the body, as a refusal names it.
const changesShape = `{"accept": {"<tool>": "<digest>"|null}, "block": {"<tool>": "<digest>"|null}}`

// maxChangesBody bounds the body: a decision for each of the most tools one
// server lists (internal/mcp's MaxServerTools), each name and digest at their
// longest (about 200 bytes), with room for the summary.
const maxChangesBody = 1 << 20

// ChangesAnswer is what the route answers.
type ChangesAnswer struct {
	Server string `json:"server"`
	// Accepted and Blocked are the tools whose decision was written, by
	// the server's own name, sorted. Always lists.
	Accepted []string `json:"accepted"`
	Blocked  []string `json:"blocked"`
	// Moved are tools somebody else decided between this node's read and
	// its write: nothing was written for them, and their changes are to be
	// read again. Always a list.
	Moved []string `json:"moved"`
	// Revision is the configuration's answer to the levels a block wrote
	// (`revision_id`, `epoch`, `warnings`), null where nothing was blocked.
	Revision map[string]any `json:"revision"`
}

// decision is one tool's decision as the body names it, matched to the change
// it decides.
type decision struct {
	tool   string
	block  bool
	change mcpreview.Change
}

// serveChanges decides the changes the body names on the server its path
// names.
func (s *Service) serveChanges(w http.ResponseWriter, r *http.Request, rec *types.MCPServerActed) {
	ctx := r.Context()
	name := r.PathValue("name")
	if s.reviews == nil {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "this node keeps no copy of the fleet's MCP listings to decide a " +
				"change by; ask through a node that serves the dashboard"})
		return
	}
	var req changesRequest
	if !decodeBodyWithin(w, r, &req, changesShape, maxChangesBody) {
		return
	}
	if len(req.Accept) == 0 && len(req.Block) == 0 {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "the body decides nothing: name the changes to accept or to keep " +
				"blocked, as " + changesShape})
		return
	}
	for tool := range req.Accept {
		if _, both := req.Block[tool]; both {
			httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				httpjson.Detail{"detail": quoted(tool) + " is both accepted and blocked: decide each " +
					"change once, as " + changesShape})
			return
		}
	}
	review, err := s.reviews.MCPReview(ctx, name)
	if err != nil {
		log.WarnContext(ctx, "mcp_changes_unread", "server", name, "error", err.Error())
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "this node could not read the fleet's listing of the server or " +
				"what was accepted of it, so nothing was decided; ask again"})
		return
	}
	if review.Entry == nil {
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": "no MCP server " + quoted(name) + " is configured: a change is " +
				"decided for a server the company declares"})
		return
	}
	decisions, stale := matchDecisions(review, req)
	// THE AUDIT NAMES THE DECISIONS THAT MATCHED A CHANGE, by the names the
	// fleet lists: a body's other keys are only the caller's text.
	for _, d := range decisions {
		if d.block {
			rec.Blocked = append(rec.Blocked, d.tool)
		} else {
			rec.Accepted = append(rec.Accepted, d.tool)
		}
	}
	if len(stale) > 0 {
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeStaleVersion,
			httpjson.Detail{"detail": "the change reviewed is not the one waiting for " + words.And(stale) +
				": the server lists another definition now, somebody decided it, or it was never " +
				"changed. Read the server's changes again; nothing was decided.",
				"tools": stale})
		return
	}
	by := auth.AttributionOf(ctx)
	answer := ChangesAnswer{Server: name, Accepted: []string{}, Blocked: []string{}, Moved: []string{}}
	if len(req.Block) > 0 {
		revision, ok := s.blockReviewed(w, r, name, rec.Blocked, req.Summary)
		if !ok {
			return
		}
		answer.Revision = revision
	}
	now := s.now()
	for i, d := range decisions {
		wrote, err := s.decide(ctx, review, d, by.Name, now)
		switch {
		case err != nil:
			log.WarnContext(ctx, "mcp_change_not_decided", "server", name, "tool", d.tool,
				"error", err.Error())
			refusePartial(w, answer, decisions[i:])
			return
		case !wrote:
			answer.Moved = append(answer.Moved, d.tool)
		case d.block:
			answer.Blocked = append(answer.Blocked, d.tool)
		default:
			answer.Accepted = append(answer.Accepted, d.tool)
		}
	}
	httpjson.Write(w, http.StatusOK, answer)
}

// refusePartial answers 503 for decisions written one by one that stopped at
// the first of undecided: what was written, what somebody decided first and
// what was not, by name, so the caller says exactly that and reads the changes
// again. Asking again with the same body is refused, since what was written is
// decided; the ones left are asked again by themselves.
func refusePartial(w http.ResponseWriter, answer ChangesAnswer, undecided []decision) {
	left := make([]string, 0, len(undecided))
	for _, d := range undecided {
		left = append(left, d.tool)
	}
	detail := "the decision about " + left[0] + " could not be written"
	if len(left) > 1 {
		detail += ", and nothing was tried for " + words.And(left[1:])
	}
	if written := slices.Concat(answer.Accepted, answer.Blocked); len(written) > 0 {
		slices.Sort(written)
		detail += ". Decided already: " + words.And(written)
	} else {
		detail += ". Nothing was decided"
	}
	if answer.Revision != nil {
		detail += ", and the blocked levels are written"
	}
	detail += ". Read the server's changes again and decide the ones left."
	httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds, httpjson.Detail{
		"detail": detail, "accepted": answer.Accepted, "blocked": answer.Blocked, "moved": answer.Moved,
		"undecided": left})
}

// matchDecisions matches each decision the body names to the change it
// decides, naming every one that matches none: a definition the fleet does not
// list for the tool now, one already accepted, or a removal named of a tool
// still listed. In tool order.
func matchDecisions(review queries.MCPReview, req changesRequest) ([]decision, []string) {
	var out []decision
	var stale []string
	decide := func(tool string, digest *string, block bool) {
		at := slices.IndexFunc(review.Changes, func(c mcpreview.Change) bool {
			if c.Tool != tool {
				return false
			}
			if digest == nil {
				return c.Kind == mcpreview.KindRemoved
			}
			return c.After != nil && c.After.Digest == *digest
		})
		if at < 0 {
			stale = append(stale, tool)
			return
		}
		out = append(out, decision{tool: tool, block: block, change: review.Changes[at]})
	}
	for _, tool := range slices.Sorted(maps.Keys(req.Accept)) {
		decide(tool, req.Accept[tool], false)
	}
	for _, tool := range slices.Sorted(maps.Keys(req.Block)) {
		decide(tool, req.Block[tool], true)
	}
	slices.SortFunc(out, func(a, b decision) int { return strings.Compare(a.tool, b.tool) })
	slices.Sort(stale)
	return out, stale
}

// decide writes one decision as the tool's accepted definition, at the version
// the review read, reporting whether that version still held.
func (s *Service) decide(ctx context.Context, review queries.MCPReview, d decision, by string,
	now time.Time) (bool, error) {

	var version uint64
	if held, ok := review.Accepted[d.tool]; ok {
		version = held.Version
	}
	accepted := mcpreview.Removing(review.Server, d.tool, by, now)
	if d.change.After != nil {
		accepted = mcpreview.Accepting(review.Server, *d.change.After, by, now)
	}
	_, wrote, err := s.reviews.PutMCPAccepted(ctx, accepted, version)
	return wrote, err
}

// blockReviewed sets the tools to blocked on the server's entry, as
// `PUT /mcp-servers/{name}/tools` sets a level, answering a refusal itself.
func (s *Service) blockReviewed(w http.ResponseWriter, r *http.Request, server string, tools []string,
	summary string) (map[string]any, bool) {

	if s.tools == nil {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "this node serves no configuration surface to block a tool through; " +
				"ask through a node that serves the dashboard"})
		return nil, false
	}
	ctx := r.Context()
	by := auth.AttributionOf(ctx)
	var managed *configapi.ManagedError
	if err := s.tools.Authorize(by); errors.As(err, &managed) {
		configapi.RefuseManaged(w, managed)
		return nil, false
	}
	if header := strings.TrimSpace(r.Header.Get("X-Summary")); header != "" {
		// THE HEADER WINS, as on every configuration write.
		summary = header
	}
	if summary = strings.TrimSpace(summary); summary == "" {
		summary = fmt.Sprintf("Keep %s blocked on MCP server %s after reviewing their changes",
			words.And(tools), server)
	}
	blocked := config.MCPToolBlocked
	levels := make(map[string]*config.MCPToolLevel, len(tools))
	for _, tool := range tools {
		levels[tool] = &blocked
	}
	write := configapi.MCPToolsWrite{Server: server, Summary: summary, By: by,
		Change: configapi.MCPToolsChange{Levels: levels}}
	if principal, how := iam.From(ctx); how == iam.Resolved {
		write.Principal = &principal
	}
	written, err := s.tools.SetMCPTools(ctx, write)
	if err != nil {
		s.tools.RefuseMCPTools(w, r, server, err)
		return nil, false
	}
	return maps.Clone(written.Answer), true
}

// acceptHeld accepts the definition a held call was made under, by the held
// call's own digest, where a live listing names the tool under it and nobody
// has accepted it: an Always allow decides the tool's level on that definition
// (ADR-0078), so a tool held only because it changed runs unasked from then
// on. EVERY CALL THAT ACCEPTS NOTHING SAYS WHY in the log: the definition is
// accepted already, the server's first listing is not yet, or no live listing
// names the tool under it (the server lists another since, which waits for a
// review, or no node lists it now). A failure leaves the change waiting, the
// level already written.
func (s *Service) acceptHeld(ctx context.Context, held sandbox.HeldCall, by string) {
	attrs := []any{"server", held.Server, "tool", held.Raw, "digest", held.Digest}
	notAccepted := func(detail string, extra ...any) {
		log.WarnContext(ctx, "mcp_held_definition_not_accepted",
			slices.Concat(attrs, extra, []any{"detail", "the tool is allowed; " + detail})...)
	}
	if s.reviews == nil {
		notAccepted("this node keeps no copy of the fleet's listings, so its change waits for a review")
		return
	}
	review, err := s.reviews.MCPReview(ctx, held.Server)
	if err != nil {
		notAccepted("its change waits for a review", "error", err.Error())
		return
	}
	before, decided := review.Accepted[held.Raw]
	switch {
	case len(review.Accepted) == 0:
		log.InfoContext(ctx, "mcp_held_definition_left", slices.Concat(attrs, []any{"detail",
			"nothing of the server's first listing is accepted yet, and it is accepted as it is"})...)
		return
	case decided && !before.Removed && before.Digest == held.Digest:
		log.InfoContext(ctx, "mcp_held_definition_left", slices.Concat(attrs, []any{"detail",
			"the definition the call was made under is accepted already; the level written decides it"})...)
		return
	}
	listed := listedUnder(review.Listings, held.Raw, held.Digest)
	if listed == nil {
		notAccepted("no live listing names it under the definition the call was made under: the "+
			"server lists another since, which waits for a review, or no node lists it now",
			"listed", listedDigests(review.Listings, held.Raw))
		return
	}
	change := mcpreview.Change{Tool: held.Raw, Kind: mcpreview.KindAdded, After: listed}
	if decided && !before.Removed {
		change.Kind, change.Before = mcpreview.KindChanged, &before
	}
	wrote, err := s.decide(ctx, review, decision{tool: held.Raw, change: change}, by, s.now())
	switch {
	case err != nil:
		notAccepted("its change waits for a review", "error", err.Error())
	case !wrote:
		notAccepted("somebody decided the change meanwhile, and it is as they left it")
	default:
		log.InfoContext(ctx, "mcp_held_definition_accepted", slices.Concat(attrs, []any{"by", by})...)
	}
}

// listedUnder is the tool raw as the first live listing naming it under digest
// lists it, nil where none does.
func listedUnder(listings []coord.MCPListing, raw, digest string) *coord.MCPListedTool {
	for _, l := range listings {
		for _, t := range l.Tools {
			if t.Raw == raw && t.Digest == digest {
				return &t
			}
		}
	}
	return nil
}

// listedDigests is every digest the live listings name the tool raw under,
// sorted.
func listedDigests(listings []coord.MCPListing, raw string) []string {
	out := []string{}
	for _, l := range listings {
		for _, t := range l.Tools {
			if t.Raw == raw {
				out = append(out, t.Digest)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
