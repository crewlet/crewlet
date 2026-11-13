package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE ATLAS'S WRITE TOOLS: how a person changes what the company's atlas says
// (ADR-0126, ADR-0127). Each is one claim, or a short run of them, on the
// atlas's log in the person's own name, so it outranks every inference and
// keeps the claim it replaces as the entry's history.
//
// # Decided on what the person reads
//
// Every write reads the atlas first, as its caller reads it (Gitlet only for a
// holder of `gitlet`, the log's claims at the person's own level), and plans
// from that: the claims to record, or nothing where what was asked already
// holds. The entries a write names are read at their version BEFORE the atlas
// is, and every claim is recorded on that version, so a change somebody made
// after the read refuses the write rather than being written over.
//
// # What a person may not change here
//
// A fact the company's settings or the engine's own records declare is
// changed where it is declared, never here: the two share the top rank, and a
// claim beside a declaration would decide by whichever was newer. So is an
// entry the engine itself names (a server the SSH settings declare, a tool
// server, a model, a Gitlet repository), which is never added by hand. And a
// Gitlet repository is changed only by a holder of `gitlet` who can read it,
// since the atlas is never a way round another permission.
//
// # Never an agent's
//
// The authority table refuses every one of these to an agent (humanOnly), the
// atlas's writer refuses a claim no person made, and no seat's registry holds
// them: an agent records only what a call of its own turn showed it, through a
// tool of its own.

// The tools' names, each its authority row's action.
const (
	AtlasAddEntryTool = string(authz.ActionAtlasAddEntry)
	AtlasCorrectTool  = string(authz.ActionAtlasCorrect)
	AtlasConfirmTool  = string(authz.ActionAtlasConfirm)
	AtlasRelateTool   = string(authz.ActionAtlasRelate)
	AtlasUnrelateTool = string(authz.ActionAtlasUnrelate)
	AtlasSetOwnerTool = string(authz.ActionAtlasSetOwner)
	AtlasRetireTool   = string(authz.ActionAtlasRetire)
	AtlasRestoreTool  = string(authz.ActionAtlasRestore)
	AtlasMergeTool    = string(authz.ActionAtlasMerge)
	AtlasSplitTool    = string(authz.ActionAtlasSplit)
	AtlasPurgeTool    = string(authz.ActionAtlasPurge)
)

// AtlasWriteTools are the write tools' names, in the order a person meets them.
var AtlasWriteTools = []string{
	AtlasAddEntryTool, AtlasCorrectTool, AtlasConfirmTool, AtlasRelateTool, AtlasUnrelateTool,
	AtlasSetOwnerTool, AtlasRetireTool, AtlasRestoreTool, AtlasMergeTool, AtlasSplitTool, AtlasPurgeTool,
}

// AtlasLog is the atlas's log as its write tools reach it: an entry's version,
// and the writes. Through the estate router on a running node, so a person's
// write is the same call from a node holding no data.
type AtlasLog interface {
	Entry(ctx context.Context, id string, fresh statelog.Freshness) (atlasdomain.EntryRow, bool, error)
	Record(ctx context.Context, c atlasdomain.Call, claim atlas.Claim, ifMatch uint64) (statelog.Result, error)
	PurgeEntry(ctx context.Context, c atlasdomain.Call, ref atlas.Ref) (statelog.Result, error)
	PurgeDigest(ctx context.Context, c atlasdomain.Call, digest string) (statelog.Result, error)
}

// AtlasWriteDeps are what the atlas's write tools change it through. The zero
// value serves none of them.
type AtlasWriteDeps struct {
	// Log is the atlas's log. Nil omits every write tool.
	Log AtlasLog

	// Actor is who writes, off the caller's own credential ([PrincipalActor]),
	// carrying the operation the call's transport names. Required with Log.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// Org is the company's chart, resolved per call: an owner is a seat or a
	// unit of it. Required with Log.
	Org func() *org.Organization

	// Now is the clock a claim's instant is read off; nil is the wall clock.
	Now func() time.Time

	ops operations
}

// serves reports whether the write tools are served.
func (d AtlasWriteDeps) serves() bool { return d.Log != nil && d.Actor != nil && d.Org != nil }

func (d AtlasWriteDeps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now().UTC()
}

// atlasWriteCandidates are the write tools, each over the atlas read its
// decisions are made on.
func atlasWriteCandidates(deps OperatorDeps) []candidate {
	on := deps.AtlasWrite.serves() && deps.Atlas.Settings != nil
	out := make([]candidate, 0, len(atlasVerbs))
	for _, v := range atlasVerbs {
		out = append(out, candidate{&atlasWrite{verb: v, deps: deps.AtlasWrite,
			read: atlasReader{deps: deps.Atlas, code: deps.Code, authorize: deps.Authorize}}, on})
	}
	return out
}

// atlasAnnotations are the write tools' hints. EVERY ONE WRITES A SHARED
// SURFACE, since every holder of the app reads the atlas whole. A retire, a
// relation removed, an owner or a value replaced and a purge each undo
// somebody else's word, so they are destructive, though only the purge cannot
// be taken back. A claim made again states the same thing and changes nothing,
// so every write but the add and the purge, which are refused a second time,
// is idempotent.
func atlasAnnotations(name string) (tools.Annotations, bool) {
	if !slices.Contains(AtlasWriteTools, name) {
		return tools.Annotations{}, false
	}
	a := tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, Idempotent: mcp.Yes, OpenWorld: mcp.Yes}
	switch name {
	case AtlasCorrectTool, AtlasUnrelateTool, AtlasSetOwnerTool, AtlasRetireTool:
		a.Destructive = mcp.Yes
	case AtlasAddEntryTool:
		a.Idempotent = mcp.No
	case AtlasPurgeTool:
		a.Destructive, a.Idempotent = mcp.Yes, mcp.No
	}
	return a, true
}

// ---- the plan ----------------------------------------------------------- //

// atlasVerb is one write tool: its name, what it tells a caller, the
// arguments it takes beyond the operation, and how it plans.
type atlasVerb struct {
	name        string
	description string
	params      func() map[string]any
	required    []string
	plan        func(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result)
}

// atlasPlan is what one write records: its steps in order, each named, and
// what its answer says it did. No step is a write that changes nothing.
type atlasPlan struct {
	// entry is the entry the write is about, the object a refusal names.
	entry atlas.Ref
	steps []atlasStep
	said  map[string]any
}

// atlasStep is one record: a claim, an entry's purge or a value's.
//
// A STEP IS NAMED BY ITS VERB AND A SHORT DIGEST of what tells it apart from
// the call's other steps of that verb (key), never by an entry's id or a
// handle written out: a step's operation is the call's and the name, and an
// id the caller brought back leaves a name little more than fifty bytes
// ([statelog.MaxCallerOpIDBytes]).
type atlasStep struct {
	name   string
	key    string
	claim  *atlas.Claim
	purge  *atlas.Ref
	digest string
}

// stepObject is the part of a step's operation that tells it from the call's
// other steps of its verb: twelve hex digits of a digest of its subject and its
// key, the same for the same step on every attempt of the call.
func stepObject(subject atlas.Ref, key string) string {
	sum := sha256.Sum256([]byte(subject.ID().String() + "\x00" + key))
	return hex.EncodeToString(sum[:6])
}

// atlasNow is the atlas a write is decided on: as its caller reads it, at the
// instant the write is made, with what people recorded current through
// recordedAt.
type atlasNow struct {
	tool       string
	view       atlas.View
	recorded   []atlas.Claim
	recordedAt statelog.Position
	gitlet     bool
	chart      *org.Organization
	person     atlas.Source
	at         time.Time
}

// atlasWrite is one write tool.
type atlasWrite struct {
	verb atlasVerb
	deps AtlasWriteDeps
	read atlasReader
}

var _ tools.SeatCallable = (*atlasWrite)(nil)

func (t *atlasWrite) Name() string        { return t.verb.name }
func (t *atlasWrite) Description() string { return t.verb.description }

