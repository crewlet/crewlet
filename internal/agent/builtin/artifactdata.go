package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE DATASTORE TOOLS (read_artifact_data, query_artifact_data,
// write_artifact_data, set_artifact_schema), each named for its authority row,
// registered from hooks [Register] and [OperatorTools] already append. Every
// row they return is fenced as text somebody else wrote.
//
// The artifact half (head read, decision, a write's operation and stamp, the
// presence hub) is the artifacts tools' own ([ArtifactDeps]), shared, never
// copied. The requester's rows are a ceiling too: a row is answered only when
// the person the turn acts for could read it, and a write is stamped with the
// lower of the two roles, except on the agent's own work ([dataTools.admit]).
// A read is presence in the artifact's room (viewing) and a write is editing,
// focused on the collection and document, so viewers see who edits which row.

// The datastore tools' names: each is its authority row's action.
const (
	ReadArtifactDataTool  = string(authz.ActionArtifactDataRead)
	QueryArtifactDataTool = string(authz.ActionArtifactDataQuery)
	WriteArtifactDataTool = string(authz.ActionArtifactDataWrite)
	SetArtifactSchemaTool = string(authz.ActionArtifactSchema)
)

// ArtifactDataTools are the four, reads first.
func ArtifactDataTools() []string {
	return []string{ReadArtifactDataTool, QueryArtifactDataTool, WriteArtifactDataTool,
		SetArtifactSchemaTool}
}

// ArtifactDataStore is the datastore as these tools ask it: this node's own,
// or a data node's through the estate router.
type ArtifactDataStore interface {
	Docs(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string,
		since int64, cursor string, limit int) (artifactdata.DocPage, error)
	Doc(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection,
		doc string) (artifactdata.Doc, error)
	Query(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string,
		q query.Query) (artifactdata.DocPage, error)
	Schema(ctx context.Context, id artifacts.ID) ([]artifacts.Collection, error)
	// State is every per-viewer state key the reading's record keeps.
	State(ctx context.Context, id artifacts.ID, rd artifactdata.Reading) ([]artifactdata.StateEntry, error)
	// CarriedSchema is the schema read_artifact_data hands a model, which
	// set_artifact_schema tells to send it back with its change: read whole
	// or refused where this node's build would drop part of it (estate's
	// artifactdata.schema_carried), never shown short and written back.
	CarriedSchema(ctx context.Context, id artifacts.ID) ([]artifacts.Collection, error)
	Write(ctx context.Context, c artifactdata.Call, id artifacts.ID,
		in artifacts.DataWrite) (artifactdata.WriteResult, error)
	Configure(ctx context.Context, c artifactdata.Call, id artifacts.ID,
		collections []artifacts.Collection) (artifactdata.WriteResult, error)
}

// ArtifactPresence is this node's presence hub as these tools touch it.
type ArtifactPresence interface {
	Touch(room presence.Room, a presence.Agent)
}

// ArtifactDataDeps are what the datastore tools act through beyond the
// artifacts tools' own half (whose presence hub they share). The zero value
// serves none of them.
type ArtifactDataDeps struct {
	// Store is the datastore.
	Store ArtifactDataStore

	// Turns is this node's tally of each turn segment's datastore ops
	// ([artifacts.TurnDataOps] at most), shared by every registry the node
	// builds. Required with Store: without it a looping turn could rewrite a
	// sheet a thousand times over.
	Turns *DataTurns
}

// dataTools is one registry's datastore tools: the artifacts half they read
// and decide through, and the datastore half.
type dataTools struct {
	art  ArtifactDeps
	data ArtifactDataDeps
}

// serves reports whether everything the tools act through is wired.
func (d dataTools) serves() bool {
	// THE WRITER TOO, for the reads: an administrator's override is
	// recorded through it before a row is shown.
	a := d.art
	return a.Reader != nil && a.Writer != nil && a.Chart != nil && a.Requesters != nil &&
		a.Limits != nil && d.data.Store != nil && d.data.Turns != nil
}

// artifactDataCandidates are the datastore tools a seat's registry may offer.
func artifactDataCandidates(deps Deps) []candidate {
	a := deps.Artifacts
	a.work, a.compact = deps.Work, deps.Compact
	return artifactDataToolSet(dataTools{art: a, data: deps.ArtifactData})
}

// artifactDataOperatorCandidates are the datastore tools the operator catalogue
// may offer.
func artifactDataOperatorCandidates(deps OperatorDeps) []candidate {
	a := deps.Artifacts
	a.work = deps.Work
	if a.Org == nil {
		a.Org = deps.Org
	}
	return artifactDataToolSet(dataTools{art: a, data: deps.ArtifactData})
}

func artifactDataToolSet(d dataTools) []candidate {
	on := d.serves()
	return []candidate{
		{&readArtifactData{d: d}, on},
		{&queryArtifactData{d: d}, on},
		{&writeArtifactData{d: d}, on},
		{&setArtifactSchema{d: d}, on},
	}
}

// artifactDataAnnotations classifies one datastore tool; false for a name that
// is not one.
func artifactDataAnnotations(name string) (tools.Annotations, bool) {
	switch name {
	case ReadArtifactDataTool, QueryArtifactDataTool:
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}, true
	case WriteArtifactDataTool:
		// DESTRUCTIVE: a set replaces a row's fields and a delete
		// tombstones it, and what was there survives only in the history
		// (and an undo, which is a person's gesture). OPEN-WORLD: a sheet
		// is read by everybody it is shared with.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes}, true
	case SetArtifactSchemaTool:
		// DESTRUCTIVE: a column removed or retyped rewrites the rows that
		// held it. IDEMPOTENT: the same schema again changes nothing.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.Yes, Idempotent: mcp.Yes,
			OpenWorld: mcp.Yes}, true
	}
	return tools.Annotations{}, false
}

