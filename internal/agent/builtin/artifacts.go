package builtin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// THE ARTIFACTS TOOLS: one tool per action, each named for its authority
// row (list_artifacts, get_artifact, list_dashboard_sources,
// get_artifact_format, get_company_brand, create_artifact, publish_artifact, update_artifact,
// share_artifact, unshare_artifact, upload_artifact_asset,
// create_artifact_folder, move_artifact). Registered for seats and in the
// operator catalogue from the hooks below, which [Register] and
// [OperatorTools] already append.
//
// Nothing that takes an artifact out of circulation is a tool: trash, restore,
// purge and transfer are a person's gestures on the HTTP surface, so a
// prompt-injected agent cannot take a team's sheet offline. Nor is `force`:
// discarding a colleague's newer version is a present person's decision
// (`artifacts.publish.force`), so a stale publish is answered with the live
// version to merge onto and nothing else. Nor is reorganising a team's
// folders: renaming, moving, removing and unfiling are a folder owner's
// gestures (`artifacts.folder.*`), and adopting the brand an administrator's
// (`artifacts.brand.*`). An agent files its own work and creates subfolders
// inside a tree somebody made.
//
// # Every verb is decided twice inside a turn somebody woke
//
// The registration gate admits a call on the table's app rows; the stored
// artifact (its owner, its shares, whether an agent owns it) decides the rest,
// so each tool reads the row and asks [authz.DecideWithin] on it: the acting
// principal first, then the principal the turn is acting for (the requester
// ceiling). The agent never discloses an artifact to a requester who could not
// open it and never writes for one what they could not write, except the edits
// it makes to an artifact it owns, which anybody who can message it steers. A
// caller with no relation at all is told the artifact does not exist, in
// exactly the words an id nothing holds is answered with.
//
// # Every write goes through the one publish pipeline
//
// Bytes reach a version only through internal/artifactupload: inline content
// rides the call (at most [artifacts.MaxInlineBytes] decoded), a coding run's
// box and a project's files are streamed into an upload the pipeline reserved
// first (a sandbox is never handed a presigned URL), and the pipeline holds
// every turn to its guardrails ([artifacts.TurnPublishes],
// [artifacts.TurnPublishesPerArtifact], [artifacts.TurnBytesWritten]) before a
// byte moves.
//
// # Publishing delivers nowhere
//
// A person who asked in a chat thread for a review sheet is waiting in that
// thread: the seat publishes, then posts the `url` every answer carries with
// its chat tools, and the delivery check sends the turn back if it only
// published. So no artifact tool declares a delivery surface. The comment and
// watch wakes of a later release arrive as changefeed-derived wakes on the
// artifacts log (ADR-0012), and the tool that answers one on the artifact
// itself is what will declare the `artifacts` surface.

// The artifacts tools' names: each is its authority row's action.
const (
	ListArtifactsTool       = string(authz.ActionArtifactList)
	GetArtifactTool         = string(authz.ActionArtifactRead)
	DashboardSourcesTool    = string(authz.ActionDashboardSources)
	ArtifactFormatTool      = string(authz.ActionArtifactFormat)
	CreateArtifactTool      = string(authz.ActionArtifactCreate)
	PublishArtifactTool     = string(authz.ActionArtifactPublish)
	UpdateArtifactTool      = string(authz.ActionArtifactUpdate)
	ShareArtifactTool       = string(authz.ActionArtifactShare)
	UnshareArtifactTool     = string(authz.ActionArtifactUnshare)
	UploadArtifactAssetTool = string(authz.ActionArtifactAsset)

	CompanyBrandTool         = string(authz.ActionBrandRead)
	CreateArtifactFolderTool = string(authz.ActionArtifactFolderCreate)
	MoveArtifactTool         = string(authz.ActionArtifactMove)
)

// ArtifactTools are the thirteen, reads first.
func ArtifactTools() []string {
	return []string{ListArtifactsTool, GetArtifactTool, DashboardSourcesTool, ArtifactFormatTool, CompanyBrandTool,
		CreateArtifactTool, PublishArtifactTool, UpdateArtifactTool, ShareArtifactTool,
		UnshareArtifactTool, UploadArtifactAssetTool, CreateArtifactFolderTool, MoveArtifactTool}
}