func (t *atlasWrite) Parameters() map[string]any {
	props := t.verb.params()
	props["if_match"] = map[string]any{
		"type": "integer",
		"description": "The entry's version you decided from: the `version` the atlas's history of it, " +
			"or your last write to it, answered. A write over a change you did not see is refused. Omit it " +
			"to decide on the atlas as it is when the write is made.",
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(t.verb.required) > 0 {
		schema["required"] = slices.Clone(t.verb.required)
	}
	return t.deps.ops.param(schema)
}

func (t *atlasWrite) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *atlasWrite) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.Actor(ctx, turn)
	if err != nil {
		return faulted(ctx, t.Name(), err, fmt.Sprintf("%s could not tell who is writing, so it wrote "+
			"nothing: %s.", t.Name(), faultSaid)), nil
	}
	if actor.Kind == worklet.AuthorAgent {
		return refused(tools.RefusalForbidden, fmt.Sprintf("%s wrote nothing: an agent never changes "+
			"what a person decides in the atlas.", t.Name())), nil
	}
	actor, bad := t.deps.ops.bindActor(actor, t.Name(), args)
	if bad != "" {
		return failed(bad), nil
	}
	ifMatch, given := uint64(0), args["if_match"] != nil
	if given {
		n := argInt(args, "if_match", -1)
		if n <= 0 {
			return failed(fmt.Sprintf("%s: `if_match` is an entry's version, a positive whole number.",
				t.Name())), nil
		}
		ifMatch = uint64(n)
	}
	n, refusal := t.now(ctx, turn, actor)
	if refusal != nil {
		return *refusal, nil
	}
	plan, refusal := t.verb.plan(n, args)
	if refusal != nil {
		return *refusal, nil
	}
	versions, refusal := t.versions(ctx, n, plan)
	if refusal != nil {
		return *refusal, nil
	}
	if given && versions[plan.entry.ID()] != ifMatch {
		return refusedBy(tools.RefusalStaleVersion, atlasdomain.ErrStaleVersion, fmt.Sprintf("%s wrote "+
			"nothing: %s is at version %d and the change was decided from %d. Read it again and decide.",
			t.Name(), plan.entry, versions[plan.entry.ID()], ifMatch)), nil
	}
	return t.write(ctx, actor, plan, versions, args), nil
}

// versions reads the version of every entry a plan records a claim on, each
// one the atlas read must have seen: an entry that moved past the position the
// read was current through changed after the plan was decided, and refuses
// it. Every claim is then recorded on the version read, so a change made after
// this read refuses the write at the log.
func (t *atlasWrite) versions(ctx context.Context, n *atlasNow, plan atlasPlan) (map[uuid.UUID]uint64,
	*tools.Result) {

	out := map[uuid.UUID]uint64{}
	subjects := []atlas.Ref{}
	if plan.entry != (atlas.Ref{}) {
		subjects = append(subjects, plan.entry)
	}
	for _, step := range plan.steps {
		if step.claim != nil {
			subjects = append(subjects, step.claim.Subject)
		}
	}
	for _, subject := range subjects {
		id := subject.ID()
		if _, read := out[id]; read {
			continue
		}
		row, found, err := t.deps.Log.Entry(ctx, id.String(), atlasdomain.DefaultRead())
		if err != nil {
			r := atlasReadFailure(ctx, t.Name(), err)
			return nil, &r
		}
		if !found {
			// DECIDED WHERE IT HELD NO RECORD, and recorded only while it
			// still holds none.
			out[id] = atlasdomain.IfNoRecord
			continue
		}
		if int64(row.Version) > n.recordedAt.Packed() {
			r := refusedBy(tools.RefusalStaleVersion, atlasdomain.ErrStaleVersion, fmt.Sprintf("%s wrote "+
				"nothing: %s changed while the change was being decided. Read it again and decide.",
				t.Name(), subject))
			return nil, &r
		}
		out[id] = row.Version
	}
	return out, nil
}

// now reads what a write is decided on: the atlas as the caller reads it, then
// the chart.
func (t *atlasWrite) now(ctx context.Context, turn *turnctx.Turn, actor Actor) (*atlasNow, *tools.Result) {

	n := &atlasNow{tool: t.Name(), at: t.deps.now(), person: atlas.PersonSource(actor.Handle)}
	answer, read, why := t.read.atlas(ctx, turn, t.Name(), atlasdomain.DefaultRead(), n.at)
	if why != "" {
		r := refusedBy(tools.RefusalUnavailable, waitless(), fmt.Sprintf("%s wrote nothing: %s", t.Name(), why))
		return nil, &r
	}
	// A CHANGE IS DECIDED ON ALL OF WHAT THE CALLER READS: a source this node
	// serves that answered in part may hold the very fact the write would
	// claim beside. One this node does not serve, and Gitlet where the caller
	// may not read it, hold nothing the write may touch.
	for _, c := range answer.Coverage {
		if c.Complete || c.Reason == gather.ErrNoSource.Error() || c.Source == gather.SourceGitlet && !read {
			continue
		}
		r := refusedBy(tools.RefusalUnavailable, waits(), fmt.Sprintf("%s wrote nothing: the atlas's %s "+
			"could not be read in full just now, and a change is decided on all of it. Try again shortly.",
			t.Name(), strings.ReplaceAll(string(c.Source), "_", " ")))
		return nil, &r
	}
	n.view, n.recorded, n.recordedAt, n.gitlet = answer.View, answer.Recorded, answer.RecordedAt, read
	if n.chart = t.deps.Org(); n.chart == nil {
		r := refusedBy(tools.RefusalUnavailable, waitless(), fmt.Sprintf("%s wrote nothing: this node runs no company "+
			"yet. Try again once its settings are applied.", t.Name()))
		return nil, &r
	}
	return n, nil
}

// write records a plan's steps in order, each under its own operation derived
// from the call's, and each on the version the one before it left: a retry of
// the call is every step again, and the log answers each that landed.
func (t *atlasWrite) write(ctx context.Context, actor Actor, plan atlasPlan, versions map[uuid.UUID]uint64,
	args map[string]any) tools.Result {

	answer := plan.said
	if plan.entry != (atlas.Ref{}) {
		answer["entry"] = plan.entry.ID().String()
	}
	if len(plan.steps) == 0 {
		answer["outcome"], answer["unchanged"] = string(statelog.OutcomeApplied), true
		if v, ok := versionOf(versions, plan.entry); ok {
			answer["version"] = v
		}
		r, _ := jsonResult(withOperation(answer, actor))
		return r
	}
	by := iam.Actor{Name: actor.Handle, Kind: iam.ActorKind(actor.Kind), OperatorID: actor.OperatorID}
	var last statelog.Result
	for i, step := range plan.steps {
		subject := plan.entry
		if step.claim != nil {
			subject = step.claim.Subject
		}
		opID := opIDFor(actor, t.Name(), "atlas-"+step.name, stepObject(subject, step.key), args)
		call := atlasdomain.Call{By: by, OpID: opID}
		var (
			res statelog.Result
			err error
		)
		switch {
		case step.claim != nil:
			id := step.claim.Subject.ID()
			res, err = t.deps.Log.Record(ctx, call, *step.claim, versions[id])
			// THE ENTRY'S NEW VERSION IS THIS RECORD'S POSITION, which the
			// next claim on it is recorded on.
			if err == nil && !res.Position.IsZero() {
				versions[id] = uint64(res.Position.Packed())
			}
		case step.purge != nil:
			res, err = t.deps.Log.PurgeEntry(ctx, call, *step.purge)
		default:
			res, err = t.deps.Log.PurgeDigest(ctx, call, step.digest)
		}
		var unserved *estate.ErrUnserved
		switch {
		case errors.Is(err, estate.ErrOutcomeUnknown) || errors.As(err, &unserved) && unserved.Silent(),
			err == nil && res.Outcome == statelog.OutcomeUnknown:
			return unknownWrite(actor, t.Name(), "the atlas was changed", opID, res.Unvouched,
				unknownNext(res.Unvouched, sameCall(actor, t.Name()), "Read the entry in the atlas again",
					"the same claims are recorded again, which changes nothing"))
		case err != nil:
			failure := atlasWriteFailure(ctx, t.Name(), actor, plan.entry, err)
			if i > 0 {
				// WHAT LANDED BEFORE IT STAYS, and the same call finishes the
				// rest: each step is its own operation, which the log answers
				// as landed when it is made again.
				failure.Output += fmt.Sprintf(" The first %d of its %d records landed; %s finishes "+
					"the rest.", i, len(plan.steps), orAgain(sameCall(actor, t.Name())))
			}
			return failure
		}
		last = res
	}
	answer["outcome"], answer["position"] = string(last.Outcome), positionOf(last.Position)
	if v, ok := versionOf(versions, plan.entry); ok {
		answer["version"] = v
	}
	r, _ := jsonResult(withOperation(answer, actor))
	return r
}

