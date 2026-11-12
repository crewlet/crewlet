package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/words"
)

// THE ATLAS'S AGENT TOOLS: how an agent records what its own work showed it
// about what the company runs on, and says that something the atlas holds is
// wrong or gone (ADR-0132).
//
// # Only what a call of the same turn showed
//
// A record cites a tool the agent called in this turn (`seen_in`), and the
// engine reads the cited call's own answer, as the model was handed it
// ([turnctx.Seen]): a new entry's identifier, every fact's value and a
// relation's target must stand in that text. A value no cited answer shows is
// refused, so an agent cannot record a host no tool returned.
//
// # The engine derives the confidence
//
// What the cited call is evidence of is read off its TOOL ([evidenceOf]),
// never off anything the agent says: a server's or a vendor's own answer, read
// with the company's credential, makes the record likely; a repository file
// makes it likely as declared in code; anything else (a message, a page, a
// work item, a colleague's answer, the atlas itself) is text anybody could
// have written, and the record stays unconfirmed and hidden until a person
// confirms it.
//
// # What an agent never does
//
// It never removes or retires anything: a dispute is shown beside the value
// and never wins ([atlas.Dispute]). It never sets a fact of meaning, an owner
// or a dependency, which the vocabulary refuses. It records nothing in a turn
// somebody outside the company woke, or somebody without Atlas: the authority
// gate refuses the call before it reaches here (ADR-0109).

// The agent tools' names, each its authority row's action.
const (
	AtlasRecordTool  = string(authz.ActionAtlasRecord)
	AtlasDisputeTool = string(authz.ActionAtlasDispute)
)

// AtlasAgentTools are the two, in the order a catalogue lists them.
var AtlasAgentTools = []string{AtlasRecordTool, AtlasDisputeTool}

// The bounds on what an agent records, each sized against a turn's real work.
const (
	// AtlasRecordsPerTurn is the most records and disputes one turn makes: a
	// turn that inventories a whole server (its services, their ports and
	// versions) fits, and a loop does not.
	AtlasRecordsPerTurn = 20

	// AtlasRecordsPerDay is the most one seat makes in a UTC day, across all
	// its turns: ten full turns, which bounds a seat stuck in a loop at about
	// 200 KiB of log a day.
	AtlasRecordsPerDay = 200

	// atlasRecordFactsMax is the most facts one record states about its
	// entry: more than any kind carries facts an agent can observe.
	atlasRecordFactsMax = 8

	// atlasReconfirmAfter is how long a seat's own claim stands before the
	// same record is written again rather than answered as already recorded:
	// a week, well inside [atlas.StaleAfter], so a fact an agent keeps meeting
	// never goes stale, and one slot costs at most a row a week of history.
	atlasReconfirmAfter = 7 * 24 * time.Hour
)

// AtlasAgentLog is the atlas's log as an agent's record reaches it: through
// the estate router on a running node, so a record made on a node holding no
// data is a data node's.
type AtlasAgentLog interface {
	Observe(ctx context.Context, c atlasdomain.Call, claim atlas.Claim,
		seen atlasdomain.Observation) (statelog.Result, error)
}

// AtlasRecordDeps are what the agent tools record through. The zero value
// serves neither.
type AtlasRecordDeps struct {
	// Log is the atlas's log. Nil omits both tools.
	Log AtlasAgentLog

	// Allow counts one record against the seat's allowance for the UTC day
	// holding now, and reports whether it stayed within limit. An error is
	// "the store could not say", and the record is refused: the allowance
	// bounds what a misled agent writes, so it fails closed. Required.
	Allow func(ctx context.Context, agentID string, limit int, now time.Time) (bool, error)

	// Now is the clock a claim's instant is read off; nil is the wall clock.
	Now func() time.Time
}

func (d AtlasRecordDeps) serves() bool { return d.Log != nil && d.Allow != nil }

func (d AtlasRecordDeps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now().UTC()
}