// ArtifactReader is the artifacts domain's read side as these tools ask it,
// this node's own or a data node's through the estate router.
type ArtifactReader interface {
	Artifact(ctx context.Context, id artifacts.ID, fresh statelog.Freshness) (artifactsdomain.Head, statelog.Position, error)
	List(ctx context.Context, v artifactsdomain.Viewer, q artifactsdomain.ListQuery) (artifactsdomain.ListPage, error)
	Version(ctx context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error)
	Versions(ctx context.Context, id artifacts.ID, cursor string, limit int) (artifactsdomain.VersionPage, error)

	// Folders is every live folder v can see, and ResolveFolder one folder
	// by id or path among those every viewer in within can see.
	Folders(ctx context.Context, v artifactsdomain.Viewer, fresh statelog.Freshness) (artifactsdomain.FolderTree, error)
	ResolveFolder(ctx context.Context, ref string, within []artifactsdomain.Viewer,
		fresh statelog.Freshness) (artifactsdomain.Folder, error)

	// Brand is the company's adopted brand, false when none is adopted.
	Brand(ctx context.Context, fresh statelog.Freshness) (artifactsdomain.Adoption, bool, error)
}

// ArtifactWriter is the domain's write side for the writes that move no
// bytes: a patch of the displayed fields, the shares, the record of an
// administrator's elevated read, a folder chain made, and an artifact filed.
type ArtifactWriter interface {
	Patch(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, p artifactsdomain.Patch) (artifactsdomain.Written, error)
	Share(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, s artifacts.Share) (artifactsdomain.Written, error)
	Unshare(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, a artifacts.Audience) (artifactsdomain.Written, error)
	ReadElevated(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, version int) (artifactsdomain.Written, error)
	CreateFolders(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.FolderCreateInput) (
		artifactsdomain.FolderCreated, error)
	Move(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, folder string) (artifactsdomain.Written, error)
}

// ArtifactPublisher is THE publish pipeline (internal/artifactupload's
// Service) as these tools call it: every byte a version or an asset holds goes
// through it, and a file's text is read back through it in the form an edit is
// written against.
type ArtifactPublisher interface {
	Intend(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, req artifacts.UploadRequest) (artifacts.UploadIntent, error)
	Transfer(ctx context.Context, c artifactsdomain.Call, upload, path string, body io.Reader, size int64) error
	Create(ctx context.Context, c artifactsdomain.Call, req artifactupload.CreateRequest) (artifactsdomain.Published, artifactupload.Report, error)
	Publish(ctx context.Context, c artifactsdomain.Call, req artifactupload.PublishRequest) (artifactsdomain.Published, artifactupload.Report, error)
	AddAsset(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, upload, name string) (artifactsdomain.Asset, error)
	SourceText(ctx context.Context, e artifacts.Entry, limit int64) ([]byte, bool, error)
}

// ArtifactBoxes finds the box the turn's coding run works in, for a publish
// that takes its files from it.
type ArtifactBoxes interface {
	// Box is the box of the coding run working in this turn, attached for
	// reading while it works: the run publishes its own build, by calling. It
	// answers [ErrNoBox] when the turn has none it may read: no run, a run
	// that is not working in its box (parked on a question, collected, its
	// pause reaped), or a box already torn down.
	Box(ctx context.Context, turn *turnctx.Turn) (ArtifactBox, error)
}

// ArtifactBox is one coding run's box as a publish reads it. Every path is
// relative to the run's workspace (the coding agent's working directory) and
// slash-separated.
type ArtifactBox interface {
	// Files lists every regular file under dir, at most max of them. The
	// listing is the box's and is treated as untrusted: every path is held
	// to the workspace and the published-path grammar again by the caller.
	Files(ctx context.Context, dir string, max int) ([]string, error)

	// Open streams one regular file. A path that names anything else is
	// refused by the box.
	Open(ctx context.Context, path string) (io.ReadCloser, error)
}

// ErrNoBox reports a turn with no coding run's box a publish may read.
var ErrNoBox = errors.New("builtin: this turn holds no coding run's box to publish from")