// ---- one call's admission ------------------------------------------------ //

// dataAdmission is what a datastore call was admitted with: the artifact, the
// caller, the decision, the role a write is stamped with, and the reading a
// row the requester could not read is filtered by.
type dataAdmission struct {
	head artifactsdomain.Head
	ref  string
	c    artifactCaller
	dec  authz.Decision

	// role is the caller's own role on the artifact: what a read is made
	// at, before the requester's rows are taken out of it.
	role artifacts.Role

	// stamp is the role a write is stamped with: role, lowered to the
	// requester's where the turn acts for one.
	stamp artifacts.Role

	// requester is the reading of the person the turn acts for, nil where
	// it acts for nobody: a row is answered only when it reads too. A
	// stranger's, or a requester's this node could not settle on an edit to
	// the agent's own work, is the zero reading, which reads no row.
	requester *artifactdata.Reading
}

// reading is the reading a datastore read is made at.
func (a dataAdmission) reading() artifactdata.Reading {
	return artifactdata.Reading{Record: a.c.self(), Role: a.role, Fresh: seatRead}
}

// admit reads the artifact a call names and decides action on it for the
// caller and whoever the turn is acting for, answering the admission or the
// refusal the call is answered with.
func (d dataTools) admit(ctx context.Context, turn *turnctx.Turn, tool string,
	action authz.Action, args map[string]any, writes bool) (dataAdmission, *tools.Result) {

	c, refusal := d.art.caller(ctx, turn, tool, args, writes)
	if refusal != nil {
		return dataAdmission{}, refusal
	}
	h, ref, refusal := d.art.head(ctx, tool, args)
	if refusal != nil {
		return dataAdmission{}, refusal
	}
	dec, refusal := d.art.decide(ctx, c, tool, action, h, ref)
	if refusal != nil {
		return dataAdmission{}, refusal
	}
	// A READ IN THE TRASH IS NO READ: the datastore is the artifact's
	// content (D03), which stops being served the moment it is trashed.
	// A write is refused on the same status by the tool, naming the write.
	if !writes && h.Status == artifactsdomain.StatusTrashed {
		r := artifactTrashed(tool, ref, h)
		return dataAdmission{}, &r
	}
	a := dataAdmission{head: h, ref: ref, c: c, dec: dec}
	object := d.art.objectOf(h)
	// THE ROLE ITSELF, exact: an unsettled role stamped into a write would
	// have the applier refuse what the caller may do, or admit what they may
	// not. A verb only the admin path admitted acts at its decision's
	// standing (owner, recorded as elevated), not at the share the admin holds.
	standing, err := dec.Standing, error(nil)
	if !dec.Elevated {
		standing, err = authz.StandingOn(ctx, c.principal, object, d.art.Chart, d.art.now())
	}
	switch {
	case err != nil:
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, err),
			fmt.Sprintf("%s could not settle your role on the artifact right now (the "+
				"company chart could not be read). Nothing was read or changed. Try "+
				"again shortly.", tool))
		return dataAdmission{}, &r
	case standing.Role == "":
		r := artifactNotFound(tool, ref)
		return dataAdmission{}, &r
	}
	a.role, a.stamp = standing.Role, standing.Role
	// AN ADMINISTRATOR'S OVERRIDE IS RECORDED BEFORE A ROW IS SHOWN (D09):
	// a read only the admin path admitted writes the elevated-read record
	// first, and is not made when the record cannot be written.
	if !writes && dec.Elevated {
		if err := d.art.readElevated(ctx, c, dec, h, h.HeadNumber); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			r := elevatedUnrecorded(tool, err)
			return dataAdmission{}, &r
		}
	}
	switch c.ceiling.Kind {
	case authz.CeilingStranger:
		// A STRANGER READS NO ROW. Only an edit to the agent's own work is
		// admitted for one, and a refusal of it hands back nothing.
		a.requester = &artifactdata.Reading{}
		return a, nil
	case authz.CeilingRequester:
	default:
		return a, nil
	}
	// AN EDIT TO THE AGENT'S OWN WORK is the agent's: its stamp is not
	// lowered to the requester's. The requester's reading still bounds every
	// row the call hands back (a conflict's current rows), so a write they
	// may steer never shows them a row they could not read.
	owned := ownedSteer(action, c, h)
	theirs, err := authz.StandingOn(ctx, c.ceiling.Principal, object, d.art.Chart, d.art.now())
	switch {
	case err != nil && owned:
		// THE WRITE DOES NOT WAIT ON THE REQUESTER, whom it is not bounded
		// by; what it hands back is bounded to nothing instead.
		a.requester = &artifactdata.Reading{}
		return a, nil
	case err != nil:
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, err),
			fmt.Sprintf("%s could not settle the role of the person this turn is working "+
				"for on the artifact right now. Nothing was read or changed. Try again "+
				"shortly.", tool))
		return dataAdmission{}, &r
	}
	if !owned && theirs.Role.Rank() < a.stamp.Rank() {
		a.stamp = theirs.Role
	}
	reading := artifactdata.Reading{Record: requesterRecord(c), Role: theirs.Role}
	a.requester = &reading
	return a, nil
}