// atlasAgentCandidates are the agent tools, over the atlas read their records
// are decided on.
func atlasAgentCandidates(deps AtlasRecordDeps, read atlasReader) []candidate {
	on := deps.serves() && read.deps.Settings != nil
	return []candidate{
		{&atlasAgentWrite{dispute: false, deps: deps, read: read}, on},
		{&atlasAgentWrite{dispute: true, deps: deps, read: read}, on},
	}
}

// atlasAgentAnnotations are the agent tools' hints: each writes a surface
// every holder of Atlas reads, removes nothing, and made again states the
// same thing.
func atlasAgentAnnotations(name string) (tools.Annotations, bool) {
	if !slices.Contains(AtlasAgentTools, name) {
		return tools.Annotations{}, false
	}
	return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, Idempotent: mcp.Yes, OpenWorld: mcp.Yes}, true
}

// evidenceOf is what a cited call is evidence of, read off its tool and where
// the tool came from. THE CLOSED END IS NONE: a tool nobody listed here shows
// text anybody could have written.
func evidenceOf(c *config.Company, a turnctx.Answer) atlas.Evidence {
	if server, ok := mcp.OriginServer(a.Origin); ok {
		// A VENDOR'S OWN ANSWER, read with the seat's credential there: a
		// per-seat server the company bound to a seat credential. Any other
		// tool server may fetch a web page as readily as a fact.
		if c != nil && slices.ContainsFunc(c.MCPServers, func(s config.MCPServer) bool {
			return s.Name == server && len(s.SeatBindings()) > 0
		}) {
			return atlas.EvidenceCall
		}
		return atlas.EvidenceNone
	}
	switch a.Tool {
	case SSHReadTool, SSHOperateTool:
		return atlas.EvidenceCall
	case CodeReadTool, CodeSearchTool:
		return atlas.EvidenceFile
	}
	return atlas.EvidenceNone
}

// atlasTurnRecords is the key a turn's count of atlas records is kept under.
type atlasTurnRecords struct{}

// spendTurn counts one record against the turn, and reports whether the turn
// had room. A resumed segment counts afresh; the seat's day does not.
func spendTurn(turn *turnctx.Turn) bool {
	held, _ := turn.LookedUp().LoadOrStore(atlasTurnRecords{}, new(atomic.Int32))
	n, _ := held.(*atomic.Int32)
	return n != nil && n.Add(1) <= AtlasRecordsPerTurn
}

// atlasAgentWrite is atlas_record, or atlas_dispute.
type atlasAgentWrite struct {
	dispute bool
	deps    AtlasRecordDeps
	read    atlasReader
}

var _ tools.SeatCallable = (*atlasAgentWrite)(nil)

func (t *atlasAgentWrite) Name() string {
	if t.dispute {
		return AtlasDisputeTool
	}
	return AtlasRecordTool
}

func (t *atlasAgentWrite) Description() string {
	if t.dispute {
		return "Say that something the company's atlas holds is wrong or gone: an entry that is no longer " +
			"there (`gone`), a fact whose value is wrong (`wrong_value`), or a relation that is not there " +
			"(`wrong_link`). Name in `seen_in` the tool you called in this turn that looked at the entry. " +
			"A dispute removes nothing: it is shown beside what the atlas holds, for a person to decide. " +
			"When you know the right value, record it with " + AtlasRecordTool + " instead."
	}
	return "Record in the company's atlas a durable fact about what the company runs on that a tool call of " +
		"this turn showed you and the atlas lacks: a service, a database, a queue, a host name, a version, a " +
		"port, where something runs. Name in `seen_in` the tool whose answer showed it; every value you " +
		"record must appear in that answer, and one that does not is refused. The engine decides how far " +
		"the record is trusted from that call: a server's or a vendor's own answer makes it likely, and " +
		"anything you only read in a message or a page stays hidden until a person confirms it. Never " +
		"record a secret, an owner, an environment, an importance or what depends on what."
}

