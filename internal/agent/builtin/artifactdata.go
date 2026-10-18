package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE DATASTORE TOOLS: read_artifact_data, query_artifact_data,
// write_artifact_data and set_artifact_schema, each named for its authority
// row, registered from the hooks below, which [Register] and [OperatorTools]
// already append. Every row they return is fenced as text somebody else wrote.
//
// # The artifact half is the artifacts tools'
//
// Reading the artifact a call names, deciding a verb on it for the caller and
// for whoever the turn is acting for, deriving the operation a write is named
// by and stamping its authority are exactly what the artifacts tools do
// ([ArtifactDeps]), so these tools take that half from the same deps rather
// than a copy of it: one head read, one decision, one stamp, one presence hub.
// [ArtifactDataDeps] adds only the datastore and the turn guardrail.
//
// # The requester's rows are the ceiling too
//
// A verb is admitted only when the agent and the person the turn is acting for
// both may take it ([authz.DecideWithin]); a datastore read or write goes one
// step further, to the rows. A row an agent may read is answered only when that
// person could read it too, by the collection's own rule over their role and
// record, and a write is stamped with the lower of the two roles, so an agent
// that edits a sheet never writes a cell for somebody that the somebody could
// not write. An agent's own artifact is steered by anybody who can message it,
// as every other piece of its own work is, so on its own artifact its writes
// are its own.
//
// # An agent in the room
//
// A seat's read is presence in the artifact's room (viewing), and its write is
// editing, focused on the collection and the document the call named, so the
// people looking at the sheet see which agent is working on which row.

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
	Write(ctx context.Context, c artifactdata.Call, id artifacts.ID,
		in artifacts.DataWrite) (artifactdata.WriteResult, error)
	Configure(ctx context.Context, c artifactdata.Call, id artifacts.ID,
		collections []artifacts.Collection) (artifactdata.WriteResult, error)
}

// ArtifactPresence is this node's presence hub as these tools touch it.
type ArtifactPresence interface {
	Touch(id artifacts.ID, seat string, activity artifacts.PresenceActivity, focus artifacts.Focus)
}

// ArtifactDataDeps are what the datastore tools act through beyond the
// artifacts tools' own half (whose presence hub they share). The zero value
// serves none of them.
type ArtifactDataDeps struct {
	// Store is the datastore.
	Store ArtifactDataStore

	// Turns is this node's tally of each turn segment's datastore ops,
	// shared by every registry the node builds: at most
	// [artifacts.TurnDataOps] per segment. Required with Store: a write
	// path with no guardrail would let a looping turn rewrite a sheet a
	// thousand times over.
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
	a := d.art
	return a.Reader != nil && a.Chart != nil && a.Requesters != nil && a.Limits != nil &&
		d.data.Store != nil && d.data.Turns != nil
}

// artifactDataCandidates are the datastore tools a seat's registry may offer.
func artifactDataCandidates(deps Deps) []candidate {
	a := deps.Artifacts
	a.work, a.compact, a.authorize = deps.Work, deps.Compact, deps.Authorize
	return artifactDataToolSet(dataTools{art: a, data: deps.ArtifactData})
}

// artifactDataOperatorCandidates are the datastore tools the operator catalogue
// may offer.
func artifactDataOperatorCandidates(deps OperatorDeps) []candidate {
	a := deps.Artifacts
	a.work, a.authorize = deps.Work, deps.Authorize
	// A CALL HERE HOLDS ITS OWN OPERATION, as the artifacts tools' do.
	a.callerOperations = true
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
	// it acts for nobody (or on the agent's own artifact): a row is answered
	// only when it reads too.
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
	a := dataAdmission{head: h, ref: ref, c: c, dec: dec}
	object := d.art.objectOf(h)
	// THE ROLE ITSELF, exact: a role the chart could not settle stamped
	// into a write would have the applier refuse what the caller may do, or
	// admit what they may not. A verb only the admin path admitted acts at
	// its own decision's standing (owner, recorded as elevated); the read
	// row would stand the same administrator at the share they hold.
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
	if c.ceiling.Kind != authz.CeilingRequester || ownedSteer(action, c, h) {
		return a, nil
	}
	theirs, err := authz.StandingOn(ctx, c.ceiling.Principal, object, d.art.Chart, d.art.now())
	if err != nil {
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, err),
			fmt.Sprintf("%s could not settle the role of the person this turn is working "+
				"for on the artifact right now. Nothing was read or changed. Try again "+
				"shortly.", tool))
		return dataAdmission{}, &r
	}
	if theirs.Role.Rank() < a.stamp.Rank() {
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
func (d dataTools) touch(c artifactCaller, id artifacts.ID, activity artifacts.PresenceActivity,
	focus artifacts.Focus) {

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
			dataSentence(err)))
	case errors.Is(err, artifactdata.ErrForbidden):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s refused that: %s", tool,
			dataSentence(err)))
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
	return dataConditionOrFault(ctx, tool, err)
}

// dataConditionOrFault is a failure no datastore sentinel claimed: a condition
// of this node, which waiting clears, or a fault, whose words are the log's.
func dataConditionOrFault(ctx context.Context, tool string, err error) tools.Result {
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not reach the "+
			"datastore right now (%s). This is NOT an empty result and nothing was "+
			"changed. Try again shortly.", tool, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s failed: %s. This is NOT an empty "+
		"result, and nothing is known to have changed: say it could not be done.", tool,
		faultSaid))
}

// ---- the turn guardrail -------------------------------------------------- //

// DataTurns is this node's tally of each turn segment's datastore ops: at most
// [artifacts.TurnDataOps] per segment, every op of every write counted once per
// operation, so a turn retrying a call it lost the answer to is not charged
// twice. A segment is named by its run id, as the publish guardrails are: a
// turn runs on the node that holds its seat, and every tool call it makes is a
// call on that node. A call outside a turn (a person's assistant) is held to
// none of this; its bound is the datastore's own rate limits.
type DataTurns struct {
	now func() time.Time

	mu    sync.Mutex
	turns map[string]*dataTally
	swept time.Time
}

// dataTally is one turn's use.
type dataTally struct {
	ops     int
	keys    map[string]bool
	touched time.Time
}

// dataTurnIdle is how long an untouched turn's tally is kept: a day, past any
// turn segment, the publish guardrail's own term.
const dataTurnIdle = 24 * time.Hour

// NewDataTurns is an empty tally on now; nil is the wall clock.
func NewDataTurns(now func() time.Time) *DataTurns {
	if now == nil {
		now = time.Now
	}
	return &DataTurns{now: now, turns: map[string]*dataTally{}}
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
	if turn == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if now.Sub(g.swept) > time.Minute {
		for id, t := range g.turns {
			if now.Sub(t.touched) > dataTurnIdle {
				delete(g.turns, id)
			}
		}
		g.swept = now
	}
	t, ok := g.turns[turn]
	if !ok {
		t = &dataTally{keys: map[string]bool{}}
		g.turns[turn] = t
	}
	t.touched = now
	if key != "" && t.keys[key] {
		return nil
	}
	if t.ops+n > artifacts.TurnDataOps {
		return &DataTurnQuota{Limit: artifacts.TurnDataOps, Used: t.ops, Asked: n}
	}
	t.ops += n
	if key != "" {
		t.keys[key] = true
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