// versionOf is an entry's version as an answer states it, false where it
// holds no record.
func versionOf(versions map[uuid.UUID]uint64, entry atlas.Ref) (uint64, bool) {
	v, ok := versions[entry.ID()]
	return v, ok && v != atlasdomain.NoIfMatch && v != atlasdomain.IfNoRecord
}

// orAgain is the repeat that is the same call, or plain words where the caller
// has none.
func orAgain(again string) string {
	if again == "" {
		return "making the same call again"
	}
	return again
}

// atlasWriteFailure is a refusal or a failure of the atlas's log as the write
// tools answer it, each in its class: what the log refused in the domain's
// own words, a node or a fleet that could not take it as unavailable, and
// anything else this node's fault.
func atlasWriteFailure(ctx context.Context, tool string, actor Actor, entry atlas.Ref, err error) tools.Result {
	if class, ok := WriterClass(err); ok {
		return refusedBy(class, err, domainRefusal(tool, err))
	}
	again := sameCall(actor, tool)
	if again == "" {
		again = "make the same call again"
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not write to the atlas right "+
			"now (%s); nothing is known to have been written about %s. %s, which is the same write.",
			tool, why, entry, capitalize(again)))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not write to the atlas: %s.", tool, faultSaid))
}

// atlasReadFailure is a read a write is decided on that failed.
func atlasReadFailure(ctx context.Context, tool string, err error) tools.Result {
	if class, ok := WriterClass(err); ok {
		return refusedBy(class, err, domainRefusal(tool, err))
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s wrote nothing: the atlas could not "+
			"be read right now (%s). Try again shortly.", tool, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not read the atlas: %s.", tool, faultSaid))
}

// atlasClass is the class of a refusal the atlas's log decided. A purged
// entry or value is gone for good; a record of a newer build is no refusal.
func atlasClass(err error) (tools.Refusal, bool) {
	switch {
	case errors.Is(err, atlasdomain.ErrStaleVersion):
		return tools.RefusalStaleVersion, true
	case errors.Is(err, atlasdomain.ErrPurged):
		return tools.RefusalNotFound, true
	case errors.Is(err, atlasdomain.ErrExists):
		return tools.RefusalExists, true
	case errors.Is(err, atlasdomain.ErrConflict), errors.Is(err, atlasdomain.ErrNotDue):
		return tools.RefusalConflict, true
	case errors.Is(err, atlasdomain.ErrInvalid):
		return tools.RefusalInvalid, true
	}
	return "", false
}

// ---- reading the arguments ---------------------------------------------- //

// entryArg describes an argument naming an entry.
func entryArg(what string) map[string]any {
	return map[string]any{
		"type": "string",
		"description": what + ": its `id`, as the atlas answers it, or its kind and key as `kind:key` " +
			"(`host:ssh/web-1`, `service:checkout-api`).",
	}
}

// member is one entry an argument names, as the atlas reads it now: the
// member of a merged entry the argument named, and the entry it is part of.
type member struct {
	ref   atlas.Ref
	entry atlas.Entry
	found bool
}

// entry resolves the entry argument arg names. An entry the atlas holds is
// found by its id or by the id of any member merged into it; a key names an
// entry whether or not the atlas holds it, for the one write that adds it.
func (n *atlasNow) entry(a map[string]any, arg string) (member, *tools.Result) {
	raw := strings.TrimSpace(argString(a, arg))
	if raw == "" {
		r := failed(fmt.Sprintf("%s: `%s` names the entry: its id, or `kind:key`.", n.tool, arg))
		return member{}, &r
	}
	var ref atlas.Ref
	if id, err := uuid.Parse(raw); err == nil && id.String() == raw {
		e, ok := n.view.Find(id)
		if !ok {
			return member{}, n.notFound(arg)
		}
		ref = e.Ref
		if i := slices.IndexFunc(e.Members, func(m atlas.Ref) bool { return m.ID() == id }); i >= 0 {
			ref = e.Members[i]
		}
	} else {
		kind, key, ok := strings.Cut(raw, ":")
		if !ok || !atlas.Kind(kind).Valid() {
			r := failed(fmt.Sprintf("%s: `%s` is an entry's id, or `kind:key` with a kind of %s.", n.tool,
				arg, words.Or(atlas.Kinds)))
			return member{}, &r
		}
		parsed, err := atlas.RecordedRef(atlas.Kind(kind), key)
		if err != nil {
			r := refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s: `%s`: %s.", n.tool, arg,
				atlasdomain.Sentence(err)))
			return member{}, &r
		}
		ref = parsed
	}
	if ref.OnGitlet() && !n.gitlet {
		r := refused(tools.RefusalForbidden, fmt.Sprintf("%s wrote nothing: a Gitlet repository is changed "+
			"in the atlas by a person who uses Gitlet and can read it.", n.tool))
		return member{}, &r
	}
	e, found := n.view.Find(ref.ID())
	if !found && ref.OnGitlet() {
		// GITLET LISTS THE CALLER EVERY REPOSITORY THEY MAY READ, so one it
		// did not list is one they may not change.
		return member{}, n.notFound(arg)
	}
	return member{ref: ref, entry: e, found: found}, nil
}

// held is an entry the atlas holds now, or the refusal that it holds none.
func (n *atlasNow) held(a map[string]any, arg string) (member, *tools.Result) {
	m, refusal := n.entry(a, arg)
	if refusal != nil {
		return member{}, refusal
	}
	if !m.found {
		return member{}, n.notFound(arg)
	}
	return m, nil
}

func (n *atlasNow) notFound(arg string) *tools.Result {
	r := refusedBy(tools.RefusalNotFound, errAtlasNoEntry, fmt.Sprintf("%s: the atlas holds no entry that "+
		"`%s` names and you can read. Find it with %s first.", n.tool, arg, AtlasSearchTool))
	return &r
}

// errAtlasNoEntry is an entry a write names that the atlas does not hold.
var errAtlasNoEntry = errors.New("builtin: no such atlas entry")

// declarer is where a computed claim's fact is declared, as a person is told:
// the settings, Gitlet, or the engine's own records of what happened.
func declarer(s atlas.Source) string {
	switch {
	case s.Kind == atlas.SourceSettings:
		return "the company's settings"
	case s.Name == string(atlas.RecordGitlet):
		return "Gitlet"
	}
	return "the engine's own records"
}

// declared is the computed source among a facet's live claims, where one is.
func declared(winner atlas.Source, others []atlas.Claimed) (atlas.Source, bool) {
	if winner.Kind.Computed() {
		return winner, true
	}
	for _, o := range others {
		if o.Source.Kind.Computed() {
			return o.Source, true
		}
	}
	return atlas.Source{}, false
}

// declaredRefusal is a write on what src declares.
func (n *atlasNow) declaredRefusal(what string, src atlas.Source) *tools.Result {
	r := refused(tools.RefusalConflict, fmt.Sprintf("%s wrote nothing: %s declare %s, and what they "+
		"declare is changed there, never in the atlas.", n.tool, declarer(src), what))
	return &r
}

// fact is the fact a write names, held to the kind of the entry it is about.
func (n *atlasNow) fact(a map[string]any, ref atlas.Ref) (atlas.Fact, *tools.Result) {
	f := atlas.Fact(strings.TrimSpace(argString(a, "fact")))
	if !f.Valid() || !f.AppliesTo(ref.Kind) {
		var carried []atlas.Fact
		for _, c := range atlas.Facts {
			if c.AppliesTo(ref.Kind) {
				carried = append(carried, c)
			}
		}
		r := failed(fmt.Sprintf("%s: `fact` is one a %s carries: %s.", n.tool, ref.Kind, words.Or(carried)))
		return "", &r
	}
	return f, nil
}