// ArtifactRequesters composes the principal a seat's turn is acting for.
type ArtifactRequesters interface {
	// Requester is the seat handle's principal as the authority table
	// decides it: the person bound to a human seat, or the agent seat
	// itself. An error is UNKNOWN, never "nobody".
	Requester(ctx context.Context, handle string) (iam.Principal, error)
}

// ArtifactDeps are what the artifacts tools act through. The zero value
// serves none of them, which is what a node built without the artifacts
// domain has.
type ArtifactDeps struct {
	Reader    ArtifactReader
	Writer    ArtifactWriter
	Publisher ArtifactPublisher

	// Chart answers the relations a share, a lead and a unit ask, three-
	// valued, read per call ([authz.Chart]).
	Chart authz.Chart

	// Requesters composes who a turn is acting for (the requester ceiling).
	Requesters ArtifactRequesters

	// Limits are the Tier A quotas this node stamps into every write.
	Limits func() artifacts.Limits

	// ManageOthers is the company's `artifacts.agents_manage_others` NOW.
	// Nil reads false, the closed end.
	ManageOthers func() bool

	// Boxes reads a coding run's box. Nil refuses every publish from a
	// sandbox, which is a node with no sandbox runtime.
	Boxes ArtifactBoxes

	// Presence puts a seat that reads or writes an artifact in the
	// artifact's room, as an agent, viewing or editing. Nil tells nobody,
	// which is a node with no presence hub.
	Presence ArtifactPresence

	// Link is the dashboard's address of an artifact, built on
	// `api.external_url`, or empty where the deployment names none. Every
	// answer that names an artifact carries it, so an agent composes no
	// address of its own.
	Link func(artifacts.ID) string

	// Org is the company chart now, for a caller with no turn to read one
	// from (the operator surface).
	Org func() *org.Organization

	// CallerKey is the operation key the call's transport carries outside
	// a turn (an Idempotency-Key), or empty.
	CallerKey func(ctx context.Context) string

	// Await is handed the position a write landed at, so the next read
	// sees it.
	Await func(ctx context.Context, at statelog.Position) error

	// Now is the clock decisions are taken at; nil is the wall clock.
	Now func() time.Time

	// KitVersion is the newest artifact kit release this build embeds, the
	// one a page linked now is sure to be served at: what get_company_brand
	// answers beside the brand. Nil, or empty, answers none.
	KitVersion func() string

	// FolderTurns is this node's tally of the folder records each agent turn
	// segment writes, shared by every registry the node builds. Nil serves
	// no create_artifact_folder: a create path with no guardrail would let a
	// looping turn bury the company's tree.
	FolderTurns *FolderTurns

	// Set by [Register] and [OperatorTools] from the deps around them.
	work             WorkDeps
	compact          *compact.Compactor
	authorize        Authorizer
	callerOperations bool
}

// serves reports whether every half the tools act through is wired. They are
// wired together or not at all: a node that could list artifacts and not
// publish one would offer a seat a catalogue it cannot complete.
func (d ArtifactDeps) serves() bool {
	return d.Reader != nil && d.Writer != nil && d.Publisher != nil && d.Chart != nil &&
		d.Requesters != nil && d.Limits != nil
}

func (d ArtifactDeps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now()
}

func (d ArtifactDeps) manageOthers() bool { return d.ManageOthers != nil && d.ManageOthers() }

func (d ArtifactDeps) link(id artifacts.ID) string {
	if d.Link == nil {
		return ""
	}
	return d.Link(id)
}

// present puts a seat's call on artifact id in the artifact's room, after the
// call was decided: an agent reading it is viewing and one writing it editing,
// focused where the call named. A person's call (an operator's assistant) is
// not presence: their own tab is.
func (d ArtifactDeps) present(c artifactCaller, id artifacts.ID,
	activity artifacts.PresenceActivity, focus artifacts.Focus) {

	if d.Presence == nil || !c.agent() || c.principal.Seat == "" {
		return
	}
	turn := ""
	if c.turn != nil {
		turn = c.turn.RunID
	}
	d.Presence.Touch(id, c.principal.Seat, turn, activity, focus)
}

// orgFor is the chart a call reads: the turn's, or the surface's own.
func (d ArtifactDeps) orgFor(turn *turnctx.Turn) *org.Organization {
	if turn != nil && turn.Org != nil {
		return turn.Org
	}
	if d.Org != nil {
		return d.Org()
	}
	return nil
}