// ownedSteer reports whether action on h is the agent steering its own work,
// which the requester does not bound ([authz.DecideWithin]'s own exception).
func ownedSteer(action authz.Action, c artifactCaller, h artifactsdomain.Head) bool {
	steerable, _ := authz.SteerableOf(action)
	return steerable && h.Owner != "" && h.Owner == c.self()
}

// requesterRecord is the record a requester's own rows are kept under.
func requesterRecord(c artifactCaller) string {
	p := c.ceiling.Principal
	if p.Seat != "" {
		return p.Seat
	}
	return p.Login
}

// touch puts a seat calling on id in the artifact's room, through the
// artifacts half's presence ([ArtifactDeps.present]).
func (d dataTools) touch(c artifactCaller, id artifacts.ID, activity presence.Activity,
	focus presence.Focus) {

	d.art.present(c, id, activity, focus)
}

// dataFailure explains a datastore call the domain or the transport refused
// before it was answered.
func dataFailure(ctx context.Context, tool string, err error) tools.Result {
	var limited *artifactdata.RateLimited
	var turn *DataTurnQuota
	switch {
	case errors.As(err, &turn):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s refused that "+
			"(turn_quota): this turn has written %d of its %d datastore ops. Stop writing, "+
			"report what was done and what is left, and let the next turn continue.",
			tool, turn.Used, turn.Limit))
	case errors.As(err, &limited):
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s refused that for "+
			"now: this node's write budget for the datastore is spent (%s). Nothing was "+
			"written. Wait %s and send the same call again.", tool, limited.Scope,
			limited.RetryAfter.Round(time.Second)))
	case errors.Is(err, artifactdata.ErrInvalid), errors.Is(err, artifactdata.ErrBadCursor),
		errors.Is(err, artifactdata.ErrMigrationTooLarge), errors.Is(err, query.ErrInvalid),
		errors.Is(err, query.ErrNotIndexed):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused that: %s", tool,
			artifactdata.Sentence(err)))
	case errors.Is(err, artifactdata.ErrForbidden):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s refused that: %s", tool,
			artifactdata.Sentence(err)))
	case errors.Is(err, artifactdata.ErrNotFound):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: there is no such "+
			"collection or document you can read. Read the schema with %s and no "+
			"`collection` to see what the datastore holds.", tool, ReadArtifactDataTool))
	case errors.Is(err, artifactdata.ErrGone):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: the artifact's "+
			"datastore was purged with it.", tool))
	case errors.Is(err, artifactdata.ErrConflict):
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s could not land: "+
			"somebody kept writing the datastore under this schema change. Read the "+
			"schema again and send the change once more.", tool))
	}
	if class, ok := WriterClass(err); ok {
		// ANY OTHER REFUSAL A DOMAIN DECIDED, in its own class — see
		// [writeFailure]'s same arm.
		return refusedBy(class, err, domainRefusal(tool, err))
	}
	return dataConditionOrFault(ctx, tool, err)
}