// claim builds one claim through the vocabulary's constructors, a refusal of
// which is the caller's argument to change.
func (n *atlasNow) claim(c atlas.Claim, err error) (*atlas.Claim, *tools.Result) {
	if err == nil {
		err = c.Recordable()
	}
	if err != nil {
		r := refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s wrote nothing: %s.", n.tool,
			atlasdomain.Sentence(err)))
		return nil, &r
	}
	return &c, nil
}

// factOn is the fact f as the entry shows it: its one value, for a fact that
// holds many the value named, and for a fact of each host the one of on, the
// member the caller named.
func factOn(e atlas.Entry, on atlas.Ref, f atlas.Fact, value string) (atlas.FactView, bool) {
	spec := f.Spec()
	for _, fv := range e.Facts {
		if fv.Fact == f && (!spec.Many || fv.Value.Text == value) && (!spec.PerHost || fv.Of == on) {
			return fv, true
		}
	}
	return atlas.FactView{}, false
}

// edgeOn is the relation r from e to the entry to, as e shows it.
func edgeOn(e atlas.Entry, r atlas.Relation, to uuid.UUID) (atlas.Edge, bool) {
	for _, ed := range e.Relations {
		if ed.Relation == r && ed.External == "" && ed.To == to {
			return ed, true
		}
	}
	return atlas.Edge{}, false
}

// shown is what an answer says of the value a write replaces: who held it and
// the word it read as, NEVER ITS TEXT, which another source wrote and which
// reaches whoever reads the answer, a person's assistant among them.
func shown(fv atlas.FactView) map[string]any {
	return map[string]any{"source": string(fv.Source.Kind), "word": string(fv.Word)}
}

// said opens an answer's account of a write about e.
func said(change string, e atlas.Ref) map[string]any {
	return map[string]any{"change": change, "kind": string(e.Kind), "key": e.Key}
}

// ---- the verbs ---------------------------------------------------------- //

// atlasVerbs are the write tools, in [AtlasWriteTools]' order.
var atlasVerbs = []atlasVerb{
	{
		name: AtlasAddEntryTool,
		description: "Add an entry to the company's atlas that no source reports: a service, a " +
			"database, a queue, a bucket, a host, a domain or anything else the company runs on, by its " +
			"kind and its key, with a name if its key is not one. A vendor the atlas does not know is an " +
			"account with vendor `other`. Entries the company's settings or Gitlet declare are added there. " +
			"What you know of it is recorded with it: its purpose, the account it belongs to, its owner and " +
			"the page that documents it.",
		params: func() map[string]any {
			return map[string]any{
				"kind": map[string]any{"type": "string", "enum": atlasKinds(), "description": "The entry's kind."},
				"key": map[string]any{"type": "string", "description": "Its key, in its kind's grammar: a " +
					"service's slug (`checkout-api`), a host name, an address with its scheme, " +
					"`<vendor>/<account id>` for an account."},
				"name": map[string]any{"type": "string", "description": fmt.Sprintf("A name to show, plain "+
					"text of at most %d characters. Omit it to show the key.", atlas.MaxLabelRunes)},
				"purpose": map[string]any{"type": "string", "description": fmt.Sprintf("What it is for, one "+
					"line of plain text of at most %d characters.", atlas.MaxLabelRunes)},
				"belongs_to":  entryArg("The vendor account it belongs to"),
				"seat":        map[string]any{"type": "string", "description": "The owning seat's handle."},
				"unit":        map[string]any{"type": "string", "description": "The owning unit's key."},
				"doclet_page": pageArg("The Doclet page that documents it"),
			}
		},
		required: []string{"kind", "key"},
		plan:     planAdd,
	},
	{
		name: AtlasCorrectTool,
		description: "Set one fact of an atlas entry to the value you know, which then outranks what any " +
			"integration or agent reports until they report a change, when both are shown for review. For " +
			"a fact that holds many values (addresses, regions) it adds one. With `wrong`, it says the " +
			"value the atlas shows is wrong instead. Facts the company's settings declare are changed there.",
		params: func() map[string]any {
			return map[string]any{
				"atlas_entry": entryArg("The entry"),
				"fact":        map[string]any{"type": "string", "enum": atlasFacts(), "description": "The fact."},
				"value": map[string]any{"type": "string", "description": "The value, in the fact's own " +
					"grammar. With `wrong`, the value that is wrong; omit it for a fact of one value."},
				"wrong": map[string]any{"type": "boolean", "description": "Say the value is wrong rather " +
					"than set it."},
			}
		},
		required: []string{"atlas_entry", "fact"},
		plan:     planCorrect,
	},
	{
		name: AtlasConfirmTool,
		description: "Confirm what the atlas shows about an entry as your own: the entry itself, one of " +
			"its facts (`fact`, and `value` for a fact that holds many), or one relation (`relation` and " +
			"`to`). What an integration or an agent reported is then confirmed by you. `value` may name " +
			"another value a source reports for the fact, which you confirm instead.",
		params: func() map[string]any {
			return map[string]any{
				"atlas_entry": entryArg("The entry"),
				"fact":        map[string]any{"type": "string", "enum": atlasFacts(), "description": "A fact to confirm."},
				"value":       map[string]any{"type": "string", "description": "The value you confirm, as the atlas shows it."},
				"relation":    map[string]any{"type": "string", "enum": atlasEntryRelations(), "description": "A relation to confirm."},
				"to":          entryArg("With `relation`, the entry it points at"),
			}
		},
		required: []string{"atlas_entry"},
		plan:     planConfirm,
	},
	{
		name: AtlasRelateTool,
		description: "Draw one relation between two atlas entries, from the one that relies to the one " +
			"relied on: a service `runs_on` a host, `depends_on` a database, a host name `resolves_to` a " +
			"host. With `documented_in`, it names the Doclet page (`doclet_page`) that documents the entry instead. " +
			"An owner is set with " + AtlasSetOwnerTool + ".",
		params: func() map[string]any {
			return map[string]any{
				"atlas_entry": entryArg("The entry that relies"),
				"relation":    map[string]any{"type": "string", "enum": atlasDrawnRelations(), "description": "The relation."},
				"to":          entryArg("The entry relied on, for every relation but `documented_in`"),
				"doclet_page": pageArg("With `documented_in`, the page"),
			}
		},
		required: []string{"atlas_entry", "relation"},
		plan:     planRelate(false),
	},
	{
		name: AtlasUnrelateTool,
		description: "Remove one relation the atlas shows between two entries, or from an entry to the " +
			"Doclet page that documents it (`documented_in` and `doclet_page`): it is recorded that it does not hold.",
		params: func() map[string]any {
			return map[string]any{
				"atlas_entry": entryArg("The entry the relation runs from"),
				"relation":    map[string]any{"type": "string", "enum": atlasDrawnRelations(), "description": "The relation."},
				"to":          entryArg("The entry it points at, for every relation but `documented_in`"),
				"doclet_page": pageArg("With `documented_in`, the page"),
			}
		},
		required: []string{"atlas_entry", "relation"},
		plan:     planRelate(true),
	},
	{
		name: AtlasSetOwnerTool,
		description: "Set who owns an atlas entry: one seat of the chart, by its handle, or one unit, by " +
			"its key, in place of any owner a person named before. `clear` says it has no owner.",
		params: func() map[string]any {
			return map[string]any{
				"atlas_entry": entryArg("The entry"),
				"seat":        map[string]any{"type": "string", "description": "The owning seat's handle."},
				"unit":        map[string]any{"type": "string", "description": "The owning unit's key."},
				"clear":       map[string]any{"type": "boolean", "description": "Remove every owner a person named."},
			}
		},
		required: []string{"atlas_entry"},
		plan:     planOwner,
	},
	{
		name: AtlasRetireTool,
		description: "Retire an atlas entry the company no longer runs: it leaves the map at once and " +
			"stays in its history and under the retired entries, and " + AtlasRestoreTool + " brings it " +
			"back. An entry the settings declare is removed there.",
		params:   func() map[string]any { return map[string]any{"atlas_entry": entryArg("The entry")} },
		required: []string{"atlas_entry"},
		plan:     planRetire(true),
	},
	{
		name:        AtlasRestoreTool,
		description: "Bring back an atlas entry somebody retired.",
		params:      func() map[string]any { return map[string]any{"atlas_entry": entryArg("The entry")} },
		required:    []string{"atlas_entry"},
		plan:        planRetire(false),
	},
	{
		name: AtlasMergeTool,
		description: "Say two atlas entries of one kind are the same thing, which no exact key joins: they " +
			"are shown as one. Where somebody said they are not the same, that is withdrawn first.",
		params: func() map[string]any {
			return map[string]any{"atlas_entry": entryArg("One entry"), "other": entryArg("The other")}
		},
		required: []string{"atlas_entry", "other"},
		plan:     planMerge,
	},
	{
		name: AtlasSplitTool,
		description: "Say two atlas entries are not the same thing, though a shared key or somebody " +
			"merged them: they are shown apart, and a person's \"not the same\" always wins.",
		params: func() map[string]any {
			return map[string]any{"atlas_entry": entryArg("One entry"), "other": entryArg("The other")}
		},
		required: []string{"atlas_entry", "other"},
		plan:     planSplit,
	},
	{
		name: AtlasPurgeTool,
		description: "Erase from the atlas, everywhere and with its history, an entry (`atlas_entry`) or a value " +
			"(`value`, as the atlas holds it) that must not be kept, and keep it from coming back. Nothing " +
			"undoes it. An administrator's, after a fresh sign-in.",
		params: func() map[string]any {
			return map[string]any{
				"atlas_entry": entryArg("The entry to erase"),
				"value": map[string]any{"type": "string", "description": "A value to erase wherever it " +
					"appears, exactly as the atlas holds it. Never repeated in any answer."},
			}
		},
		plan: planPurge,
	},
}