// artifactCandidates are the artifacts tools a seat's registry may offer.
func artifactCandidates(deps Deps) []candidate {
	a := deps.Artifacts
	a.work, a.compact, a.authorize = deps.Work, deps.Compact, deps.Authorize
	return artifactToolSet(a)
}

// artifactOperatorCandidates are the artifacts tools the operator catalogue may
// offer a person and their assistant.
func artifactOperatorCandidates(deps OperatorDeps) []candidate {
	a := deps.Artifacts
	a.work, a.authorize = deps.Work, deps.Authorize
	// A CALL HERE HOLDS ITS OWN OPERATION, as the tracker's do: there is no
	// turn to derive one from, so a write answers with its `op_id` and a
	// call bringing it back is that operation again.
	a.callerOperations = true
	if a.Org == nil {
		a.Org = deps.Org
	}
	return artifactToolSet(a)
}

func artifactToolSet(a ArtifactDeps) []candidate {
	on := a.serves()
	return []candidate{
		{&listArtifacts{deps: a}, on},
		{&getArtifact{deps: a}, on},
		{&dashboardSources{}, on},
		{&artifactFormat{}, on},
		{&companyBrand{deps: a}, on},
		{&createArtifact{deps: a}, on},
		{&publishArtifact{deps: a}, on},
		{&updateArtifact{deps: a}, on},
		{&shareArtifact{deps: a}, on},
		{&unshareArtifact{deps: a}, on},
		{&uploadArtifactAsset{deps: a}, on},
		{&createArtifactFolder{deps: a}, on && a.FolderTurns != nil},
		{&moveArtifact{deps: a}, on},
	}
}

// artifactAnnotations classifies one artifacts tool for the delivery gate and
// the sub-agent guard; false for a name that is not one.
//
// EVERY WRITE IS OPEN-WORLD: an artifact is read by the people it is shared
// with, so a worker acting under its parent's name is kept off every one of
// them ([mcp.WritesToSharedSurface]), and handed the five reads.
func artifactAnnotations(name string) (tools.Annotations, bool) {
	switch name {
	case ListArtifactsTool, GetArtifactTool, DashboardSourcesTool, ArtifactFormatTool, CompanyBrandTool:
		// Reads, and idempotent. get_artifact may condense a long file a
		// colleague wrote through the auxiliary model, which is a read's
		// cost the way query_episodes' condensing is, and writes nothing a
		// person reads (an administrator's elevated-read record is the
		// engine's audit, never a shared surface).
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}, true
	case CreateArtifactTool, UploadArtifactAssetTool:
		// Additive, and NOT idempotent as tools: a second call with other
		// content is a second artifact or a second asset. A re-run turn's
		// call collapses onto the first, which is the engine's protection
		// rather than the tool's shape.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}, true
	case PublishArtifactTool:
		// NOT DESTRUCTIVE: every version is kept and a rollback is a
		// publish of `restore_version`, and a publish over a version
		// somebody else made in the meantime is refused rather than
		// discarding it. Not idempotent: a second publish is a second
		// version.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}, true
	case UpdateArtifactTool:
		// DESTRUCTIVE for update_work_item's reason: it replaces a title, a
		// description or an icon somebody chose, and the previous value
		// survives only in the history.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes}, true
	case ShareArtifactTool, UnshareArtifactTool:
		// IDEMPOTENT: the same share again, or an absent one withdrawn,
		// writes nothing. Not destructive: an audience withdrawn is shared
		// again by the same call, and nothing anybody wrote is lost.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes}, true
	case CreateArtifactFolderTool, MoveArtifactTool:
		// IDEMPOTENT: a chain that already exists is answered as it is, and
		// an artifact already in its folder is moved nowhere. NOT
		// DESTRUCTIVE: a folder is made and nothing removed, and filing
		// never changes who may open anything (a folder grants nothing), so
		// the previous place is one more move away. Open-world all the same:
		// a team finds its work where these put it.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes}, true
	}
	return tools.Annotations{}, false
}

// ---- who a call is, and what it may do ---------------------------------- //