func (t *atlasAgentWrite) Parameters() map[string]any {
	seenIn := map[string]any{
		"type": "string",
		"description": "The name of the tool you called in this turn whose answer shows what you record, " +
			"exactly as you called it.",
	}
	if t.dispute {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"atlas_entry": entryArg("The entry the atlas holds"),
				"reason": map[string]any{
					"type": "string", "enum": atlasDisputeReasons(),
					"description": "`gone`: the entry is no longer there. `wrong_value`: the `fact` named is " +
						"wrong. `wrong_link`: the `relation` to `to` is not there.",
				},
				"fact": map[string]any{"type": "string", "enum": atlasFacts(),
					"description": "With `wrong_value`: the fact whose value is wrong."},
				"value": map[string]any{"type": "string",
					"description": "With `wrong_value` on a fact that holds several values: the one that is wrong."},
				"relation": map[string]any{"type": "string", "enum": atlasEntryRelations(),
					"description": "With `wrong_link`: the relation that is not there."},
				"to":      entryArg("With `wrong_link`: the entry the relation points at"),
				"seen_in": seenIn,
			},
			"required": []string{"atlas_entry", "reason", "seen_in"},
		}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"atlas_entry": entryArg("The entry the record is about, which the atlas need not hold yet"),
			"name": map[string]any{"type": "string",
				"description": "The entry's name, where its key is not one."},
			"facts": map[string]any{
				"type":     "array",
				"maxItems": atlasRecordFactsMax,
				"description": "Facts about the entry, each a `fact` its kind carries and the `value` the " +
					"cited answer shows.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"fact":  map[string]any{"type": "string", "enum": atlasFacts()},
						"value": map[string]any{"type": "string"},
					},
					"required": []string{"fact", "value"},
				},
			},
			"relation": map[string]any{"type": "string", "enum": agentRelations(),
				"description": "One relation from the entry to the entry `to` names, from the thing that " +
					"relies to the thing relied on."},
			"to":      entryArg("With `relation`: the entry it points at, which the atlas must hold"),
			"seen_in": seenIn,
		},
		"required": []string{"atlas_entry", "seen_in"},
	}
}

// The reasons a dispute gives, each naming what it is about.
const (
	disputeGone       = "gone"
	disputeWrongValue = "wrong_value"
	disputeWrongLink  = "wrong_link"
)

func atlasDisputeReasons() []string {
	return []string{disputeGone, disputeWrongValue, disputeWrongLink}
}

func (t *atlasAgentWrite) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// grounded is one claim an agent states and the texts the cited answer must
// show for it: any one of them, or the call having been made on the server
// host names. what names the argument a refusal points at, never its value.
type grounded struct {
	claim atlas.Claim
	what  string
	shown []string
	host  string
}

// observing is one agent call being decided: the atlas it is decided on, the
// seat and the turn making it, and the claims it would record.
type observing struct {
	*atlasNow
	seat   string
	turn   string
	claims []grounded
	// known is how many claims the atlas already holds and nothing records.
	known int
}