// atlasFacts is every fact, as a schema names them.
func atlasFacts() []string {
	out := make([]string, len(atlas.Facts))
	for i, f := range atlas.Facts {
		out[i] = string(f)
	}
	return out
}

// atlasDrawnRelations is every relation a person draws or removes by name:
// those between two entries, and the page that documents one. An owner is one
// seat or one unit, set by its own tool.
func atlasDrawnRelations() []string {
	return append(atlasEntryRelations(), string(atlas.RelationDocumentedIn))
}

// pageArg describes an argument naming a Doclet page.
func pageArg(what string) map[string]any {
	return map[string]any{"type": "string", "description": what + ": its id, as Doclet answers it."}
}

// atlasEntryRelations is every relation between two entries, as a schema
// names them.
func atlasEntryRelations() []string {
	var out []string
	for _, r := range atlas.Relations {
		if r != atlas.RelationOwnedBy && r != atlas.RelationDocumentedIn {
			out = append(out, string(r))
		}
	}
	return out
}

func planAdd(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
	kind := atlas.Kind(strings.TrimSpace(argString(a, "kind")))
	if !kind.Valid() {
		r := failed(fmt.Sprintf("%s: `kind` is %s.", n.tool, words.Or(atlas.Kinds)))
		return atlasPlan{}, &r
	}
	ref, err := atlas.RecordedRef(kind, argString(a, "key"))
	if err != nil {
		r := refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s: `key`: %s.", n.tool, atlasdomain.Sentence(err)))
		return atlasPlan{}, &r
	}
	if src, ok := ref.EngineNamed(); ok {
		r := refused(tools.RefusalConflict, fmt.Sprintf("%s wrote nothing: a %s keyed %s is one %s "+
			"declare, and enters the atlas from there.", n.tool, kind, ref.Key, declarer(src)))
		return atlasPlan{}, &r
	}
	plan := atlasPlan{entry: ref, said: said("added", ref)}
	e, found := n.view.Find(ref.ID())
	if found && e.State == atlas.StateRetired {
		r := refused(tools.RefusalConflict, fmt.Sprintf("%s wrote nothing: %s is in the atlas, retired. "+
			"Bring it back with %s.", n.tool, ref, AtlasRestoreTool))
		return atlasPlan{}, &r
	}
	// AN ADD THAT STOPPED PART OF THE WAY IS FINISHED BY THE SAME CALL: where
	// the caller's own claim already stands on the entry, what the call still
	// says of it and the atlas does not is recorded, and an add that would
	// record nothing is refused as the second add it is.
	mine := found && slices.Contains(e.Sources, n.person)
	if found && !mine {
		r := refusedBy(tools.RefusalExists, atlasdomain.ErrExists, fmt.Sprintf("%s wrote nothing: %s is in "+
			"the atlas already, as entry %s.", n.tool, ref, e.ID))
		return atlasPlan{}, &r
	}
	step := func(name, key string) func(atlas.Claim, error) *tools.Result {
		return func(c atlas.Claim, err error) *tools.Result {
			claim, bad := n.claim(c, err)
			if bad != nil {
				return bad
			}
			plan.steps = append(plan.steps, atlasStep{name: name, key: key, claim: claim})
			return nil
		}
	}
	if !found {
		if bad := step("add", "")(atlas.Exists(ref, n.person, n.at)); bad != nil {
			return atlasPlan{}, bad
		}
	}
	says := func(stepName string, f atlas.Fact, raw string) *tools.Result {
		c, err := atlas.Says(ref, f, raw, n.person, n.at)
		if err == nil {
			plan.said[string(f)] = c.Value.Text
			if now, held := factOn(e, ref, f, c.Value.Text); held && now.Value.Equal(c.Value) {
				return nil
			}
		}
		return step(stepName, "")(c, err)
	}
	if name := strings.TrimSpace(argString(a, "name")); name != "" && name != ref.Key {
		if bad := says("name", atlas.FactName, name); bad != nil {
			return atlasPlan{}, bad
		}
	}
	if purpose := strings.TrimSpace(argString(a, "purpose")); purpose != "" {
		if bad := says("purpose", atlas.FactPurpose, purpose); bad != nil {
			return atlasPlan{}, bad
		}
	}
	if strings.TrimSpace(argString(a, "belongs_to")) != "" {
		account, bad := n.held(a, "belongs_to")
		if bad != nil {
			return atlasPlan{}, bad
		}
		if !atlas.RelationBelongsTo.Joins(kind, account.entry.Ref.Kind) {
			r := failed(fmt.Sprintf("%s: `belongs_to` names a vendor account, which a %s may belong to; "+
				"a %s does not belong to a %s.", n.tool, atlas.KindService, kind, account.entry.Ref.Kind))
			return atlasPlan{}, &r
		}
		plan.said["belongs_to"] = account.entry.ID.String()
		if _, drawn := edgeOn(e, atlas.RelationBelongsTo, account.entry.ID); !drawn {
			if bad := step("belong", "")(atlas.Relates(ref, atlas.RelationBelongsTo, account.entry.Ref, n.person,
				n.at)); bad != nil {
				return atlasPlan{}, bad
			}
		}
	}
	seat, unit := strings.TrimSpace(argString(a, "seat")), strings.TrimSpace(argString(a, "unit"))
	if seat != "" && unit != "" {
		r := failed(fmt.Sprintf("%s: an owner is one seat (`seat`) or one unit (`unit`), never both.", n.tool))
		return atlasPlan{}, &r
	}
	names := func(stepName string, r atlas.Relation, external atlas.External, name string) *tools.Result {
		if slices.ContainsFunc(e.Relations, func(ed atlas.Edge) bool {
			return ed.Relation == r && ed.External == external && ed.Name == name
		}) {
			return nil
		}
		return step(stepName, name)(atlas.Names(ref, r, external, name, n.person, n.at))
	}
	owner, refusal := n.owner(seat, unit)
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	if owner.External != "" {
		plan.said["owner"] = string(owner.External) + ":" + owner.Name
		if bad := names("own", atlas.RelationOwnedBy, owner.External, owner.Name); bad != nil {
			return atlasPlan{}, bad
		}
	}
	if strings.TrimSpace(argString(a, "doclet_page")) != "" {
		page, bad := n.page(a)
		if bad != nil {
			return atlasPlan{}, bad
		}
		plan.said["doclet_page"] = page
		if bad := names("document", atlas.RelationDocumentedIn, atlas.ExternalPage, page); bad != nil {
			return atlasPlan{}, bad
		}
	}
	if found && len(plan.steps) == 0 {
		r := refusedBy(tools.RefusalExists, atlasdomain.ErrExists, fmt.Sprintf("%s wrote nothing: %s is in "+
			"the atlas already, as entry %s.", n.tool, ref, e.ID))
		return atlasPlan{}, &r
	}
	return plan, nil
}

