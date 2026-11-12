package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
)

// The atlas tools, each reading the company's atlas on the atlas app
// (ADR-0127), a seat held to whoever woke its turn (ADR-0109).
const (
	// AtlasSearchTool lists the atlas's entries, or those a query or a kind
	// names.
	AtlasSearchTool = toolname.AtlasSearch
	// AtlasGetTool reads one entry in full, and what relies on it.
	AtlasGetTool = toolname.AtlasGet
	// AtlasTraceTool walks what depends on one entry, or what it depends on.
	AtlasTraceTool = toolname.AtlasTrace
)

// The listing's page: fifty entries by default, a screen of them, and at most
// five hundred, the most a person reads through before narrowing by kind. Each
// page is cut further, whole entries, wherever it outgrows one tool answer.
const (
	defaultAtlasList = 50
	maxAtlasList     = 500
)

// AtlasSettings is the settings an atlas read reads: the document this node runs
// (read only through its redacting copy), its revision, empty where this node
// cannot tell, and the addresses the deployment answers at.
type AtlasSettings struct {
	Company    *config.Company
	Revision   string
	Deployment derive.Deployment
}

// AtlasDeps are what the atlas tools and questions read the atlas through.
// The zero value serves none of them.
type AtlasDeps struct {
	// Settings is the company settings this node runs, read per call, and
	// false where it runs none yet. Nil omits every atlas tool.
	Settings func(ctx context.Context) (AtlasSettings, bool)
	// Records is every record source of the engine's this node reaches, read
	// per call, Gitlet aside: each caller adds Gitlet as itself. A source this
	// node does not reach is nil, which the read reports as unread.
	Records func(ctx context.Context) gather.Sources
	// Log is the atlas's own log as this node reaches it, read per call, and
	// false where it reaches no copy. Nil serves no question about the log.
	Log func(ctx context.Context) (AtlasLogReads, bool)
}

// AtlasLogReads is the atlas's log read as lines and counted
// (atlasdomain.Reader, or the estate router's reads of one).
type AtlasLogReads interface {
	Log(ctx context.Context, q atlasdomain.LogQuery) (atlasdomain.LogPage, error)
	Activity(ctx context.Context, since time.Time, fresh statelog.Freshness) (atlasdomain.Activity, error)
}

// atlasLog is the log this node reaches, or [gather.ErrNoSource].
func (d AtlasDeps) atlasLog(ctx context.Context) (AtlasLogReads, error) {
	if d.Log != nil {
		if log, ok := d.Log(ctx); ok {
			return log, nil
		}
	}
	return nil, fmt.Errorf("the atlas's log: %w", gather.ErrNoSource)
}

// AtlasLog is one page of the atlas's log as this node reaches it. Whose
// lines a reader is shown is the caller's to decide, against the atlas it
// reads for them.
func (d AtlasDeps) AtlasLog(ctx context.Context, q atlasdomain.LogQuery) (atlasdomain.LogPage, error) {
	log, err := d.atlasLog(ctx)
	if err != nil {
		return atlasdomain.LogPage{}, err
	}
	return log.Log(ctx, q)
}

// AtlasActivity is the atlas's log counted since an instant, and each entry's
// newest record, as this node reaches it.
func (d AtlasDeps) AtlasActivity(ctx context.Context, since time.Time,
	fresh statelog.Freshness) (atlasdomain.Activity, error) {
	log, err := d.atlasLog(ctx)
	if err != nil {
		return atlasdomain.Activity{}, err
	}
	return log.Activity(ctx, since, fresh)
}

// AtlasRead is one read of the atlas for one caller: Gitlet read as asker
// where reads is set (or why it is not, unread), every record this node
// reaches, and the settings it runs. False where it runs no company yet.
func (d AtlasDeps) AtlasRead(ctx context.Context, reads gather.GitletReads, unread string, asker gitlet.Principal,
	fresh statelog.Freshness, now time.Time) (gather.Answer, bool) {
	if d.Settings == nil {
		return gather.Answer{}, false
	}
	settings, running := d.Settings(ctx)
	if !running {
		return gather.Answer{}, false
	}
	var sources gather.Sources
	if d.Records != nil {
		sources = d.Records(ctx)
	}
	sources.Gitlet = reads
	return gather.Read(ctx, sources, gather.Request{
		Company: settings.Company, Revision: settings.Revision, Deployment: settings.Deployment,
		Asker: asker, GitletUnread: unread, Fresh: fresh, Now: now,
	}), true
}