// dataConditionOrFault is a failure no datastore sentinel claimed: a condition
// of this node, which waiting clears, or a fault, whose words are the log's.
func dataConditionOrFault(ctx context.Context, tool string, err error) tools.Result {
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not reach the "+
			"datastore%s (%s). This is NOT an empty result and nothing was "+
			"changed. %s", tool, rightNow(err), why,
			conditionNext(err, "Try again shortly.", stuckWrite)))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s failed: %s. This is NOT an empty "+
		"result, and nothing is known to have changed: say it could not be done.", tool,
		faultSaid))
}

// ---- the turn guardrail -------------------------------------------------- //

// DataTurns is this node's tally of each turn segment's datastore ops: at most
// [artifacts.TurnDataOps] per segment, every op of a write counted, once per
// operation, so a turn retrying a call it lost the answer to is not charged
// twice. A segment is named by its run id, as the publish guardrails are; a
// node-local tally suffices because a turn and all its tool calls run on its
// seat's node. A call outside a turn (a person's assistant) is bound only by
// the datastore's own rate limits.
type DataTurns struct{ budget *turnBudget }

// NewDataTurns is an empty tally on now; nil is the wall clock.
func NewDataTurns(now func() time.Time) *DataTurns {
	return &DataTurns{budget: newTurnBudget(artifacts.TurnDataOps, now)}
}

// DataTurnQuota is a write refused because its turn segment has spent its
// datastore ops.
type DataTurnQuota struct {
	Limit int
	Used  int
	Asked int
}

func (e *DataTurnQuota) Error() string {
	return fmt.Sprintf("builtin: the turn has written %d of its %d datastore ops and asked "+
		"for %d more", e.Used, e.Limit, e.Asked)
}

// spend charges n ops to turn under the operation key, refused past the
// bound. An empty turn is a call outside one, charged nothing; a key already
// charged is a retry, charged nothing again.
func (g *DataTurns) spend(turn, key string, n int) error {
	if _, used, ok := g.budget.spend(turn, key, n); !ok {
		return &DataTurnQuota{Limit: artifacts.TurnDataOps, Used: used, Asked: n}
	}
	return nil
}

// collectionRule is the read rule of the named collection in schema, false
// for one the schema does not hold.
func collectionRule(schema []artifacts.Collection, name string) (artifacts.ReadRule, bool) {
	i := slices.IndexFunc(schema, func(c artifacts.Collection) bool { return c.Name == name })
	if i < 0 {
		return "", false
	}
	return schema[i].Read, true
}