func planCorrect(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
	m, refusal := n.held(a, "atlas_entry")
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	f, refusal := n.fact(a, m.entry.Ref)
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	ref, raw, wrong := m.entry.Ref, argString(a, "value"), argBool(a, "wrong")
	// A FACT OF EACH HOST IS WRITTEN ON THE MEMBER THE CALLER NAMED, whichever
	// member the entry is known by: a mirror's visibility is not its source's.
	if f.Spec().PerHost {
		ref = m.ref
	}
	many := f.Spec().Many
	plan := atlasPlan{entry: ref, said: said("corrected", ref)}
	plan.said["fact"] = string(f)
	if !wrong {
		if strings.TrimSpace(raw) == "" {
			r := failed(fmt.Sprintf("%s: `value` is the value to record, or pass `wrong` to say the "+
				"value shown is wrong.", n.tool))
			return atlasPlan{}, &r
		}
		says, bad := n.claim(atlas.Says(ref, f, raw, n.person, n.at))
		if bad != nil {
			return atlasPlan{}, bad
		}
		plan.said["value"] = says.Value.Text
		now, held := factOn(m.entry, ref, f, says.Value.Text)
		if held {
			if src, ok := declared(now.Source, now.Others); ok {
				return atlasPlan{}, n.declaredRefusal(fmt.Sprintf("%s's %s", ref, f), src)
			}
			plan.said["before"] = shown(now)
			if now.Source.Kind == atlas.SourcePerson && now.Value.Equal(says.Value) && !now.Suggested &&
				now.Standing == atlas.StandingSettled {
				return plan, nil
			}
		}
		plan.steps = []atlasStep{{name: "correct", claim: says}}
		return plan, nil
	}
	// A VALUE IS CALLED WRONG ONLY AS THE ATLAS SHOWS IT: a person's denial of
	// a fact of one value hides the fact whichever value it names, so only the
	// value shown is denied, and a value a source reports beside it is chosen
	// with a correction or a confirmation instead.
	lookup := raw
	if !many {
		lookup = ""
	} else if strings.TrimSpace(raw) == "" {
		r := failed(fmt.Sprintf("%s: %s holds many values: `value` names the one that is wrong.", n.tool, f))
		return atlasPlan{}, &r
	}
	now, held := factOn(m.entry, ref, f, lookup)
	if !held {
		r := refused(tools.RefusalConflict, fmt.Sprintf("%s wrote nothing: the atlas shows no %s on %s "+
			"that value could be.", n.tool, f, ref))
		return atlasPlan{}, &r
	}
	if src, ok := declared(now.Source, now.Others); ok {
		return atlasPlan{}, n.declaredRefusal(fmt.Sprintf("%s's %s", ref, f), src)
	}
	if raw != "" {
		v, err := f.Value(raw)
		if err != nil || !v.Equal(now.Value) {
			r := refusedBy(tools.RefusalStaleVersion, atlasdomain.ErrStaleVersion, fmt.Sprintf("%s wrote "+
				"nothing: the atlas shows another %s on %s now. Read it again; a value a source reports "+
				"beside it is chosen with %s.", n.tool, f, ref, AtlasConfirmTool))
			return atlasPlan{}, &r
		}
	}
	says, refusal := n.claim(atlas.Says(ref, f, now.Value.Text, n.person, n.at))
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	denial, refusal := n.claim(atlas.Denial(*says, n.person, n.at))
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	plan.said["wrong"], plan.said["before"] = true, shown(now)
	plan.steps = []atlasStep{{name: "wrong", claim: denial}}
	return plan, nil
}

func planConfirm(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
	m, refusal := n.held(a, "atlas_entry")
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	ref := m.entry.Ref
	plan := atlasPlan{entry: ref, said: said("confirmed", ref)}
	switch {
	case argString(a, "fact") != "" && argString(a, "relation") != "":
		r := failed(fmt.Sprintf("%s: confirm a `fact` or a `relation`, one at a time.", n.tool))
		return atlasPlan{}, &r
	case argString(a, "fact") != "":
		f, bad := n.fact(a, ref)
		if bad != nil {
			return atlasPlan{}, bad
		}
		if f.Spec().PerHost {
			ref = m.ref
			plan = atlasPlan{entry: ref, said: said("confirmed", ref)}
		}
		value := argString(a, "value")
		lookup := value
		if !f.Spec().Many {
			lookup = ""
		}
		now, ok := factOn(m.entry, ref, f, lookup)
		if !ok {
			r := refused(tools.RefusalConflict, fmt.Sprintf("%s wrote nothing: the atlas shows no %s on %s "+
				"to confirm; set it with %s.", n.tool, f, ref, AtlasCorrectTool))
			return atlasPlan{}, &r
		}
		pick := now.Value
		if value != "" && value != now.Value.Text {
			i := slices.IndexFunc(now.Others, func(o atlas.Claimed) bool { return o.Value.Text == value && !o.Denied })
			if i < 0 {
				r := refusedBy(tools.RefusalStaleVersion, atlasdomain.ErrStaleVersion, fmt.Sprintf("%s wrote "+
					"nothing: no source says %s's %s is that value now. Read it again and decide.", n.tool, ref, f))
				return atlasPlan{}, &r
			}
			pick = now.Others[i].Value
		}
		plan.said["fact"] = string(f)
		if src, ok := declared(now.Source, now.Others); ok {
			if now.Source.Kind.Computed() && now.Value.Equal(pick) {
				return plan, nil
			}
			return atlasPlan{}, n.declaredRefusal(fmt.Sprintf("%s's %s", ref, f), src)
		}
		// A PERSON'S VALUE A NEWER REPORT CONTRADICTS IS SAID AGAIN: only a
		// claim newer than the report settles what the two disagree on.
		if now.Source.Kind == atlas.SourcePerson && now.Value.Equal(pick) && !now.Suggested &&
			now.Standing == atlas.StandingSettled {
			return plan, nil
		}
		says, bad := n.claim(atlas.Says(ref, f, pick.Text, n.person, n.at))
		if bad != nil {
			return atlasPlan{}, bad
		}
		plan.steps = []atlasStep{{name: "confirm", claim: says}}
		return plan, nil
	case argString(a, "relation") != "":
		r, to, bad := n.relation(a)
		if bad != nil {
			return atlasPlan{}, bad
		}
		ed, ok := edgeOn(m.entry, r, to.entry.ID)
		if !ok {
			res := refused(tools.RefusalConflict, fmt.Sprintf("%s wrote nothing: the atlas shows no %s from "+
				"%s to %s to confirm; draw it with %s.", n.tool, r, ref, to.entry.Ref, AtlasRelateTool))
			return atlasPlan{}, &res
		}
		plan.said["relation"], plan.said["to"] = string(r), to.entry.ID.String()
		if ed.Source.Kind.Computed() || ed.Source.Kind == atlas.SourcePerson && !ed.Suggested &&
			ed.Standing == atlas.StandingSettled {
			return plan, nil
		}
		claim, bad := n.claim(atlas.Relates(ref, r, to.entry.Ref, n.person, n.at))
		if bad != nil {
			return atlasPlan{}, bad
		}
		plan.steps = []atlasStep{{name: "confirm", claim: claim}}
		return plan, nil
	}
	if m.entry.State == atlas.StateRetired {
		r := refused(tools.RefusalConflict, fmt.Sprintf("%s wrote nothing: %s is retired; bring it back "+
			"with %s.", n.tool, ref, AtlasRestoreTool))
		return atlasPlan{}, &r
	}
	if m.entry.State == atlas.StateConfirmed {
		return plan, nil
	}
	exists, refusal := n.claim(atlas.Exists(ref, n.person, n.at))
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	plan.steps = []atlasStep{{name: "confirm", claim: exists}}
	return plan, nil
}