func (t *atlasAgentWrite) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	seat, err := turn.RequireSeat()
	if err != nil || seat.IsHuman() || strings.TrimSpace(turn.RunID) == "" {
		return refused(tools.RefusalForbidden, fmt.Sprintf("%s recorded nothing: only an agent records in "+
			"the atlas, in a turn of its own, from what a call of that turn showed it.", t.Name())), nil
	}
	settings, running := AtlasSettings{}, false
	if t.read.deps.Settings != nil {
		settings, running = t.read.deps.Settings(ctx)
	}
	if !running {
		return failed(fmt.Sprintf("%s: %s", t.Name(), noAtlas)), nil
	}
	if !settings.Company.Atlas.AgentsRecord() {
		return refused(tools.RefusalForbidden, fmt.Sprintf("%s recorded nothing: this company has turned "+
			"agents' atlas records off (`atlas.agent_records`). Do not call it again; tell whoever you are "+
			"working for what you found instead.", t.Name())), nil
	}
	actor, err := actorFor(ctx, turn, nil)
	if err != nil {
		return faulted(ctx, t.Name(), err, fmt.Sprintf("%s could not tell who is recording, so it recorded "+
			"nothing: %s.", t.Name(), faultSaid)), nil
	}
	actor.Calls = turn.CallLog()

	n, refusal := readAtlasNow(ctx, t.read, turn, t.Name(), t.deps.now())
	if refusal != nil {
		return *refusal, nil
	}
	o := &observing{atlasNow: n, seat: seat.Handle(), turn: turn.RunID}
	subject, refusal := o.plan(t.dispute, args)
	if refusal != nil {
		return *refusal, nil
	}
	if len(o.claims) == 0 {
		r, _ := jsonResult(map[string]any{"entry": subject.ID().String(), "recorded": 0,
			"already_known": o.known, "said": "The atlas already holds this, so nothing was recorded."})
		return r, nil
	}
	evidence, refusal := o.ground(turn, settings.Company, argString(args, "seen_in"))
	if refusal != nil {
		return *refusal, nil
	}
	if !spendTurn(turn) {
		return refused(tools.RefusalQuotaExceeded, fmt.Sprintf("%s recorded nothing: a turn records at most %d "+
			"times in the atlas, and this one has. Keep the rest for a later turn.", t.Name(),
			AtlasRecordsPerTurn)), nil
	}
	switch within, err := t.deps.Allow(ctx, turn.AgentID(), AtlasRecordsPerDay, o.at); {
	case err != nil:
		return refusedBy(tools.RefusalUnavailable, waits(), fmt.Sprintf("%s recorded nothing: today's "+
			"allowance of atlas records could not be read right now. Try again shortly.", t.Name())), nil
	case !within:
		return refused(tools.RefusalQuotaExceeded, fmt.Sprintf("%s recorded nothing: this agent records at most "+
			"%d times a day in the atlas, and today's are used. Do not call it again today.", t.Name(),
			AtlasRecordsPerDay)), nil
	}
	return t.write(ctx, actor, o, subject, evidence, args), nil
}

// readAtlasNow reads what a write to the atlas is decided on: the atlas as the
// caller reads it at the instant at, every source this node serves read in
// full, since a source that answered in part may hold the very fact the write
// would claim beside.
func readAtlasNow(ctx context.Context, read atlasReader, turn *turnctx.Turn, tool string,
	at time.Time) (*atlasNow, *tools.Result) {

	n := &atlasNow{tool: tool, at: at}
	answer, gitletRead, why := read.atlas(ctx, turn, tool, atlasdomain.DefaultRead(), at)
	if why != "" {
		r := refusedBy(tools.RefusalUnavailable, waitless(), fmt.Sprintf("%s wrote nothing: %s", tool, why))
		return nil, &r
	}
	if refusal := wholeRead(tool, answer.Coverage, gitletRead); refusal != nil {
		return nil, refusal
	}
	n.view, n.recorded, n.recordedAt, n.gitlet = answer.View, answer.Recorded, answer.RecordedAt, gitletRead
	return n, nil
}

// plan reads the call's arguments into the claims it would record, each in
// the seat's voice with no evidence yet ([observing.ground] derives it), and
// answers the entry the call is about.
func (o *observing) plan(dispute bool, a map[string]any) (atlas.Ref, *tools.Result) {
	if strings.TrimSpace(argString(a, "seen_in")) == "" {
		r := failed(fmt.Sprintf("%s: `seen_in` names the tool you called in this turn whose answer shows "+
			"what you record. A record with no call behind it is refused.", o.tool))
		return atlas.Ref{}, &r
	}
	if dispute {
		return o.planDispute(a)
	}
	return o.planRecord(a)
}

// source is the seat's voice in this turn. The evidence is none until the
// cited call is read, so a claim built before it is checked claims nothing.
func (o *observing) source(evidence atlas.Evidence) atlas.Source {
	return atlas.AgentSource(o.seat, o.turn, evidence)
}

// add keeps one claim to record, unless the atlas already holds it.
func (o *observing) add(c atlas.Claim, err error, what string, shown []string, host string) *tools.Result {
	claim, refusal := o.claim(c, err)
	if refusal != nil {
		return refusal
	}
	o.claims = append(o.claims, grounded{claim: *claim, what: what, shown: shown, host: host})
	return nil
}