// artifactCaller is one call's acting party: who it is, who the turn is acting
// for, and the operation its writes are named by.
type artifactCaller struct {
	principal iam.Principal
	actor     iam.Actor
	turn      *turnctx.Turn
	ceiling   authz.Ceiling

	// key is the operation key every record the call writes is derived
	// from, and opID the `op_id` the answer hands back on the operator
	// surface (empty elsewhere).
	key  string
	opID string
}

// self is the handle provenance compares a writer against: the one writer
// whose text reads back as the caller's own.
func (c artifactCaller) self() string { return iam.RecordOwner(c.principal) }

// agent reports a seat's own call, which is what the per-turn guardrails and
// the agent-only rules are about.
func (c artifactCaller) agent() bool { return c.principal.Kind == iam.KindSeat }

// artifactCallNamespace scopes the derivation of a turn's artifact operation
// keys. FIXED for the life of the format: a new one would make every re-run
// turn's publish a second version.
const artifactCallNamespace = "crewlet.artifacts.call"

// caller resolves who this call is, or the refusal it is answered with.
func (d ArtifactDeps) caller(ctx context.Context, turn *turnctx.Turn, tool string,
	args map[string]any, writes bool) (artifactCaller, *tools.Result) {

	p, how := iam.From(ctx)
	if how != iam.Resolved {
		r := notInATurn(tool)
		return artifactCaller{}, &r
	}
	c := artifactCaller{principal: p, actor: iam.ActorFor(p), turn: turn}
	ceiling, err := d.ceilingFor(ctx, turn, p)
	if err != nil {
		r := refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not "+
			"establish who asked for this turn's work, which every artifact verb "+
			"is decided for as well as for you. Nothing was read or changed. "+
			"Try again shortly.", tool))
		return artifactCaller{}, &r
	}
	c.ceiling = ceiling
	if writes {
		key, opID, refusal := d.operation(ctx, turn, tool, args)
		if refusal != "" {
			r := failed(refusal)
			return artifactCaller{}, &r
		}
		c.key, c.opID = key, opID
	}
	return c, nil
}

// ceilingFor is who a seat's turn is acting for: nobody beyond the agent for a
// turn nobody woke and for any caller who is not a seat (a person acting
// through their own credential is no deputy), the requester a known principal
// woke it as, or a stranger.
func (d ArtifactDeps) ceilingFor(ctx context.Context, turn *turnctx.Turn,
	p iam.Principal) (authz.Ceiling, error) {

	if p.Kind != iam.KindSeat || turn == nil {
		return authz.Ceiling{Kind: authz.CeilingNone}, nil
	}
	if handle := strings.TrimSpace(turn.Requester); handle != "" && handle != p.Seat {
		requester, err := d.Requesters.Requester(ctx, handle)
		if err != nil {
			return authz.Ceiling{}, fmt.Errorf("builtin: compose requester %s: %w", handle, err)
		}
		return authz.Ceiling{Kind: authz.CeilingRequester, Principal: requester}, nil
	}
	if len(turn.Senders) > 0 {
		return authz.Ceiling{Kind: authz.CeilingStranger}, nil
	}
	return authz.Ceiling{Kind: authz.CeilingNone}, nil
}