// atlasReader is what the atlas tools share: the atlas read as the caller.
// Gitlet is read as the caller and only where they may read code (ADR-0110),
// since the atlas never opens another app's records (ADR-0127).
type atlasReader struct {
	deps      AtlasDeps
	code      CodeDeps
	authorize Authorizer
}

// atlasRead is one read of the atlas, as one caller may read it.
type atlasRead struct {
	view     atlas.View
	coverage []gather.Coverage
	// withheld is the values left out for being shaped like a credential,
	// and hidden the private addresses left out of an agent's read as
	// outside its own reach, each counted and never shown.
	withheld int
	hidden   int
	// read is the answer's provenance: who wrote what it carries.
	read *foreignRead
}

// read reads the atlas as the call's caller. An administrator reads what
// only administrators read elsewhere; an agent seat reads a private address
// only of an entry its own access reaches; and the answer names who wrote
// what it carries, at the least trust any of them earns.
func (r atlasReader) read(ctx context.Context, turn *turnctx.Turn, tool string) (atlasRead, *tools.Result) {
	settings, running := AtlasSettings{}, false
	if r.deps.Settings != nil {
		settings, running = r.deps.Settings(ctx)
	}
	if !running {
		res := failed(fmt.Sprintf("%s: %s", tool, noAtlas))
		return atlasRead{}, &res
	}
	answer, _, _ := r.atlas(ctx, turn, tool, seatRead, time.Now().UTC())
	principal, _ := iam.From(ctx)
	out := atlasRead{view: atlas.For(answer.View, atlas.Reader{Admin: principal.Can(iam.GrantAdmin)}),
		coverage: answer.Coverage, withheld: len(answer.Withheld)}
	if turn != nil && turn.Seat != nil && !turn.Seat.IsHuman() {
		out.view, out.hidden = AtlasForSeat(out.view, settings.Company, turn.Seat)
	}
	out.read = newForeignRead(turn, iam.ActorFor(principal).Name)
	return out, nil
}

// noAtlas is why a node that runs no company yet answers no atlas.
const noAtlas = "this node runs no company yet, so there is no atlas to read. " +
	"Try again once its settings are applied."

// atlas is the atlas as this call's caller reads it, at fresh: every source
// this node reaches, what people recorded among them, and Gitlet as the caller
// and only where they may read code (gitlet says whether it was read).
// missing says why there is no atlas at all: a node that runs no company yet.
func (r atlasReader) atlas(ctx context.Context, turn *turnctx.Turn, tool string, fresh statelog.Freshness,
	now time.Time) (answer gather.Answer, gitlet bool, missing string) {

	reads, asker, why := r.gitlet(ctx, turn, tool)
	answer, running := r.deps.AtlasRead(ctx, reads, why, asker, fresh, now)
	if !running {
		return gather.Answer{}, false, noAtlas
	}
	return answer, reads != nil, ""
}

// attribute names who wrote what entries carry on the answer's envelope: a
// person and an agent by their seats, the settings as the company's and the
// engine's records as the engine's, each read by the rule for a surface the
// engine hosts; and a vendor's listing by the rule for a vendor's site, where
// nobody named is an outside party, the least a reader may rely on.
func (a atlasRead) attribute(entries []atlas.Entry) {
	writers, listed := atlas.Writers(entries)
	if len(writers) > 0 {
		a.read.attribute(writers, false)
	}
	if listed {
		a.read.trusts = append(a.read.trusts, provenance.TrustFor(a.read.self, nil))
	}
}

// AtlasForSeat is v as an agent seat reads it: no signal only administrators
// read, a private address only of an entry its own access reaches (ADR-0109),
// and nothing only an agent's unbacked report says ([atlas.Vouched]), so one
// agent's report never reaches another as a fact (ADR-0132). It answers how
// many private values it left out.
func AtlasForSeat(v atlas.View, c *config.Company, seat *org.Role) (atlas.View, int) {
	return atlas.Within(atlas.Vouched(atlas.For(v, atlas.Reader{})), seatReach(c, seat))
}