// relation reads a write's `relation` and `to`: a relation between two
// entries, and the entry it points at as the atlas holds it.
func (n *atlasNow) relation(a map[string]any) (atlas.Relation, member, *tools.Result) {
	r := atlas.Relation(strings.TrimSpace(argString(a, "relation")))
	if !slices.Contains(atlasEntryRelations(), string(r)) {
		res := failed(fmt.Sprintf("%s: `relation` is %s; an owner is set with %s.", n.tool,
			words.Or(atlasEntryRelations()), AtlasSetOwnerTool))
		return "", member{}, &res
	}
	if strings.TrimSpace(argString(a, "doclet_page")) != "" {
		res := failed(fmt.Sprintf("%s: `doclet_page` goes with %s alone; %s points at an entry, named by `to`.",
			n.tool, atlas.RelationDocumentedIn, r))
		return "", member{}, &res
	}
	to, refusal := n.held(a, "to")
	if refusal != nil {
		return "", member{}, refusal
	}
	return r, to, nil
}

func planRelate(remove bool) func(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
	return func(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
		m, bad := n.held(a, "atlas_entry")
		if bad != nil {
			return atlasPlan{}, bad
		}
		if strings.TrimSpace(argString(a, "relation")) == string(atlas.RelationDocumentedIn) {
			return planPage(n, a, m, remove)
		}
		r, to, bad := n.relation(a)
		if bad != nil {
			return atlasPlan{}, bad
		}
		ref := m.entry.Ref
		if to.entry.ID == m.entry.ID {
			res := failed(fmt.Sprintf("%s: an entry does not relate to itself.", n.tool))
			return atlasPlan{}, &res
		}
		if !r.Joins(ref.Kind, to.entry.Ref.Kind) {
			res := failed(fmt.Sprintf("%s: %s does not run from a %s to a %s.", n.tool, r, ref.Kind,
				to.entry.Ref.Kind))
			return atlasPlan{}, &res
		}
		change := "related"
		if remove {
			change = "unrelated"
		}
		plan := atlasPlan{entry: ref, said: said(change, ref)}
		plan.said["relation"], plan.said["to"] = string(r), to.entry.ID.String()
		ed, shownNow := edgeOn(m.entry, r, to.entry.ID)
		if shownNow {
			if src, ok := declared(ed.Source, ed.Others); ok {
				if !remove {
					return plan, nil
				}
				return atlasPlan{}, n.declaredRefusal(fmt.Sprintf("that %s %s %s", ref, r, to.entry.Ref), src)
			}
		}
		claim, bad := n.claim(atlas.Relates(ref, r, to.entry.Ref, n.person, n.at))
		if bad != nil {
			return atlasPlan{}, bad
		}
		switch {
		case !remove && shownNow && ed.Source.Kind == atlas.SourcePerson && ed.Standing == atlas.StandingSettled:
			return plan, nil
		case remove && !shownNow:
			return plan, nil
		case remove:
			denial, bad := n.claim(atlas.Denial(*claim, n.person, n.at))
			if bad != nil {
				return atlasPlan{}, bad
			}
			claim = denial
		}
		plan.steps = []atlasStep{{name: change, claim: claim}}
		return plan, nil
	}
}

// page reads a write's `doclet_page`: a Doclet page by its id, in its canonical
// form. THE PAGE IS NOT READ HERE: whether it exists and who may read it is
// Doclet's to say on every read of it, and the atlas keeps only its id, so a
// link is never a way to learn of a page.
func (n *atlasNow) page(a map[string]any) (string, *tools.Result) {
	raw := strings.TrimSpace(argString(a, "doclet_page"))
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw {
		r := failed(fmt.Sprintf("%s: `doclet_page` is a Doclet page's id, as Doclet answers it.", n.tool))
		return "", &r
	}
	return raw, nil
}

// planPage draws or removes the page that documents an entry.
func planPage(n *atlasNow, a map[string]any, m member, remove bool) (atlasPlan, *tools.Result) {
	if strings.TrimSpace(argString(a, "to")) != "" {
		r := failed(fmt.Sprintf("%s: %s names a page by `doclet_page`, never an entry by `to`.", n.tool,
			atlas.RelationDocumentedIn))
		return atlasPlan{}, &r
	}
	page, refusal := n.page(a)
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	ref := m.entry.Ref
	change := "related"
	if remove {
		change = "unrelated"
	}
	plan := atlasPlan{entry: ref, said: said(change, ref)}
	plan.said["relation"], plan.said["doclet_page"] = string(atlas.RelationDocumentedIn), page
	at := slices.IndexFunc(m.entry.Relations, func(ed atlas.Edge) bool {
		return ed.Relation == atlas.RelationDocumentedIn && ed.External == atlas.ExternalPage && ed.Name == page
	})
	if at >= 0 {
		ed := m.entry.Relations[at]
		if src, ok := declared(ed.Source, ed.Others); ok {
			if !remove {
				return plan, nil
			}
			return atlasPlan{}, n.declaredRefusal(fmt.Sprintf("the page that documents %s", ref), src)
		}
		if !remove && ed.Source.Kind == atlas.SourcePerson && ed.Standing == atlas.StandingSettled {
			return plan, nil
		}
	} else if remove {
		return plan, nil
	}
	claim, refusal := n.claim(atlas.Names(ref, atlas.RelationDocumentedIn, atlas.ExternalPage, page, n.person, n.at))
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	if remove {
		if claim, refusal = n.claim(atlas.Denial(*claim, n.person, n.at)); refusal != nil {
			return atlasPlan{}, refusal
		}
	}
	plan.steps = []atlasStep{{name: change, key: page, claim: claim}}
	return plan, nil
}

// owner is the seat or the unit of the chart a write names as an owner, the
// zero edge where it names neither.
func (n *atlasNow) owner(seat, unit string) (atlas.Edge, *tools.Result) {
	switch {
	case seat != "":
		if n.chart.Role(seat) == nil {
			r := failed(fmt.Sprintf("%s: no seat of the chart has the handle `seat` names.", n.tool))
			return atlas.Edge{}, &r
		}
		return atlas.Edge{External: atlas.ExternalSeat, Name: seat}, nil
	case unit != "":
		u := n.chart.UnitByRef(unit)
		if u == nil {
			r := failed(fmt.Sprintf("%s: no unit of the chart has the key `unit` names.", n.tool))
			return atlas.Edge{}, &r
		}
		return atlas.Edge{External: atlas.ExternalUnit, Name: u.Key()}, nil
	}
	return atlas.Edge{}, nil
}

func planOwner(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
	m, refusal := n.held(a, "atlas_entry")
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	ref := m.entry.Ref
	seat, unit, clear := strings.TrimSpace(argString(a, "seat")), strings.TrimSpace(argString(a, "unit")),
		argBool(a, "clear")
	given := 0
	for _, set := range []bool{seat != "", unit != "", clear} {
		if set {
			given++
		}
	}
	if given != 1 {
		r := failed(fmt.Sprintf("%s: name the owner by exactly one of `seat` (a handle) and `unit` (a "+
			"key), or pass `clear`.", n.tool))
		return atlasPlan{}, &r
	}
	plan := atlasPlan{entry: ref, said: said("owner set", ref)}
	want, refusal := n.owner(seat, unit)
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	if want.External == "" {
		plan.said["change"] = "owner cleared"
	} else {
		plan.said["owner"] = string(want.External) + ":" + want.Name
	}
	// THE OWNER A PERSON NAMED BEFORE IS WITHDRAWN, and so is one an
	// integration only suggested: an owner is one seat or one unit, and a
	// person's word on it is the whole of it. One the settings or Gitlet
	// declare is changed there.
	// THE NEW OWNER FIRST: a write stopped part of the way leaves two owners
	// shown, never none.
	kept := false
	var unown []atlasStep
	for _, ed := range m.entry.Relations {
		if ed.Relation != atlas.RelationOwnedBy {
			continue
		}
		if src, ok := declared(ed.Source, ed.Others); ok {
			return atlasPlan{}, n.declaredRefusal(fmt.Sprintf("who owns %s", ref), src)
		}
		if ed.External == want.External && ed.Name == want.Name {
			kept = ed.Source.Kind == atlas.SourcePerson && !ed.Suggested && ed.Standing == atlas.StandingSettled
			continue
		}
		owner, bad := n.claim(atlas.Names(ref, atlas.RelationOwnedBy, ed.External, ed.Name, n.person, n.at))
		if bad != nil {
			return atlasPlan{}, bad
		}
		denial, bad := n.claim(atlas.Denial(*owner, n.person, n.at))
		if bad != nil {
			return atlasPlan{}, bad
		}
		unown = append(unown, atlasStep{name: "unown", key: string(ed.External) + ":" + ed.Name,
			claim: denial})
	}
	if want.External != "" && !kept {
		owner, bad := n.claim(atlas.Names(ref, atlas.RelationOwnedBy, want.External, want.Name, n.person, n.at))
		if bad != nil {
			return atlasPlan{}, bad
		}
		plan.steps = append(plan.steps, atlasStep{name: "own", claim: owner})
	}
	plan.steps = append(plan.steps, unown...)
	return plan, nil
}