// met is the texts an entry is met by where a call prints it: its key's
// identifiers, its members' and its name; and the server id an SSH call made
// on it names, where it is one the SSH settings declare.
func met(m member) (shown []string, host string) {
	refs := []atlas.Ref{m.ref}
	if m.found {
		refs = append(refs, m.entry.Members...)
		if m.entry.Name != "" {
			shown = append(shown, m.entry.Name)
		}
	}
	for _, r := range refs {
		for _, id := range r.Identifiers() {
			if !slices.Contains(shown, id) {
				shown = append(shown, id)
			}
		}
		if id, ok := strings.CutPrefix(r.Key, atlas.HostSSHPrefix+"/"); ok && r.Kind == atlas.KindHost {
			host = id
		}
	}
	return shown, host
}

func (o *observing) planRecord(a map[string]any) (atlas.Ref, *tools.Result) {
	m, refusal := o.entry(a, "atlas_entry")
	if refusal != nil {
		return atlas.Ref{}, refusal
	}
	ref := m.ref
	if m.found {
		ref = m.entry.Ref
	}
	src := o.source(atlas.EvidenceNone)
	if !m.found || m.entry.State == atlas.StateRetired {
		if declarer, named := m.ref.EngineNamed(); named && !m.found {
			return atlas.Ref{}, o.declaredRefusal(fmt.Sprintf("a %s keyed like %s", ref.Kind, ref.Key), declarer)
		}
		shown, host := met(m)
		c, err := atlas.Exists(ref, src, o.at)
		if refusal := o.add(c, err, "the entry `atlas_entry` names", shown, host); refusal != nil {
			return atlas.Ref{}, refusal
		}
	} else if mine, ok := o.own(ref, atlas.Claim{Subject: ref, Facet: atlas.Facet{Kind: atlas.FacetExists}}); ok &&
		o.at.Sub(mine.ObservedAt) >= atlasReconfirmAfter {
		// THE SEAT'S OWN ENTRY, SEEN AGAIN: the record that keeps an entry
		// only this seat reports from going stale.
		shown, host := met(m)
		c, err := atlas.Exists(ref, src, o.at)
		if refusal := o.add(c, err, "the entry `atlas_entry` names", shown, host); refusal != nil {
			return atlas.Ref{}, refusal
		}
	}
	stated, refusal := o.facts(a, ref)
	if refusal != nil {
		return atlas.Ref{}, refusal
	}
	for _, f := range stated {
		c, err := atlas.Says(ref, f.fact, f.value, src, o.at)
		if err == nil && o.holds(m, c) {
			o.known++
			continue
		}
		var shown []string
		if err == nil {
			shown = []string{c.Value.Text}
		}
		if refusal := o.add(c, err, fmt.Sprintf("`%s` (%s)", f.arg, f.fact), shown, ""); refusal != nil {
			return atlas.Ref{}, refusal
		}
	}
	if strings.TrimSpace(argString(a, "relation")) != "" || strings.TrimSpace(argString(a, "to")) != "" {
		r, to, refusal := o.related(a, agentRelations())
		if refusal != nil {
			return atlas.Ref{}, refusal
		}
		c, err := atlas.Relates(ref, r, to.entry.Ref, src, o.at)
		if err == nil && o.holds(m, c) {
			o.known++
		} else {
			shown, host := met(to)
			if refusal := o.add(c, err, "the entry `to` names", shown, host); refusal != nil {
				return atlas.Ref{}, refusal
			}
		}
	}
	if len(o.claims) == 0 && o.known == 0 {
		r := failed(fmt.Sprintf("%s: the atlas already holds %s; state a `name`, `facts` or a `relation` "+
			"a call of this turn showed you about it.", o.tool, ref))
		return atlas.Ref{}, &r
	}
	return ref, nil
}

// agentRelations are the relations an agent records: every relation between
// two entries but a dependency, which a person decides.
func agentRelations() []string {
	return slices.DeleteFunc(atlasEntryRelations(), func(r string) bool {
		return r == string(atlas.RelationDependsOn)
	})
}