// operation settles the operation key a write's records derive from, the
// `op_id` its answer carries, or the refusal of one brought back.
//
//   - IN A TURN, a key derived from the turn's seed, the tool and this call's
//     repeat count in the run ([pageOpKey]'s rule), so a re-run turn's publish
//     is the first run's and lands once;
//   - OUTSIDE ONE, the key the caller's transport carries, as it was sent;
//   - on the operator's MCP, which carries none, the `op_id` an earlier answer
//     handed back for exactly this call, or a fresh one the answer hands out.
func (d ArtifactDeps) operation(ctx context.Context, turn *turnctx.Turn, tool string,
	args map[string]any) (key, opID, refusal string) {

	raw, brought := args[opIDArg]
	if text, _ := argval.String(raw); text == "" {
		brought = false
	}
	if seed := turnKey(turn); seed != "" {
		if brought {
			return "", "", fmt.Sprintf("%s takes no `op_id` in a turn: the turn "+
				"names the operation, so the same call made again is already the "+
				"same publish. Leave it out.", tool)
		}
		identity := []string{artifactCallNamespace, seed, tool}
		if n := turn.CallLog().Ordinal(tool, args); n > 0 {
			identity = append(identity, "repeat:"+strconv.Itoa(n))
		}
		return statelog.DeriveOpID(turnSince(turn), "", identity...), "", ""
	}
	if d.CallerKey != nil {
		if k := d.CallerKey(ctx); k != "" {
			if brought {
				return "", "", fmt.Sprintf("%s takes no `op_id` beside the request's "+
					"own operation key: send the same request again to repeat it.", tool)
			}
			return k, "", ""
		}
	}
	if !d.callerOperations {
		if brought {
			return "", "", fmt.Sprintf("%s takes no `op_id` here. Leave it out.", tool)
		}
		return "", "", ""
	}
	call := callName(tool, args)
	if !brought {
		op := statelog.NewOpID(d.now(), call)
		return op, op, ""
	}
	op, _ := argval.String(raw)
	if err := statelog.CheckCallerOpID(op); err != nil {
		return "", "", fmt.Sprintf("%s refused that `op_id`: %v", tool, err)
	}
	if !answeredFor(op, call) {
		return "", "", fmt.Sprintf("%s refused that `op_id`, and nothing was written: "+
			"it was answered for another call, another tool or this one with other "+
			"arguments. To finish that call, send its arguments unchanged with it; to "+
			"make this one, leave `op_id` out.", tool)
	}
	return op, op, ""
}

// writeParams adds the operator surface's `op_id` to a write tool's schema,
// on the surface whose callers bring one back and nowhere else.
func (d ArtifactDeps) writeParams(schema map[string]any) map[string]any {
	if !d.callerOperations {
		return schema
	}
	props, _ := schema["properties"].(map[string]any)
	props[opIDArg] = map[string]any{
		"type": "string",
		"description": "Leave it out for a new write. To finish one an earlier " +
			"answer from this tool left `unknown`, send back the `op_id` that " +
			"answer carried, unchanged, with exactly the same arguments.",
	}
	return schema
}

// objectOf is a stored artifact as the authority table decides it.
func (d ArtifactDeps) objectOf(h artifactsdomain.Head) authz.Object {
	return authz.Object{
		Kind: authz.KindArtifact, ID: h.ID.String(), Owner: h.Owner,
		Shares: h.Shares, OwnerIsAgent: h.OwnerKind == iam.ActorAgent,
		AgentsManageOthers: d.manageOthers(),
	}
}

// decide asks action of artifact h for the caller and the principal the turn
// is acting for, answering the decision or the refusal the call is answered
// with. A caller the artifact is not shared with is told it does not exist,
// in exactly the words an id nothing holds is answered with.
func (d ArtifactDeps) decide(ctx context.Context, c artifactCaller, tool string,
	action authz.Action, h artifactsdomain.Head, ref string) (authz.Decision, *tools.Result) {

	dec := authz.DecideWithin(ctx, c.principal, c.ceiling, action, d.objectOf(h), d.Chart, d.now())
	switch {
	case dec.Unknown():
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, dec.Err),
			fmt.Sprintf("%s could not check access to the artifact right now (the "+
				"company chart or the requester could not be read). Nothing was read "+
				"or changed. Try again shortly.", tool))
		return dec, &r
	case dec.Allowed:
		return dec, nil
	case dec.NotFound():
		r := artifactNotFound(tool, ref)
		return dec, &r
	}
	r := authorityRefusal(tool, DecisionError(action, dec))
	switch dec.Reason {
	case authz.ReasonCeiling:
		r.Output = fmt.Sprintf("%s didn't run: the person this turn is working for "+
			"could not do that to this artifact themselves, so you may not do it for "+
			"them. Tell them what they would need, or ask the artifact's owner.", tool)
	case authz.ReasonSeatRefused:
		r.Output = fmt.Sprintf("%s didn't run: an agent does not publish over, "+
			"rename or reshare an artifact it does not own. Ask its owner (%s) to make "+
			"the change, or to share the work with you as an artifact of your own.",
			tool, h.Owner)
	case authz.ReasonRole:
		r.Output = fmt.Sprintf("%s didn't run: it needs the %s role on this artifact, "+
			"and you hold %s. Ask its owner (%s) for %s.", tool, dec.Role,
			roleOrNone(dec.Standing.Role), h.Owner, dec.Role)
	}
	return dec, &r
}