// seatReach is which entries an agent seat's own access reaches: the servers
// its SSH access grants, the tool servers it is granted, the accounts it
// holds one at, its Gitlet repositories (Gitlet lists it no other), the
// models one of its own chains names, and the embeddings its searches use.
func seatReach(c *config.Company, seat *org.Role) func(atlas.Entry) bool {
	var hosts []string
	if c != nil {
		hosts = c.SSHReachOf(seat.Handle()).HostIDs()
	}
	chained := func(key string) bool {
		return c != nil && slices.ContainsFunc(c.ProviderReferences(key), func(r config.ProviderReference) bool {
			return r.Seat == seat.Handle()
		})
	}
	return func(e atlas.Entry) bool {
		for _, m := range e.Members {
			switch m.Kind {
			case atlas.KindHost:
				if id, ok := strings.CutPrefix(m.Key, atlas.HostSSHPrefix+"/"); ok && slices.Contains(hosts, id) {
					return true
				}
			case atlas.KindToolServer:
				name := strings.TrimPrefix(m.Key, atlas.ToolServerPrefix+"/")
				if c != nil && slices.ContainsFunc(c.MCPServers, func(s config.MCPServer) bool {
					return s.Name == name && s.Grants(seat)
				}) {
					return true
				}
			case atlas.KindRepository:
				if strings.HasPrefix(m.Key, atlas.GitletPrefix+"/") {
					return true
				}
			case atlas.KindModel:
				if key, ok := strings.CutPrefix(m.Key, atlas.ModelLLMPrefix+"/"); ok && chained(key) ||
					m.Key == atlas.ModelEmbeddings {
					return true
				}
			}
		}
		return slices.ContainsFunc(e.Holders, func(h atlas.Holder) bool { return h.Seat == seat.Handle() })
	}
}

// gitlet is the Gitlet source this call reads and who it reads as: none where
// this surface serves no code or the caller (or whoever a seat's turn works
// for) may not read it, and then why says which, for the answer's coverage.
func (r atlasReader) gitlet(ctx context.Context, turn *turnctx.Turn, tool string) (
	src gather.GitletReads, asker gitlet.Principal, why string) {

	if !r.code.serves() || r.authorize == nil {
		return nil, gitlet.Principal{}, ""
	}
	switch err := r.authorize(ctx, authz.ActionCodeRead, authz.Object{}); {
	case errors.Is(err, ErrUndecidable):
		return nil, gitlet.Principal{}, "whether you may read the company's code could not be " +
			"decided right now, so no repository was read; ask again shortly"
	case err != nil:
		return nil, gitlet.Principal{}, "you do not use Gitlet, so no repository was read"
	}
	caller, refusal := r.code.callerOn(ctx, turn, tool, authz.ActionCodeRead)
	if refusal != nil {
		return nil, gitlet.Principal{}, "whoever this turn works for may not read the company's " +
			"code now, so no repository was read"
	}
	return atlasGitlet{code: r.code, caller: caller}, caller.asker, ""
}

// atlasGitlet is Gitlet as one call reads it: every query asked as the
// call's caller, with its ceiling, through [CodeDeps.read].
type atlasGitlet struct {
	code   CodeDeps
	caller codeCaller
}

func (g atlasGitlet) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return g.code.read(ctx, g.caller, q)
}

// atlasSearch is atlas_search: the atlas's entries as the caller may read
// them.
type atlasSearch struct{ atlasReader }

var _ tools.SeatCallable = (*atlasSearch)(nil)

func (t *atlasSearch) Name() string { return AtlasSearchTool }

func (t *atlasSearch) Description() string {
	return "Search the company's atlas: the map of everything it runs on (vendor accounts, domains, " +
		"hostnames, servers, data stores, repositories, monitoring, tool servers and models). Lists the " +
		"entries a `query` or a `kind` names, each with its facts, where each fact came from, and what it " +
		"depends on. Repositories are listed only for a caller who uses Gitlet. Read it before probing a " +
		"server or a vendor for what the company already knows."
}

func (t *atlasSearch) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Part of an entry's name or key, or its id. Omit for every entry.",
			},
			"kind": map[string]any{
				"type":        "string",
				"enum":        atlasKinds(),
				"description": "Only entries of this kind.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many entries to return, 1..%d (default %d).",
					maxAtlasList, defaultAtlasList),
			},
		},
	}
}