// related reads a call's `relation`, one of set, and the entry `to` names as
// the atlas holds it.
func (o *observing) related(a map[string]any, set []string) (atlas.Relation, member, *tools.Result) {
	r := atlas.Relation(strings.TrimSpace(argString(a, "relation")))
	if !slices.Contains(set, string(r)) {
		res := failed(fmt.Sprintf("%s: `relation` is %s. An owner and what depends on what are a person's "+
			"to decide.", o.tool, words.Or(set)))
		return "", member{}, &res
	}
	to, refusal := o.held(a, "to")
	if refusal != nil {
		return "", member{}, refusal
	}
	return r, to, nil
}

// statedFact is one fact a record states, and the argument that states it.
type statedFact struct {
	fact  atlas.Fact
	value string
	arg   string
}

// facts reads a record's `name` and `facts`: each a fact the entry's kind
// carries, stated once.
func (o *observing) facts(a map[string]any, ref atlas.Ref) ([]statedFact, *tools.Result) {
	var out []statedFact
	if name := strings.TrimSpace(argString(a, "name")); name != "" {
		out = append(out, statedFact{fact: atlas.FactName, value: name, arg: "name"})
	}
	raw, listed := argval.List(a["facts"])
	if a["facts"] != nil && !listed {
		r := failed(fmt.Sprintf("%s: `facts` is a list of objects, each a `fact` and a `value`.", o.tool))
		return nil, &r
	}
	if len(raw) > atlasRecordFactsMax {
		r := failed(fmt.Sprintf("%s: `facts` holds at most %d facts a record.", o.tool, atlasRecordFactsMax))
		return nil, &r
	}
	for i, item := range raw {
		arg := fmt.Sprintf("facts[%d]", i)
		entry, ok := argval.Object(item)
		if !ok {
			r := failed(fmt.Sprintf("%s: `%s` is an object with a `fact` and a `value`.", o.tool, arg))
			return nil, &r
		}
		f, refusal := o.fact(entry, ref)
		if refusal != nil {
			return nil, refusal
		}
		value := strings.TrimSpace(argString(entry, "value"))
		if value == "" {
			r := failed(fmt.Sprintf("%s: `%s` states no `value`.", o.tool, arg))
			return nil, &r
		}
		if slices.ContainsFunc(out, func(s statedFact) bool {
			return s.fact == f && (!f.Spec().Many || s.value == value)
		}) {
			r := failed(fmt.Sprintf("%s: `%s` states %s a second time.", o.tool, arg, f))
			return nil, &r
		}
		out = append(out, statedFact{fact: f, value: value, arg: arg})
	}
	return out, nil
}

// own is the seat's own live claim on the slot c fills, where it holds one.
func (o *observing) own(subject atlas.Ref, c atlas.Claim) (atlas.Claim, bool) {
	slot := c.Slot()
	for _, held := range o.recorded {
		if held.Source.Kind == atlas.SourceAgent && held.Source.Name == o.seat && held.Subject == subject &&
			held.Slot() == slot {
			return held, true
		}
	}
	return atlas.Claim{}, false
}

// holds reports whether the atlas already holds what c says, so recording it
// again would add a row and no knowledge: the seat's own claim of the same
// value, made within [atlasReconfirmAfter] with evidence the engine checked
// (an unbacked one is recorded again, since this call may back it); or the
// same value held by anybody the atlas trusts more than an agent.
func (o *observing) holds(m member, c atlas.Claim) bool {
	if mine, ok := o.own(c.Subject, c); ok && !mine.Denied && mine.Value.Equal(c.Value) {
		return mine.Source.Evidence != atlas.EvidenceNone && o.at.Sub(mine.ObservedAt) < atlasReconfirmAfter
	}
	if !m.found {
		return false
	}
	trusted := func(src atlas.Source, confidence atlas.Confidence, suggested bool) bool {
		return src.Kind != atlas.SourceAgent && !suggested && confidence.Rank() < atlas.ConfidenceObserved.Rank()
	}
	switch c.Facet.Kind {
	case atlas.FacetFact:
		lookup := ""
		if c.Facet.Fact.Spec().Many {
			lookup = c.Value.Text
		}
		fv, ok := factOn(m.entry, c.Facet.Fact, lookup)
		return ok && fv.Value.Equal(c.Value) && trusted(fv.Source, fv.Confidence, fv.Suggested)
	case atlas.FacetRelation:
		ed, ok := edgeOn(m.entry, c.Facet.Relation, c.Facet.Target.Entry.ID())
		return ok && trusted(ed.Source, ed.Confidence, ed.Suggested)
	}
	return false
}