func roleOrNone(r artifacts.Role) string {
	if r == "" {
		return "none"
	}
	return string(r)
}

// call is the caller context a write is stamped with: the actor, the role the
// decision found, whether only the admin path admitted it, the limits in
// force, the surface, and the work item the turn is charged to.
func (d ArtifactDeps) call(c artifactCaller, role artifacts.Role, elevated bool) artifactsdomain.Call {
	auth := artifacts.Authority{
		Actor: c.actor, Role: role, Elevated: elevated, Limits: d.Limits(),
		Origin: artifacts.OriginTool,
	}
	out := artifactsdomain.Call{OpKey: c.key}
	if c.turn != nil {
		if c.turn.WorkItem != nil {
			auth.WorkItem = c.turn.WorkItem.Key
		}
		// THE GUARDRAILS ARE A TURN'S, and only an agent's: a person's
		// assistant is bounded by the reservation budget instead.
		if c.agent() {
			out.TurnID = c.turn.RunID
		}
	}
	out.Authority = auth
	return out
}

// ---- what a call may show of an artifact -------------------------------- //

// readElevated writes the elevated-read record an administrator's read owes
// when only the admin path admitted it (dec.Elevated), before anything of
// version is shown: the owner and the editors see "read by an administrator"
// in the artifact's own history (D09). An error is a record that did not land
// (or whose landing is unknown), and the content is then not shown.
func (d ArtifactDeps) readElevated(ctx context.Context, c artifactCaller, dec authz.Decision,
	h artifactsdomain.Head, version int) error {

	written, err := d.Writer.ReadElevated(ctx, d.call(c, dec.Standing.Role, true), h.ID, version)
	if err == nil && written.Result.Outcome == statelog.OutcomeUnknown {
		err = fmt.Errorf("%w: the elevated read's record", ErrOutcomeUnknown)
	}
	return err
}

// elevatedUnrecorded is the refusal of a read only the admin path admitted
// whose record could not be written: nothing is shown.
func elevatedUnrecorded(tool string, err error) tools.Result {
	return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s did not open this "+
		"artifact: you are reading it as an administrator, it is not shared with you, "+
		"and that override is recorded in the artifact's own history before anything "+
		"is shown. The record could not be written just now. Try again shortly.", tool))
}

// discloses reports whether this call may hand back what version of h holds,
// as a refusal's context (the live files a stale publish answers with, the rows
// a conflicting data write answers with): exactly when reading it would be
// admitted, for the caller and for whoever the turn is acting for
// ([authz.DecideWithin] over the read row, which no steerable exception
// reaches), with an administrator's override recorded first.
//
// A WRITE ADMITTED WITHOUT THE CEILING IS NOT A READ: an agent's edit to its
// own work skips the requester, and a refusal that inlined the work's content
// would hand a requester who could not open it exactly what reading it is
// ceilinged against (D11). So the answer is false for a requester who could
// not read it, for a turn a stranger woke, for a decision this node could not
// take, and for an override whose record could not be written; the refusal is
// then answered with numbers, authors and paths alone.
func (d ArtifactDeps) discloses(ctx context.Context, c artifactCaller, h artifactsdomain.Head,
	version int) bool {

	dec := authz.DecideWithin(ctx, c.principal, c.ceiling, authz.ActionArtifactRead,
		d.objectOf(h), d.Chart, d.now())
	if dec.Unknown() || !dec.Allowed {
		return false
	}
	if dec.Elevated {
		if err := d.readElevated(ctx, c, dec, h, version); err != nil {
			log.WarnContext(ctx, "artifact_elevated_read_unrecorded", "artifact", h.ID.String(),
				"version", version, "error", err.Error(),
				"detail", "the refusal is answered without the artifact's content")
			return false
		}
	}
	return true
}

// settle hands a write's position to [ArtifactDeps.Await], best effort, as
// [PageDeps.settle] does.
func (d ArtifactDeps) settle(ctx context.Context, at statelog.Position) {
	if d.Await == nil || at.Seq == 0 {
		return
	}
	if err := d.Await(ctx, at); err != nil {
		log.WarnContext(ctx, "artifact_write_not_applied_yet",
			"at", at.String(), "error", err.Error(),
			"detail", "the write landed on the fleet's log; the copy this surface "+
				"reads has not caught up, so a read in this same turn may show the "+
				"previous version")
	}
}