func (t *atlasSearch) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// atlasCoverage is what every atlas tool's answer says of its read: which
// sources were read in full, how many values were left out for being shaped
// like a credential, and how many private addresses were left out as outside
// the reading agent's reach, each counted and never shown.
type atlasCoverage struct {
	Coverage []gather.Coverage `json:"coverage"`
	Withheld int               `json:"withheld,omitempty"`
	Hidden   int               `json:"private_withheld,omitempty"`
}

func (a atlasRead) said() atlasCoverage {
	return atlasCoverage{Coverage: a.coverage, Withheld: a.withheld, Hidden: a.hidden}
}

// atlasList is atlas_search's answer: the entries, how many matched, and what
// the read says of itself.
type atlasList struct {
	Entries []atlas.Entry `json:"entries"`
	Total   int           `json:"total"`
	atlasCoverage
}

func (t *atlasSearch) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	kind := atlas.Kind(strings.TrimSpace(argString(args, "kind")))
	if kind != "" && !kind.Valid() {
		return failed(fmt.Sprintf("%s: `kind` is %s, not %q.", t.Name(), words.Or(atlas.Kinds),
			clip(string(kind)))), nil
	}
	limit := argInt(args, "limit", defaultAtlasList)
	if limit < 1 || limit > maxAtlasList {
		return failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), maxAtlasList)), nil
	}
	a, refusal := t.read(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	matched := a.view.Select(atlas.Query{Text: argString(args, "query"), Kind: kind})
	shown := matched[:min(limit, len(matched))]
	a.attribute(shown)
	rendered, err := fitPages(len(shown), func(k int) (json.RawMessage, error) {
		return a.read.render(atlasList{Entries: shown[:k], Total: len(matched), atlasCoverage: a.said()})
	})
	if err != nil {
		return labelFault(ctx, t.Name(), err), nil
	}
	return compactAnswer(rendered, "Ask for fewer with `limit`, or narrow by `kind` or `query`.")
}