func (o *observing) planDispute(a map[string]any) (atlas.Ref, *tools.Result) {
	m, refusal := o.held(a, "atlas_entry")
	if refusal != nil {
		return atlas.Ref{}, refusal
	}
	ref := m.entry.Ref
	if m.entry.State == atlas.StateRetired {
		r := refused(tools.RefusalConflict, fmt.Sprintf("%s recorded nothing: %s is retired already.", o.tool, ref))
		return atlas.Ref{}, &r
	}
	shown, host := met(m)
	src := o.source(atlas.EvidenceNone)
	var disputed atlas.Claim
	switch reason := strings.TrimSpace(argString(a, "reason")); reason {
	case disputeGone:
		disputed = atlas.Claim{Subject: ref, Facet: atlas.Facet{Kind: atlas.FacetExists}}
	case disputeWrongValue:
		f, bad := o.fact(a, ref)
		if bad != nil {
			return atlas.Ref{}, bad
		}
		lookup := ""
		if f.Spec().Many {
			lookup = strings.TrimSpace(argString(a, "value"))
		}
		fv, ok := factOn(m.entry, f, lookup)
		if !ok {
			r := refused(tools.RefusalConflict, fmt.Sprintf("%s recorded nothing: the atlas shows no such %s "+
				"on %s to dispute. Read it with %s.", o.tool, f, ref, AtlasGetTool))
			return atlas.Ref{}, &r
		}
		disputed = atlas.Claim{Subject: ref, Facet: atlas.Facet{Kind: atlas.FacetFact, Fact: f}, Value: fv.Value}
	case disputeWrongLink:
		r, to, bad := o.related(a, atlasEntryRelations())
		if bad != nil {
			return atlas.Ref{}, bad
		}
		if _, ok := edgeOn(m.entry, r, to.entry.ID); !ok {
			res := refused(tools.RefusalConflict, fmt.Sprintf("%s recorded nothing: the atlas shows no %s "+
				"from %s to %s to dispute.", o.tool, r, ref, to.entry.Ref))
			return atlas.Ref{}, &res
		}
		disputed = atlas.Claim{Subject: ref, Facet: atlas.Facet{Kind: atlas.FacetRelation, Relation: r,
			Target: atlas.Target{Entry: to.entry.Ref}}}
	default:
		r := failed(fmt.Sprintf("%s: `reason` is %s.", o.tool, words.Or(atlasDisputeReasons())))
		return atlas.Ref{}, &r
	}
	if mine, ok := o.own(ref, disputed); ok && mine.Denied && o.at.Sub(mine.ObservedAt) < atlasReconfirmAfter {
		o.known++
		return ref, nil
	}
	c, err := atlas.Dispute(disputed, src, o.at)
	if refusal := o.add(c, err, "the entry `atlas_entry` names", shown, host); refusal != nil {
		return atlas.Ref{}, refusal
	}
	return ref, nil
}