func planRetire(retire bool) func(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
	return func(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
		m, bad := n.held(a, "atlas_entry")
		if bad != nil {
			return atlasPlan{}, bad
		}
		ref := m.entry.Ref
		change := "restored"
		if retire {
			change = "retired"
		}
		plan := atlasPlan{entry: ref, said: said(change, ref)}
		for _, s := range m.entry.Sources {
			if s.Kind.Computed() && retire {
				return atlasPlan{}, n.declaredRefusal(ref.String(), s)
			}
		}
		if (m.entry.State == atlas.StateRetired) == retire {
			return plan, nil
		}
		exists, bad := n.claim(atlas.Exists(ref, n.person, n.at))
		if bad != nil {
			return atlasPlan{}, bad
		}
		if retire {
			if exists, bad = n.claim(atlas.Denial(*exists, n.person, n.at)); bad != nil {
				return atlasPlan{}, bad
			}
		}
		plan.steps = []atlasStep{{name: change, claim: exists}}
		return plan, nil
	}
}

// pair is the two entries a merge or a split names: each the member its
// argument named, of one kind and not one member twice.
func (n *atlasNow) pair(a map[string]any) (member, member, *tools.Result) {
	one, refusal := n.held(a, "atlas_entry")
	if refusal != nil {
		return member{}, member{}, refusal
	}
	other, refusal := n.held(a, "other")
	if refusal != nil {
		return member{}, member{}, refusal
	}
	switch {
	case one.ref == other.ref:
		r := failed(fmt.Sprintf("%s: `atlas_entry` and `other` name one entry.", n.tool))
		return member{}, member{}, &r
	case one.ref.Kind != other.ref.Kind:
		r := failed(fmt.Sprintf("%s: a %s and a %s are never one entry.", n.tool, one.ref.Kind, other.ref.Kind))
		return member{}, member{}, &r
	}
	return one, other, nil
}

// apart is the person's "not the same" that keeps a and b apart now, where one
// does: the newest of those recorded between them, in either direction, which
// is what the fold reads the pair's standing by, unless it is withdrawn.
func (n *atlasNow) apart(a, b atlas.Ref) (atlas.Claim, bool) {
	var newest atlas.Claim
	for _, c := range n.recorded {
		if c.Facet.Kind != atlas.FacetDistinct || c.Source.Kind != atlas.SourcePerson {
			continue
		}
		if t := c.Facet.Target.Entry; (c.Subject != a || t != b) && (c.Subject != b || t != a) {
			continue
		}
		if c.ObservedAt.After(newest.ObservedAt) || c.ObservedAt.Equal(newest.ObservedAt) &&
			c.Position > newest.Position {
			newest = c
		}
	}
	return newest, newest.Subject != (atlas.Ref{}) && !newest.Denied
}

// keptApart is every "not the same" that keeps two shown entries apart: one
// standing between any member of the first and any member of the second,
// since any one of them blocks the fold's join of the two groups.
func (n *atlasNow) keptApart(one, other atlas.Entry) []atlas.Claim {
	var out []atlas.Claim
	for _, a := range one.Members {
		for _, b := range other.Members {
			if d, ok := n.apart(a, b); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

func planMerge(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
	one, other, refusal := n.pair(a)
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	plan := atlasPlan{entry: one.ref, said: said("merged", one.ref)}
	plan.said["other"] = other.ref.ID().String()
	if one.entry.ID == other.entry.ID {
		return plan, nil
	}
	// "THE SAME" FIRST, then every "not the same" between the two entries'
	// members withdrawn: a write stopped part of the way leaves them apart,
	// as they were.
	same, refusal := n.claim(atlas.Same(one.ref, other.ref, n.person, n.at))
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	plan.steps = append(plan.steps, atlasStep{name: "merge", claim: same})
	for _, d := range n.keptApart(one.entry, other.entry) {
		withdrawn, bad := n.claim(atlas.Denial(d, n.person, n.at))
		if bad != nil {
			return atlasPlan{}, bad
		}
		plan.steps = append(plan.steps, atlasStep{name: "withdraw", key: d.Facet.Target.Entry.String(),
			claim: withdrawn})
	}
	return plan, nil
}

func planSplit(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
	one, other, refusal := n.pair(a)
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	plan := atlasPlan{entry: one.ref, said: said("split", one.ref)}
	plan.said["other"] = other.ref.ID().String()
	if _, ok := n.apart(one.ref, other.ref); ok {
		return plan, nil
	}
	distinct, refusal := n.claim(atlas.Distinct(one.ref, other.ref, n.person, n.at))
	if refusal != nil {
		return atlasPlan{}, refusal
	}
	plan.steps = []atlasStep{{name: "split", claim: distinct}}
	return plan, nil
}

func planPurge(n *atlasNow, a map[string]any) (atlasPlan, *tools.Result) {
	entry, value := strings.TrimSpace(argString(a, "atlas_entry")), argString(a, "value")
	if (entry == "") == (value == "") {
		r := failed(fmt.Sprintf("%s: name exactly one of `atlas_entry` and `value`.", n.tool))
		return atlasPlan{}, &r
	}
	if entry != "" {
		m, bad := n.entry(a, "atlas_entry")
		if bad != nil {
			return atlasPlan{}, bad
		}
		// WHAT THE ENGINE DECLARES IS COMPUTED AGAIN ON EVERY READ, so a
		// purge of it would erase nothing it shows: it is removed where it
		// is declared, whether or not the caller's read showed it.
		if src, named := m.ref.EngineNamed(); named {
			return atlasPlan{}, n.declaredRefusal(m.ref.String(), src)
		}
		if m.found {
			if i := slices.IndexFunc(m.entry.Sources, func(s atlas.Source) bool { return s.Kind.Computed() }); i >= 0 {
				return atlasPlan{}, n.declaredRefusal(m.ref.String(), m.entry.Sources[i])
			}
		}
		plan := atlasPlan{entry: m.ref, said: said("purged", m.ref)}
		plan.steps = []atlasStep{{name: "purge", purge: &m.ref}}
		return plan, nil
	}
	switch {
	case strings.TrimSpace(value) != value:
		r := failed(fmt.Sprintf("%s: `value` is the value exactly as the atlas holds it, with no space "+
			"around it.", n.tool))
		return atlasPlan{}, &r
	case !utf8.ValidString(value) || len(value) > atlasdomain.MaxPurgedValueBytes:
		r := failed(fmt.Sprintf("%s: `value` is text of at most %d bytes.", n.tool,
			atlasdomain.MaxPurgedValueBytes))
		return atlasPlan{}, &r
	}
	for _, e := range n.view.Entries {
		for _, member := range e.Members {
			i := slices.IndexFunc(e.Sources, func(s atlas.Source) bool { return s.Kind.Computed() })
			if member.Key == value && i >= 0 {
				return atlasPlan{}, n.declaredRefusal("an entry keyed by that value", e.Sources[i])
			}
		}
		for _, fv := range e.Facts {
			if fv.Value.Text != value {
				continue
			}
			if src, ok := declared(fv.Source, fv.Others); ok {
				return atlasPlan{}, n.declaredRefusal("that value", src)
			}
		}
	}
	// THE VALUE IS NEVER STATED AGAIN: the answer says a value was purged, and
	// the record names its digest.
	plan := atlasPlan{said: map[string]any{"change": "purged", "purged": "value"}}
	plan.steps = []atlasStep{{name: "purge-value", digest: atlasdomain.Digest(value)}}
	return plan, nil
}