// atlasEntryArg is the entry a call names: by `id`, or by `name`, an entry's
// exact name or key. A name more than one entry answers to is refused,
// naming each by its id.
func (a atlasRead) entryArg(tool string, args map[string]any) (atlas.Entry, *tools.Result) {
	if raw := strings.TrimSpace(argString(args, "id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			res := failed(fmt.Sprintf("%s: `id` is an atlas entry's id, as %s answers it.", tool, AtlasSearchTool))
			return atlas.Entry{}, &res
		}
		e, ok := a.view.Find(id)
		if !ok {
			res := failed(fmt.Sprintf("%s: no entry of the atlas you may read has this id. Find it with "+
				"%s.", tool, AtlasSearchTool))
			return atlas.Entry{}, &res
		}
		return e, nil
	}
	name := strings.ToLower(strings.TrimSpace(argString(args, "name")))
	if name == "" {
		res := failed(fmt.Sprintf("%s: name the entry by `id` or by `name`.", tool))
		return atlas.Entry{}, &res
	}
	var found []atlas.Entry
	for _, e := range a.view.Entries {
		if e.State != atlas.StateRetired && (strings.ToLower(e.Name) == name || strings.ToLower(e.Ref.Key) == name) {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 0:
		res := failed(fmt.Sprintf("%s: no entry of the atlas you may read is named that. Search for it "+
			"with %s.", tool, AtlasSearchTool))
		return atlas.Entry{}, &res
	case 1:
		return found[0], nil
	}
	ids := make([]string, len(found))
	for i, e := range found {
		ids[i] = e.ID.String() + " (" + string(e.Ref.Kind) + ")"
	}
	res := failed(fmt.Sprintf("%s: %d entries are named that; name one by `id`: %s.", tool, len(found),
		strings.Join(ids, ", ")))
	return atlas.Entry{}, &res
}

// entryParameters is how an atlas tool names one entry.
func entryParameters(extra map[string]any) map[string]any {
	props := map[string]any{
		"id": map[string]any{
			"type":        "string",
			"description": "The entry's id, as " + AtlasSearchTool + " answers it.",
		},
		"name": map[string]any{
			"type":        "string",
			"description": "The entry's exact name or key, where `id` is not given.",
		},
	}
	for k, v := range extra {
		props[k] = v
	}
	return map[string]any{"type": "object", "properties": props}
}

// atlasGet is atlas_get: one entry in full, and what relies on it.
type atlasGet struct{ atlasReader }

var _ tools.SeatCallable = (*atlasGet)(nil)

func (t *atlasGet) Name() string { return AtlasGetTool }

func (t *atlasGet) Description() string {
	return "Read one entry of the company's atlas in full: its facts with where each came from and how sure " +
		"the atlas is of it, its relations, its use, the agents holding accounts at it, its open signals, " +
		"and every entry that relies on it. Name it by `id` or by its exact `name`."
}

func (t *atlasGet) Parameters() map[string]any { return entryParameters(nil) }

func (t *atlasGet) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// atlasEntryAnswer is atlas_get's answer.
type atlasEntryAnswer struct {
	Entry  atlas.Entry  `json:"entry"`
	UsedBy []atlas.Link `json:"used_by"`
	atlasCoverage
}

func (t *atlasGet) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	a, refusal := t.read(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	e, refusal := a.entryArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	usedBy := a.view.UsedBy(e.ID)
	carried := []atlas.Entry{e}
	for _, l := range usedBy {
		if found, ok := a.view.Find(l.Entry.ID); ok {
			carried = append(carried, found)
		}
	}
	a.attribute(carried)
	return a.read.answer(ctx, t.Name(), atlasEntryAnswer{Entry: e, UsedBy: usedBy, atlasCoverage: a.said()}, "")
}

// atlasTrace is atlas_trace: what depends on one entry, or what it depends on.
type atlasTrace struct{ atlasReader }

var _ tools.SeatCallable = (*atlasTrace)(nil)

func (t *atlasTrace) Name() string { return AtlasTraceTool }

func (t *atlasTrace) Description() string {
	return fmt.Sprintf("Walk the company's atlas from one entry: `up` for what depends on it (what breaks if "+
		"it breaks), `down` for what it depends on, hop by hop, at most %d hops and %d entries. Name the "+
		"entry by `id` or by its exact `name`.", atlas.TraceDepthMax, atlas.TraceToolMax)
}

func (t *atlasTrace) Parameters() map[string]any {
	return entryParameters(map[string]any{
		"direction": map[string]any{
			"type": "string", "enum": []string{string(atlas.DirectionUp), string(atlas.DirectionDown)},
			"description": "`up` (default) for what depends on the entry, `down` for what it depends on.",
		},
		"depth": map[string]any{
			"type":        "integer",
			"description": fmt.Sprintf("How many hops, 1..%d (default %d).", atlas.TraceDepthMax, atlas.TraceDepthMax),
		},
		"all": map[string]any{
			"type": "boolean",
			"description": "Walk every relation (ownership, monitoring, the account a thing belongs to), " +
				"not only those a request travels through.",
		},
	})
}

func (t *atlasTrace) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// atlasTraceAnswer is atlas_trace's answer.
type atlasTraceAnswer struct {
	atlas.Trace
	atlasCoverage
}

func (t *atlasTrace) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	direction := atlas.Direction(strings.TrimSpace(argString(args, "direction")))
	if direction == "" {
		direction = atlas.DirectionUp
	}
	a, refusal := t.read(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	e, refusal := a.entryArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	trace, _, err := a.view.Trace(e.ID, direction, argInt(args, "depth", atlas.TraceDepthMax),
		argBool(args, "all"), atlas.TraceToolMax)
	if err != nil {
		return failed(fmt.Sprintf("%s: %s.", t.Name(), strings.TrimPrefix(err.Error(), "atlas: bad read: "))), nil
	}
	reached := []atlas.Entry{e}
	for _, hop := range trace.Hops {
		for _, s := range hop.Entries {
			if found, ok := a.view.Find(s.ID); ok {
				reached = append(reached, found)
			}
		}
	}
	a.attribute(reached)
	return a.read.answer(ctx, t.Name(), atlasTraceAnswer{Trace: trace, atlasCoverage: a.said()},
		"Ask for fewer hops with `depth`.")
}

// atlasCandidates is the atlas tools, offered where this node runs a company.
func atlasCandidates(r atlasReader) []candidate {
	on := r.deps.Settings != nil
	return []candidate{{&atlasSearch{r}, on}, {&atlasGet{r}, on}, {&atlasTrace{r}, on}}
}

// atlasKinds is every kind of entry, as the tools' schemas name them.
func atlasKinds() []string {
	out := make([]string, len(atlas.Kinds))
	for i, k := range atlas.Kinds {
		out[i] = string(k)
	}
	return out
}