// ---- reading the artifact a call names ---------------------------------- //

// artifactRef reads the `artifact` argument: an id, the artifact's 26-character
// host label, or a dashboard address carrying `#/artifacts/<id>`.
func artifactRef(raw string) (artifacts.ID, error) {
	ref := strings.TrimSpace(raw)
	if i := strings.Index(ref, artifactAddress); i >= 0 {
		ref = ref[i+len(artifactAddress):]
		if j := strings.IndexAny(ref, "/?@#"); j >= 0 {
			ref = ref[:j]
		}
	}
	if len(ref) == artifacts.LabelLen {
		return artifacts.ParseLabel(ref)
	}
	return artifacts.ParseID(ref)
}

// artifactAddress is the dashboard's address of an artifact, before its id.
const artifactAddress = "#/artifacts/"

// head reads the artifact a call names at the seat's read level, or answers
// the refusal: a malformed reference, an artifact nothing holds, or a read
// this node could not serve.
func (d ArtifactDeps) head(ctx context.Context, tool string, args map[string]any) (
	artifactsdomain.Head, string, *tools.Result) {

	ref := strings.TrimSpace(argString(args, "artifact"))
	if ref == "" {
		r := failed(tool + " needs an `artifact`: its id, or the address an earlier answer gave.")
		return artifactsdomain.Head{}, "", &r
	}
	id, err := artifactRef(ref)
	if err != nil {
		r := failed(fmt.Sprintf("%s could not read %q as an artifact: pass the `artifact` "+
			"id an earlier answer gave, or its dashboard address.", tool, clip(ref)))
		return artifactsdomain.Head{}, ref, &r
	}
	h, _, err := d.Reader.Artifact(ctx, id, seatRead)
	if err != nil {
		r := artifactReadFailure(ctx, tool, ref, err)
		return artifactsdomain.Head{}, ref, &r
	}
	return h, ref, nil
}

// artifactNotFound is the ONE answer for an artifact nothing holds and for one
// the caller holds no relation to: the same class and the same words, so an id
// is no oracle.
func artifactNotFound(tool, ref string) tools.Result {
	return refusedBy(tools.RefusalNotFound, artifactsdomain.ErrNotFound,
		fmt.Sprintf("%s: there is no artifact %q you can open. Check the id, or "+
			"find it with list_artifacts.", tool, clip(ref)))
}

// artifactTrashed is the answer for a read of an artifact in the trash, to a
// caller it is disclosed to: its content is not served until a person
// restores it, so nothing of it is read.
func artifactTrashed(tool string, h artifactsdomain.Head) tools.Result {
	return refusedBy(tools.RefusalNotFound, artifactsdomain.ErrTrashed, fmt.Sprintf("%s: "+
		"artifact %s is in the trash, and its content is not served until its owner (%s) "+
		"restores it. Nothing was read.", tool, h.ID, h.Owner))
}

// artifactReadFailure explains a read of the artifacts domain that did not
// answer an artifact: one nothing holds, or a node that could not serve it.
func artifactReadFailure(ctx context.Context, tool, ref string, err error) tools.Result {
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return artifactNotFound(tool, ref)
	case errors.Is(err, artifactsdomain.ErrBadCursor):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused that "+
			"`continuation_token`: pass the `next_continuation_token` of the previous page "+
			"unchanged, with the same `prefix`.", tool))
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not "+
			"read the artifacts right now (%s). This is NOT an empty result: do not "+
			"conclude the artifact does not exist. Try again shortly.", tool, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not read the artifacts: %s. "+
		"This is NOT an empty result: say you could not check.", tool, faultSaid))
}

// actorJSON is how an answer names who did something: a seat handle and its
// kind, never a display name (the dashboard resolves those).
func actorJSON(a iam.Actor) map[string]any {
	out := map[string]any{"kind": string(a.Kind)}
	if a.Name != "" && (a.Kind == iam.ActorAgent || a.Kind == iam.ActorHuman) {
		out["seat"] = a.Name
	}
	return out
}