// ground holds every claim to the cited call: an answer of the tool seenIn
// names, made in this turn, that shows what each claim states. The newest
// such answer is the evidence, and its class is the record's.
func (o *observing) ground(turn *turnctx.Turn, c *config.Company, seenIn string) (atlas.Evidence, *tools.Result) {
	tool := strings.TrimSpace(seenIn)
	answers, dropped := turn.Answers().Of(tool)
	if len(answers) == 0 {
		why := "this turn made no call of that tool that answered"
		if dropped > 0 {
			why = "this turn no longer holds that call's answer"
		}
		r := refused(tools.RefusalInvalid, fmt.Sprintf("%s recorded nothing: `seen_in` names a tool you "+
			"called in this turn, and %s. Make the read in this turn, then record what it shows.", o.tool, why))
		return "", &r
	}
	var missing []string
	for _, answer := range answers {
		missing = missing[:0]
		for _, g := range o.claims {
			if !g.shownBy(answer) {
				missing = append(missing, g.what)
			}
		}
		if len(missing) == 0 {
			return evidenceOf(c, answer), nil
		}
	}
	r := refused(tools.RefusalInvalid, fmt.Sprintf("%s recorded nothing: no answer of %s in this turn shows "+
		"%s. Record only what the cited call's answer shows, as it shows it.", o.tool, clip(tool),
		strings.Join(missing, ", ")))
	return "", &r
}

// shownBy reports whether one answer shows what g states: one of its texts
// standing in the answer, or the call having been made on the server it is.
func (g grounded) shownBy(a turnctx.Answer) bool {
	if g.host != "" && (a.Tool == SSHReadTool || a.Tool == SSHOperateTool) && argString(a.Args, "host") == g.host {
		return true
	}
	return slices.ContainsFunc(g.shown, func(text string) bool { return atlas.Mentions(a.Output, text) })
}

// write records the call's claims in order, each its own operation derived
// from the call, so a retry of the call is every claim again and the log
// answers each that landed.
func (t *atlasAgentWrite) write(ctx context.Context, actor Actor, o *observing, subject atlas.Ref,
	evidence atlas.Evidence, args map[string]any) tools.Result {

	by := iam.Actor{Name: actor.Handle, Kind: iam.ActorAgent}
	seen := atlasdomain.Observation{Turn: o.turn, Evidence: evidence}
	var last statelog.Result
	for i, g := range o.claims {
		opID := opIDFor(actor, t.Name(), "atlas-observe", stepObject(g.claim.Subject, g.claim.Slot()), args)
		res, err := t.deps.Log.Observe(ctx, atlasdomain.Call{By: by, OpID: opID}, g.claim, seen)
		var unserved *estate.ErrUnserved
		switch {
		case errors.Is(err, estate.ErrOutcomeUnknown) || errors.As(err, &unserved) && unserved.Silent(),
			err == nil && res.Outcome == statelog.OutcomeUnknown:
			return unknownWrite(actor, t.Name(), "the atlas was changed", opID, res.Unvouched,
				unknownNext(res.Unvouched, sameCall(actor, t.Name()), "Read the entry in the atlas again",
					"the same claims are recorded again, which changes nothing"))
		case err != nil:
			failure := atlasWriteFailure(ctx, t.Name(), actor, subject, err)
			if i > 0 {
				failure.Output += fmt.Sprintf(" The first %d of its %d claims landed; %s finishes the rest.",
					i, len(o.claims), orAgain(sameCall(actor, t.Name())))
			}
			return failure
		}
		last = res
	}
	word := atlas.AgentSource(o.seat, o.turn, evidence).Confidence().Display()
	answer := map[string]any{
		"entry": subject.ID().String(), "kind": string(subject.Kind), "key": subject.Key,
		"recorded": len(o.claims), "already_known": o.known, "evidence": string(evidence),
		"shown_as": string(word), "outcome": string(last.Outcome),
	}
	switch {
	case t.dispute && word == atlas.WordUnconfirmed:
		answer["said"] = "Recorded, and kept out of the atlas: the cited call is not a read of a server, a " +
			"vendor or a repository, so nobody is shown this dispute until a person looks."
	case t.dispute:
		answer["said"] = "Recorded beside what the atlas holds, which stays until a person decides."
	case word == atlas.WordUnconfirmed:
		answer["said"] = "Recorded, and kept out of the atlas until a person confirms it: the cited call is " +
			"not a read of a server, a vendor or a repository, so the engine cannot check it."
	}
	r, _ := jsonResult(answer)
	return r
}
